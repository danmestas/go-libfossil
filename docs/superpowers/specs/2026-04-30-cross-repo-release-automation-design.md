# Cross-repo release automation — design spec

- **Date**: 2026-04-30
- **Status**: Draft, pending implementation
- **Scope**: libfossil, edgesync, bones — three Go modules with a dependency chain
- **Author**: Dan Mestas

## Background

Three Go modules form a dependency chain:

```
libfossil ──> edgesync ──> bones
```

bones also imports libfossil directly (a diamond), but this design treats the chain as linear and accepts that bones receives libfossil updates only when edgesync re-releases. See "Decisions log" for rationale.

The most recent cascade (libfossil v0.4.5 → edgesync v0.0.7 → bones v0.3.3, all 2026-04-30) exposed the manual cost of the chain:

- libfossil tagged v0.4.5 with a sync bug fix.
- ~3 days passed before edgesync was bumped + tagged v0.0.7 (no notification mechanism).
- bones then required two separate PRs (#85, #86) to bump libfossil and edgesync — one because the dependency graph fans in, and one because there's no consolidation logic.
- Each repo's tag annotation was inconsistent: bones v0.3.3 had hand-written multi-line release notes; libfossil/edgesync had bare tags.
- A pre-existing rule in project memory ("run CI locally every time after you pr") was not catching CI-only failures because there's no enforcement and no single command that mirrors what GitHub Actions runs.

Pre-existing CI backlog (deferred 2026-04-26, see `project_pending_ci_improvements.md`): `paths-ignore` for docs PRs, `-race`, SDK-drift check, DST seed sweep. Not in scope here; tracked separately.

## Goals

1. Reduce upstream-tag → downstream-PR latency from days to minutes.
2. Eliminate the "two PRs to bones per cascade" failure mode.
3. Replace the discipline-based "run CI locally" rule with a fast, automatic pre-push check that catches obvious breakage.
4. Provide one command (`make ci`) per repo that runs the *exact* CI workflow, so contributors and the maintainer can verify before pushing.
5. Standardize release notes across all three repos (consistent quality, less manual annotation work).
6. Provide a `make release VERSION=...` helper that validates and tags from a template.

## Non-goals

- Auto-merging PRs. Maintainer reviews every PR per `~/.claude/CLAUDE.md` policy.
- Renovate/Dependabot. The repository_dispatch chain replaces it; Dependabot's polling adds latency without benefit for first-party deps.
- Monorepo migration.
- Cross-repo CI (running edgesync's tests on a libfossil PR). Out of scope; possible follow-up.
- The deferred CI backlog (paths-ignore, race, drift, DST sweep) — separate PR.

## Architecture overview

End-state of the cascade:

```
   libfossil tag (e.g. v0.4.6)
        │
        │ release.yml fires:
        │   - gh release create --generate-notes
        │   - gh api repos/danmestas/EdgeSync/dispatches  (event_type=upstream-bump)
        ▼
     edgesync
        │ bump-upstream.yml fires:
        │   - go get libfossil@vX.Y.Z && go mod tidy && go build ./...
        │   - opens PR "chore: bump libfossil → vX.Y.Z" on branch chore/bump-libfossil-vX.Y.Z
        │
        │ maintainer reviews, CI passes, merges
        │ maintainer runs: make release VERSION=v0.0.8
        │
        │ release.yml fires:
        │   - goreleaser publishes GitHub release with auto-changelog
        │   - gh api bones/dispatches
        ▼
       bones
        │ bump-upstream.yml fires:
        │   - go get EdgeSync@vA.B.C && go mod tidy
        │   - libfossil bump comes along automatically via Go MVS (edgesync's go.sum pins it)
        │   - opens single PR "chore: bump EdgeSync → vA.B.C"
        │
        │ maintainer reviews, merges, tags, ships goreleaser → Homebrew
```

Each repo additionally gains:

- `make ci` — verbatim mirror of that repo's GitHub Actions test workflow.
- `make ci-fast` — vet + build + short tests, sub-60s, used by pre-push hook.
- `.git/hooks/pre-push` (auto-installed via `make setup-hooks`) — runs `make ci-fast`, blocks push on failure.
- `make release VERSION=vX.Y.Z` — validates clean tree, runs `make ci`, opens `$EDITOR` with auto-populated tag annotation template, creates annotated tag locally. Push is intentionally manual.
- `.github/RELEASE_TEMPLATE.md` — boilerplate filled in by `make release`.
- GoReleaser changelog config (edgesync, bones) — auto-grouped release notes.

## Per-repo components

### libfossil (top of chain — currently has no release pipeline)

> **Deviation from PR #1/#2 (decided 2026-04-30, option A):** libfossil's existing `.githooks/pre-commit` is comprehensive (~45s — modernc + ncruces + vet + otel + build + SDK drift) and exceeds what `make ci-fast` would do. Therefore PR #3 does NOT add a `pre-push` hook or a `ci-fast` target. The existing pre-commit is libfossil's sole local gate; `make ci` is for manual full-CI verification before tagging.

**New files**:
- `.github/workflows/release.yml` (PR #6) — triggers on `push: tags: [v*]`. Two steps: `gh release create $TAG --generate-notes --verify-tag` and the dispatch step to edgesync.
- `Makefile` targets (PR #3, merged): `ci`, `ci-default`, `ci-ncruces`, `ci-otel-target`, `release`. (No `ci-fast` per the deviation above.)
- `.github/RELEASE_TEMPLATE.md` (PR #3, merged).

**Existing — unchanged**: `.github/workflows/test.yml`, `.githooks/pre-commit`, `setup-hooks` Makefile target, `test/test-drivers/test-otel` Makefile targets (the dev-friendly fast-path; intentionally distinct from the new `ci-*` targets which mirror CI verbatim).

**No goreleaser**: libfossil is a library — no binaries are published. `gh release create --generate-notes` provides auto-changelog and source tarballs without 50 lines of goreleaser config. Revisit if `cmd/libfossil` ever ships as a published binary.

**Multi-module note**: libfossil has 3 sub-modules with their own `go.mod` (`db/driver/modernc/`, `db/driver/ncruces/`, `observer/otel/`). The new `ci-default`/`ci-ncruces`/`ci-otel-target` targets handle this via explicit `cd <subdir> && go test ./...` steps. If a future `ci-fast` is ever added, it must do the same — `go test ./...` from repo root would silently skip the sub-modules.

**Secret needed**: `DISPATCH_PAT` — fine-grained PAT, repo access scoped to `danmestas/EdgeSync` only, permission `Contents: Read and write` (for PR #6).

### edgesync (middle — receives + dispatches)

> **Naming note**: the GitHub repo is `danmestas/EdgeSync` (mixed case, matches `go.mod` import path `github.com/danmestas/EdgeSync`). All `gh api repos/danmestas/EdgeSync/...` calls and `${{ github.event.repository.name }}` outputs use this canonical case. Local clone path `~/projects/edgesync` is lowercase but irrelevant to the workflow.

**New files**:
- `.github/workflows/bump-upstream.yml` — receiver listening for `repository_dispatch: types: [upstream-bump]` plus `workflow_dispatch` manual fallback.
- Same Makefile + hook + template files as libfossil.

**Modified files**:
- `.github/workflows/release.yml` — append a dispatch step targeting bones.
- `.goreleaser.yml` — add `changelog: { use: github, sort: asc, groups: [...] }` config.

**Secret needed**: `DISPATCH_PAT` — fine-grained PAT, repo access scoped to `danmestas/bones` only, `Contents: Read and write`.

**EXPECTED_REPO** in receiver workflow: `libfossil` (matches `${{ github.event.repository.name }}` from libfossil's release.yml). Receiver no-ops on any other dispatch.

### bones (leaf — receives only)

**New files**:
- `.github/workflows/bump-upstream.yml` — receiver only, no sender.
- Same Makefile + hook + template files.

**Modified files**:
- `.goreleaser.yml` — add changelog config.
- `.github/workflows/release.yml` — no changes (bones doesn't dispatch).

**Secret needed**: none beyond existing.

**EXPECTED_REPO** in receiver workflow: `EdgeSync`.

## Dispatch protocol

**Event name**: `upstream-bump` (single event, receiver discriminates on payload).

**Payload schema** (`client_payload`):

| Field | Example | Purpose |
|-------|---------|---------|
| `repo` | `libfossil` | Short name; receiver checks against `EXPECTED_REPO` env var |
| `module_path` | `github.com/danmestas/libfossil` | Full Go module path used by `go get` |
| `version` | `v0.4.6` | Tag string with leading `v`; validated against `^v\d+\.\d+\.\d+(-.+)?$` |
| `release_url` | `https://github.com/danmestas/libfossil/releases/tag/v0.4.6` | Embedded in PR body for one-click reviewer access |
| `tag_sha` | `2baaced...` | Audit trail in case of force-moved tags |
| `actor` | `danmestas` | Who pushed the tag |

**Validation**: receiver workflow fails loud (`::error::`) on:
- `repo` not matching `EXPECTED_REPO` → exits 0 (silent ignore for cross-routing safety).
- `module_path` not matching `EXPECTED_MODULE` → exits 1.
- `version` failing regex → exits 1.

**Sender step pattern** (in libfossil/edgesync `release.yml`):

```yaml
- name: Dispatch downstream bump
  env:
    GH_TOKEN: ${{ secrets.DISPATCH_PAT }}
    DOWNSTREAM: EdgeSync                            # in libfossil; "bones" in edgesync
    MODULE_PATH: github.com/danmestas/libfossil    # the upstream's own module path; in edgesync: github.com/danmestas/EdgeSync
  run: |
    gh api -X POST "repos/danmestas/${DOWNSTREAM}/dispatches" \
      -f event_type=upstream-bump \
      -f client_payload[repo]=${{ github.event.repository.name }} \
      -f client_payload[module_path]=${MODULE_PATH} \
      -f client_payload[version]=${{ github.ref_name }} \
      -f client_payload[release_url]=${{ github.server_url }}/${{ github.repository }}/releases/tag/${{ github.ref_name }} \
      -f client_payload[tag_sha]=${{ github.sha }} \
      -f client_payload[actor]=${{ github.actor }}
```

`DOWNSTREAM` and `MODULE_PATH` are hard-coded per repo:
- libfossil's release.yml: `DOWNSTREAM=EdgeSync`, `MODULE_PATH=github.com/danmestas/libfossil`
- edgesync's release.yml: `DOWNSTREAM=bones`, `MODULE_PATH=github.com/danmestas/EdgeSync`

**Idempotency**: Receiver checks for an existing open PR on branch `chore/bump-${repo}-${version}` before doing work. Re-firing the same dispatch is a no-op.

**Failure recovery**:

| Failure | Symptom | Recovery |
|---------|---------|----------|
| `DISPATCH_PAT` expired | Sender's dispatch step fails; tag + release still created | Rotate PAT, manually fire via `gh workflow run bump-upstream.yml` |
| Receiver workflow broken | No PR opens after upstream tag | Manually bump go.mod, fix workflow on a separate PR |
| `go get` fails (module proxy lag) | Receiver auto-retries 3× with 15s backoff; if still failing, workflow fails in "Bump module" step | Re-fire via `workflow_dispatch` after a few minutes (most cases self-recover) |
| Bad tag pushed (force-move) | `tag_sha` in PR body shows the old SHA | Reviewer notices in PR body before merging |

## Receiver workflow (`bump-upstream.yml`)

Same shape in edgesync and bones. ~75 lines including `workflow_dispatch` fallback.

```yaml
name: Bump upstream
on:
  repository_dispatch:
    types: [upstream-bump]
  workflow_dispatch:
    inputs:
      repo:        { required: true,  type: string }
      module_path: { required: true,  type: string }
      version:     { required: true,  type: string }
      release_url: { required: false, type: string }
      tag_sha:     { required: false, type: string }
      actor:       { required: false, type: string }

permissions:
  contents: write
  pull-requests: write

jobs:
  bump:
    runs-on: ubuntu-latest
    env:
      EXPECTED_REPO: libfossil          # bones uses "EdgeSync"
      EXPECTED_MODULE: github.com/danmestas/libfossil  # bones: github.com/danmestas/EdgeSync
    steps:
      - name: Resolve inputs
        id: in
        run: |
          REPO="${{ github.event.client_payload.repo || inputs.repo }}"
          MODULE_PATH="${{ github.event.client_payload.module_path || inputs.module_path }}"
          VERSION="${{ github.event.client_payload.version || inputs.version }}"
          RELEASE_URL="${{ github.event.client_payload.release_url || inputs.release_url }}"
          TAG_SHA="${{ github.event.client_payload.tag_sha || inputs.tag_sha }}"
          if [[ "$REPO" != "$EXPECTED_REPO" ]]; then
            echo "::notice::Ignoring dispatch for $REPO (expected $EXPECTED_REPO)"
            exit 0
          fi
          [[ "$MODULE_PATH" == "$EXPECTED_MODULE" ]] || { echo "::error::module_path mismatch: got $MODULE_PATH, expected $EXPECTED_MODULE"; exit 1; }
          [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$ ]] || { echo "::error::bad version: $VERSION"; exit 1; }
          {
            echo "repo=$REPO"
            echo "module=$MODULE_PATH"
            echo "version=$VERSION"
            echo "release_url=$RELEASE_URL"
            echo "tag_sha=$TAG_SHA"
            echo "branch=chore/bump-${REPO}-${VERSION}"
          } >> "$GITHUB_OUTPUT"

      - name: Idempotency check
        id: idem
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          if gh pr list --repo "${{ github.repository }}" --head "${{ steps.in.outputs.branch }}" --state open --json number -q '.[0].number' | grep -q .; then
            echo "exists=true" >> "$GITHUB_OUTPUT"
            echo "::notice::PR already open for ${{ steps.in.outputs.branch }}; skipping"
          fi

      - if: steps.idem.outputs.exists != 'true'
        uses: actions/checkout@v4
        with: { fetch-depth: 0 }

      - if: steps.idem.outputs.exists != 'true'
        uses: actions/setup-go@v5
        with: { go-version-file: go.mod }

      - if: steps.idem.outputs.exists != 'true'
        name: Bump module
        id: bump
        run: |
          git switch -c "${{ steps.in.outputs.branch }}"
          # GOPROXY can lag the upstream tag push; retry before giving up.
          for i in 1 2 3; do
            if go get "${{ steps.in.outputs.module }}@${{ steps.in.outputs.version }}"; then
              break
            fi
            [ "$i" -lt 3 ] && sleep 15 || { echo "::error::go get failed after 3 attempts"; exit 1; }
          done
          go mod tidy
          if git diff --quiet go.mod go.sum; then
            echo "::notice::No-op bump — version already pinned"
            echo "noop=true" >> "$GITHUB_OUTPUT"
          fi

      - if: steps.idem.outputs.exists != 'true' && steps.bump.outputs.noop != 'true'
        name: Verify build
        run: go build ./...

      - if: steps.idem.outputs.exists != 'true' && steps.bump.outputs.noop != 'true'
        name: Commit + push + open PR
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          git config user.name  "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git add go.mod go.sum
          git commit -m "chore: bump ${{ steps.in.outputs.repo }} → ${{ steps.in.outputs.version }}"
          git push -u origin "${{ steps.in.outputs.branch }}"
          gh pr create \
            --title "chore: bump ${{ steps.in.outputs.repo }} → ${{ steps.in.outputs.version }}" \
            --body "Auto-bump from upstream release.

          - Module: \`${{ steps.in.outputs.module }}\`
          - Version: \`${{ steps.in.outputs.version }}\`
          - Upstream release: ${{ steps.in.outputs.release_url }}
          - Tag SHA: \`${{ steps.in.outputs.tag_sha }}\`

          CI must pass before merge. Review go.sum diff for transitive surprises."
```

## Local dev surface

### `make ci` — per-repo verbatim mirrors

**libfossil** (mirrors `test.yml`):

```make
.PHONY: ci ci-default ci-ncruces ci-otel ci-fast

ci: ci-default ci-ncruces ci-otel

ci-default:
	GOWORK=off go test $(shell GOWORK=off go list ./... | grep -v '/dst') -count=1 -timeout=120s
	GOWORK=off go test ./dst/... -count=1 -timeout=300s
	cd db/driver/modernc && GOWORK=off go test ./... -count=1
	GOWORK=off go vet ./...
	GOWORK=off go build ./cmd/libfossil/

ci-ncruces:
	GOWORK=off go test -tags test_ncruces $(shell GOWORK=off go list ./... | grep -v '/dst' | grep -v 'cmd/libfossil') -count=1 -timeout=120s
	GOWORK=off go test -tags test_ncruces ./dst/... -count=1 -timeout=300s
	cd db/driver/ncruces && GOWORK=off go test ./... -count=1

ci-otel:
	cd observer/otel && GOWORK=off go test ./... -count=1
```

**edgesync** (mirrors `ci.yml` + `test.yml`): targets `ci-vet`, `ci-leaf-bridge`, `ci-cmd`, `ci-sim`. Exact commands extracted verbatim from each step's `run:` block.

**bones** (mirrors the bones-side portion of `ci.yml`; cross-repo leaf-binary integration is CI-only):

```make
ci:
	$(MAKE) check
	go build -tags=otel ./...
	go test -tags=otel -short ./... -count=1
```

### `make ci-fast` — pre-push subset

```make
ci-fast:
	go vet ./...
	go build ./...
	go test -short -count=1 -timeout=30s ./...
```

No `-race`, no `-tags=otel`, no `-tags test_ncruces`, no driver subdirs. ~30-60s. Catches build break, vet errors, obvious test failures. Misses: race conditions, otel-tagged code, ncruces-tagged code, DST. Those are `make ci` territory.

### `scripts/install-hooks.sh`

```sh
#!/usr/bin/env bash
set -euo pipefail
HOOK_DIR="$(git rev-parse --git-dir)/hooks"
mkdir -p "$HOOK_DIR"
cat > "$HOOK_DIR/pre-push" <<'EOF'
#!/usr/bin/env bash
set -e
echo "[pre-push] running make ci-fast..."
make ci-fast
EOF
chmod +x "$HOOK_DIR/pre-push"
echo "Installed pre-push hook → make ci-fast"
```

Wired into existing `make setup-hooks`. Bypass with `git push --no-verify` only when intentional.

### `make release VERSION=...`

```make
.PHONY: release
release:
	@test -n "$(VERSION)" || { echo "VERSION=vX.Y.Z required"; exit 1; }
	@echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$$' || { echo "bad version format"; exit 1; }
	@git diff --quiet || { echo "tree dirty; commit or stash first"; exit 1; }
	@git fetch origin --tags
	@if git rev-parse "$(VERSION)" >/dev/null 2>&1; then echo "tag $(VERSION) already exists"; exit 1; fi
	@$(MAKE) ci
	@PREV=$$(git describe --tags --abbrev=0 2>/dev/null || echo ""); \
	  TMPL=.github/RELEASE_TEMPLATE.md; \
	  TMP=$$(mktemp); \
	  { echo "Release $(VERSION)"; echo; \
	    [ -f $$TMPL ] && { cat $$TMPL; echo; }; \
	    echo "## Changes"; \
	    if [ -n "$$PREV" ]; then git log --oneline $$PREV..HEAD; else git log --oneline; fi; } > $$TMP; \
	  $${EDITOR:-vi} $$TMP; \
	  git tag -a "$(VERSION)" -F $$TMP; \
	  rm $$TMP
	@echo "Tag created locally. Run: git push origin $(VERSION)"
```

Two-phase intentional: `make release` creates tag locally only. Push is manual (`git push origin $VERSION`) so a fat-fingered version doesn't immediately fan out to remote + downstream.

### `.github/RELEASE_TEMPLATE.md`

```markdown
<!-- Edit before tagging. Auto-populated change list appended below. -->

## Highlights

-

## Breaking changes

None.

## Upgrade notes

-
```

### GoReleaser changelog (edgesync, bones)

```yaml
changelog:
  use: github
  sort: asc
  groups:
    - title: 'Features'
      regexp: '^.*?feat(\(.+\))??!?:.+$'
      order: 0
    - title: 'Bug fixes'
      regexp: '^.*?fix(\(.+\))??!?:.+$'
      order: 1
    - title: 'Other'
      order: 999
  filters:
    exclude:
      - '^docs:'
      - '^test:'
      - '^chore:'
      - '^ci:'
```

libfossil uses `gh release create --generate-notes` instead, which produces equivalent grouping from PR labels + commit prefixes.

## Rollout — 6 PRs, sequential

| # | Repo | Title | Adds | Risk |
|---|------|-------|------|------|
| 1 | bones | `chore: add make ci, ci-fast, release helper, pre-push hook` | Makefile targets, `scripts/install-hooks.sh`, `.github/RELEASE_TEMPLATE.md` | Low |
| 2 | edgesync | same as #1 | same | Low |
| 3 | libfossil | same as #1 (no goreleaser config exists yet to touch) | same | Low |
| 4 | bones | `feat(ci): add bump-upstream workflow + goreleaser changelog` | `bump-upstream.yml`, goreleaser changelog | Medium |
| 5 | edgesync | `feat(ci): add bump-upstream workflow, dispatch on tag, goreleaser changelog` | `bump-upstream.yml` + dispatch step in `release.yml` + goreleaser changelog | Medium-high |
| 6 | libfossil | `feat(ci): add release workflow with downstream dispatch` | new `release.yml` | Medium |

Order rationale:

- PRs 1-3 (local dev hygiene) are pure adds with no remote behavior change. Land them first to have `make ci` and pre-push hooks across all repos before touching dispatch wiring.
- PR 4 lets bones be tested as a receiver in isolation via manual `gh api` fire.
- PR 5 brings up both sides of the dispatch in one repo; tests both directions in one cycle (manual fire to receiver, real tag to test sender).
- PR 6 closes the chain. After this PR merges and a real libfossil release fires, the cascade is alive.

## PAT setup

One-time, before PR #4 merges:

1. **`DISPATCH_PAT` for libfossil** — fine-grained PAT, repo access: `danmestas/EdgeSync` only, permissions: `Contents: Read and write`. Stored in libfossil repo secrets.
2. **`DISPATCH_PAT` for edgesync** — fine-grained PAT, repo access: `danmestas/bones` only, same perms. Stored in `danmestas/EdgeSync` repo secrets.

1-year expiry; calendar reminder for rotation. Existing `HOMEBREW_PAT_TOKEN` is too broad to reuse.

## Testing strategy

### Per-PR local validation

PRs 1-3 (Makefile/hooks):
- `make ci` exits 0 on a clean main.
- `make ci-fast` runs in <60s.
- `make setup-hooks` installs `pre-push`; verify with `cat .git/hooks/pre-push` and `git push --dry-run`.
- `make release VERSION=v9.9.9-test` validates and tags locally; `git tag -d v9.9.9-test` to clean up.

### Receiver workflow validation (PR #4, then again in #5)

Manual fire after PR merge:

```bash
gh api -X POST repos/danmestas/bones/dispatches \
  -f event_type=upstream-bump \
  -f client_payload[repo]=EdgeSync \
  -f client_payload[module_path]=github.com/danmestas/EdgeSync \
  -f client_payload[version]=v0.0.7 \
  -f client_payload[release_url]=https://github.com/danmestas/EdgeSync/releases/tag/v0.0.7 \
  -f client_payload[tag_sha]=$(gh api repos/danmestas/EdgeSync/git/refs/tags/v0.0.7 -q .object.sha) \
  -f client_payload[actor]=danmestas
```

**Expected outcomes**:

| Scenario | Expected |
|----------|----------|
| First fire with already-pinned version | `noop=true`, no PR created |
| First fire with new version | PR opens on `chore/bump-EdgeSync-vX.Y.Z` |
| Re-fire same version | "PR already open" notice, no second PR |
| Bad version (`vbogus`) | Workflow fails with `::error::bad version` |
| Wrong repo (`repo=foobar`) | Workflow logs notice and exits 0 silently |
| `workflow_dispatch` with same inputs | Same behavior as `repository_dispatch` |

### End-to-end cascade validation (PR #5, PR #6)

After PR #5:
1. Tag edgesync with `v0.0.8-rc1`.
2. Watch `release.yml` fire goreleaser + dispatch step.
3. Watch bones receive dispatch and open PR `chore/bump-EdgeSync-v0.0.8-rc1`.
4. Close PR (don't merge); delete `v0.0.8-rc1` tag from origin and the prerelease GitHub release.

After PR #6:
1. Tag libfossil with `v0.4.6-rc1`.
2. Watch libfossil `release.yml` fire `gh release create` + dispatch.
3. Watch edgesync open PR.
4. Don't merge edgesync PR; close it; delete the rc tag and prerelease release.

`-rc1` suffix matters: changelog/release configs treat non-numeric suffixes as prereleases, so rc tags don't pollute the main release stream.

## Decisions log

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Scope | Bucket C: local dev hygiene + dispatch automation + release ergonomics | Closes the loop on consistency; deferred CI backlog handled separately |
| Dispatch mechanism | `repository_dispatch` (not Dependabot) | Tag-triggered, seconds latency, customizable PR body with upstream changelog link |
| Pre-push enforcement | Fast subset (vet+build+short tests, ~60s); full mirror behind `make ci` | Hard-block on 7-min hook would train `--no-verify` bypass; fast hook catches cheap mistakes |
| Dispatch graph | Linear (libfossil→edgesync→bones); libfossil does NOT dispatch to bones directly | Avoids fan-in to bones (the original "two PRs" pain); accepts that bones needs an edgesync release to pick up libfossil-only updates |
| Rollout | 6 PRs (3 hygiene + 3 dispatch) | Per CLAUDE.md, every PR is reviewed manually — smaller diffs are easier to review |
| libfossil release pipeline | `gh release create --generate-notes` (not goreleaser) | Library, no binaries; goreleaser would be 50 lines of YAGNI |
| Spec location | `libfossil/docs/superpowers/specs/` | Matches prior `2026-04-25-docs-site-cf-deploy-design.md`; libfossil is the source of the chain |
| Bot identity | `github-actions[bot]` (uid 41898282) | GitHub stock identity; complies with "never attribute Claude/AI" rule |

## Open questions / future work

- **Cross-repo CI**: should an edgesync PR run libfossil's tests against its current pin? Possible follow-up; out of scope here.
- **Reusable workflow**: `bump-upstream.yml` is ~95% identical between edgesync and bones. Could move to a fourth `.github` repo as a reusable workflow once the pattern stabilizes. Premature for a 3-repo cascade.
- **Auto-merge tier**: if cascade reliability is high after a few real cascades, consider an `automerge` label that lets bot-authored chore-bump PRs merge on green CI. Requires branch protection + status checks. Not in this design.
- **Deferred CI improvements**: paths-ignore for docs PRs, `-race` in libfossil, SDK-drift check in libfossil, DST seed sweep. Tracked in `project_pending_ci_improvements.md`. Could fold into PRs 1-3 but bloats their scope; recommend keeping separate.
- **Diamond dep edge case**: linear-only means bones lags libfossil if edgesync doesn't release. If a libfossil-only fix becomes urgent for bones, manual override is `gh workflow run bump-upstream.yml -f repo=libfossil ...` after temporarily relaxing the `EXPECTED_REPO` check, or a one-off manual go.mod bump. Acceptable today; revisit if it happens more than annually.
