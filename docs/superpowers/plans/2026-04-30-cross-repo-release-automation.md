# Cross-repo release automation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automate the libfossil → edgesync → bones release cascade. After all 6 PRs ship: tagging libfossil triggers an auto-bump PR in edgesync; merging + tagging edgesync triggers the same in bones. Each repo also gets a verbatim CI mirror (`make ci`), a fast pre-push hook (`make ci-fast`), and a `make release VERSION=...` helper.

**Architecture:** GitHub `repository_dispatch` chain with linear graph (libfossil dispatches only to edgesync; edgesync dispatches only to bones). Receiver workflow (`bump-upstream.yml`) is shared shape across edgesync and bones with a single `EXPECTED_REPO` env var distinguishing them. Local dev surface is per-repo Makefile targets and `.githooks/pre-push` (using existing `git config core.hooksPath .githooks` pattern).

**Tech Stack:** GitHub Actions, GoReleaser v2 (edgesync, bones only), `gh` CLI, GNU make, bash, Go 1.26.

**Spec:** [`docs/superpowers/specs/2026-04-30-cross-repo-release-automation-design.md`](../specs/2026-04-30-cross-repo-release-automation-design.md)

---

## File structure

### libfossil (`/Users/dmestas/projects/libfossil`)
- **Create:** `.github/workflows/release.yml` (PR #6)
- **Create:** `.github/RELEASE_TEMPLATE.md` (PR #3)
- **Create:** `.githooks/pre-push` (PR #3)
- **Modify:** `Makefile` — add `ci`, `ci-default`, `ci-ncruces`, `ci-otel-target`, `ci-fast`, `release` targets; existing `setup-hooks` already sets `core.hooksPath` (no change needed) (PR #3)

### edgesync (`/Users/dmestas/projects/edgesync`, GitHub: `danmestas/EdgeSync`)
- **Create:** `.github/workflows/bump-upstream.yml` (PR #5)
- **Create:** `.github/RELEASE_TEMPLATE.md` (PR #2)
- **Create:** `.githooks/pre-push` (PR #2)
- **Modify:** `Makefile` — add `ci`, `ci-fast`, `release` targets (PR #2)
- **Modify:** `.github/workflows/release.yml` — append dispatch step (PR #5)
- **Modify:** `.goreleaser.yml` — add `groups:` to existing `changelog:` block (PR #5)

### bones (`/Users/dmestas/projects/bones`)
- **Create:** `.github/workflows/bump-upstream.yml` (PR #4)
- **Create:** `.github/RELEASE_TEMPLATE.md` (PR #1)
- **Create:** `.githooks/pre-push` (PR #1)
- **Modify:** `Makefile` — add `ci`, `ci-fast`, `release`, `setup-hooks` targets (PR #1)
- **Modify:** `.goreleaser.yml` — add `groups:` to existing `changelog:` block (PR #4)

---

## Phase 0: Prerequisites (one-time, before PR #4 merges)

These steps are manual and produce no commits. Complete them between PR #3 merge and PR #4 PR-open so dispatch tokens are in place when needed.

- [ ] **Step 0.1: Generate `DISPATCH_PAT` for libfossil → EdgeSync**

Open https://github.com/settings/personal-access-tokens/new in a browser. Configure:
- Token name: `libfossil-dispatch-to-edgesync`
- Expiration: 1 year
- Resource owner: `danmestas`
- Repository access: "Only select repositories" → select `danmestas/EdgeSync`
- Repository permissions: `Contents: Read and write`, `Metadata: Read-only` (Metadata is auto-required)

Generate token, copy it, then:

```bash
cd /Users/dmestas/projects/libfossil
gh secret set DISPATCH_PAT --body "<paste-token-here>"
```

Verify:
```bash
gh secret list | grep DISPATCH_PAT
```
Expected: `DISPATCH_PAT  Updated <date>`

- [ ] **Step 0.2: Generate `DISPATCH_PAT` for edgesync → bones**

Same flow, configure:
- Token name: `edgesync-dispatch-to-bones`
- Resource owner: `danmestas`
- Repository access: `danmestas/bones` only
- Permissions: `Contents: Read and write`, `Metadata: Read-only`

```bash
cd /Users/dmestas/projects/edgesync
gh secret set DISPATCH_PAT --body "<paste-token-here>"
```

Verify:
```bash
gh secret list | grep DISPATCH_PAT
```
Expected: `DISPATCH_PAT  Updated <date>`

- [ ] **Step 0.3: Note rotation reminder**

Add a calendar reminder for ~10 months from today (2027-02-28) to rotate both PATs before the 1-year expiry hits.

---

## Phase 1: PR #1 — bones local dev hygiene

**Repo:** `/Users/dmestas/projects/bones`
**Branch:** `chore/local-dev-hygiene`
**Files added:** `.githooks/pre-push`, `.github/RELEASE_TEMPLATE.md`
**Files modified:** `Makefile`

### Task 1.0: Branch setup

- [ ] **Step 1.0.1: Create feature branch**

```bash
cd /Users/dmestas/projects/bones
git fetch origin
git checkout main
git pull --ff-only origin main
git checkout -b chore/local-dev-hygiene
```

Expected: switched to a new branch.

### Task 1.1: Add `make ci` target

bones' CI workflow (`.github/workflows/ci.yml`) runs the discipline suite via `make check` (fmt-check + vet + lint + race + todo-check), then otel-tagged build, then otel-tagged short tests. The cross-repo leaf-binary integration step (sibling EdgeSync checkout + `make leaf`) is **CI-only** and intentionally not mirrored locally — it requires a sibling working directory layout that doesn't generalize.

**Files:** Modify `Makefile`

- [ ] **Step 1.1.1: Append `ci` target to Makefile**

Append to the end of `/Users/dmestas/projects/bones/Makefile`:

```makefile

# CI mirror — runs the bones-side portion of .github/workflows/ci.yml.
# Cross-repo leaf-binary integration (requires sibling EdgeSync checkout)
# is CI-only and not mirrored here.
.PHONY: ci ci-fast
ci:
	$(MAKE) check
	go build -tags=otel ./...
	go test -tags=otel -short ./... -count=1

# Fast subset for pre-push hook (~30-60s; no make check, no -tags=otel, no -race).
ci-fast:
	go vet ./...
	go build ./...
	go test -short -count=1 -timeout=30s ./...
```

Also add `ci ci-fast` to the `.PHONY` line at the top if there's a single combined `.PHONY:` declaration. If multiple `.PHONY:` lines exist (as currently), the new line above is sufficient.

- [ ] **Step 1.1.2: Run `make ci` to verify it passes**

```bash
make ci
```
Expected: exits 0. (May take 1-3 min.)

- [ ] **Step 1.1.3: Run `make ci-fast` to verify it passes and is fast**

```bash
time make ci-fast
```
Expected: exits 0 in under 60s.

- [ ] **Step 1.1.4: Commit**

```bash
git add Makefile
git commit -m "feat(make): add ci and ci-fast targets

ci mirrors .github/workflows/ci.yml verbatim (-tags=otel -short).
ci-fast runs vet + build + short tests (~60s) for pre-push hook."
```

### Task 1.2: Add `make release VERSION=...` target

**Files:** Modify `Makefile`

- [ ] **Step 1.2.1: Append release target to Makefile**

Append to `/Users/dmestas/projects/bones/Makefile`:

```makefile

.PHONY: release
release:
	@test -n "$(VERSION)" || { echo "VERSION=vX.Y.Z required"; exit 1; }
	@echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$$' || { echo "bad version format: $(VERSION)"; exit 1; }
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
	@echo ""
	@echo "Tag $(VERSION) created locally. To publish:"
	@echo "  git push origin $(VERSION)"
```

- [ ] **Step 1.2.2: Verify the target rejects bad input**

```bash
make release          # missing VERSION
```
Expected: exits 1 with "VERSION=vX.Y.Z required".

```bash
make release VERSION=foo
```
Expected: exits 1 with "bad version format: foo".

- [ ] **Step 1.2.3: Verify the target works on a test version (without committing the tag)**

```bash
make release VERSION=v9.9.9-test EDITOR=true
```
(Sets `EDITOR=true` so the no-op editor command exits cleanly without prompting.)
Expected: tag created locally. Verify:

```bash
git tag -l 'v9.9.9*'
git tag -d v9.9.9-test
```
Expected: tag listed, then deleted.

- [ ] **Step 1.2.4: Commit**

```bash
git add Makefile
git commit -m "feat(make): add release VERSION=... helper

Validates clean tree, runs make ci, opens \$EDITOR with auto-populated
tag annotation template (.github/RELEASE_TEMPLATE.md + git log since
prev tag), creates annotated tag locally. Push is intentionally manual."
```

### Task 1.3: Add `setup-hooks` target

bones doesn't have one yet. Use the same `core.hooksPath .githooks` pattern that libfossil and edgesync use.

**Files:** Modify `Makefile`

- [ ] **Step 1.3.1: Append setup-hooks target**

Append to `/Users/dmestas/projects/bones/Makefile`:

```makefile

.PHONY: setup-hooks
setup-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks configured to use .githooks/ directory."
	@echo "Pre-push runs make ci-fast (~60s). Skip with: git push --no-verify"
```

- [ ] **Step 1.3.2: Verify (without running it yet — pre-push file doesn't exist)**

```bash
grep -A 3 'setup-hooks' Makefile
```
Expected: shows the new target.

- [ ] **Step 1.3.3: Commit**

```bash
git add Makefile
git commit -m "feat(make): add setup-hooks target

Configures git core.hooksPath to .githooks/ directory.
Matches the pattern already used in libfossil and edgesync."
```

### Task 1.4: Create `.githooks/pre-push`

**Files:** Create `.githooks/pre-push`

- [ ] **Step 1.4.1: Create the directory and file**

```bash
mkdir -p /Users/dmestas/projects/bones/.githooks
```

Create `/Users/dmestas/projects/bones/.githooks/pre-push` with content:

```bash
#!/usr/bin/env bash
# Pre-push hook — runs `make ci-fast` (~60s) to catch obvious breakage
# before pushing. Mirrors a fast subset of GitHub CI.
#
# Skip with: git push --no-verify

set -e
echo "[pre-push] running make ci-fast..."
cd "$(git rev-parse --show-toplevel)"
make ci-fast
```

- [ ] **Step 1.4.2: Make executable**

```bash
chmod +x /Users/dmestas/projects/bones/.githooks/pre-push
```

- [ ] **Step 1.4.3: Test the hook directly**

```bash
cd /Users/dmestas/projects/bones
./.githooks/pre-push
```
Expected: prints `[pre-push] running make ci-fast...`, runs ci-fast, exits 0.

- [ ] **Step 1.4.4: Wire it up by running setup-hooks**

```bash
make setup-hooks
git config core.hooksPath
```
Expected: prints `.githooks`.

- [ ] **Step 1.4.5: Verify hook fires on push (dry run)**

Make a trivial change so push has work to do:
```bash
echo "" >> .githooks/pre-push   # add trailing newline
git add .githooks/pre-push
git commit --amend --no-edit
git push --dry-run -u origin chore/local-dev-hygiene 2>&1 | head -5
```
Expected: see `[pre-push] running make ci-fast...` in output.

- [ ] **Step 1.4.6: Commit the hook**

```bash
git add .githooks/pre-push
git commit -m "feat: add pre-push hook running make ci-fast

Catches obvious breakage (vet errors, build break, short test failures)
before push. ~60s. Skip with: git push --no-verify"
```

### Task 1.5: Create `.github/RELEASE_TEMPLATE.md`

**Files:** Create `.github/RELEASE_TEMPLATE.md`

- [ ] **Step 1.5.1: Create the file**

Write to `/Users/dmestas/projects/bones/.github/RELEASE_TEMPLATE.md`:

```markdown
<!-- Edit before tagging. The `make release` target appends `git log --oneline` since the previous tag below this template, then opens $EDITOR. Save and quit to create the annotated tag. -->

## Highlights

-

## Breaking changes

None.

## Upgrade notes

-
```

- [ ] **Step 1.5.2: Verify `make release` picks up the template**

```bash
make release VERSION=v9.9.9-test EDITOR=cat
```
Expected: prints "Release v9.9.9-test", then the template, then "## Changes" and a commit list.

Cleanup:
```bash
git tag -d v9.9.9-test
```

- [ ] **Step 1.5.3: Commit**

```bash
git add .github/RELEASE_TEMPLATE.md
git commit -m "feat: add release annotation template

Used by 'make release VERSION=...' as the body skeleton for annotated tags."
```

### Task 1.6: Open PR

- [ ] **Step 1.6.1: Push branch**

```bash
cd /Users/dmestas/projects/bones
git push -u origin chore/local-dev-hygiene
```

- [ ] **Step 1.6.2: Open PR**

```bash
gh pr create --title "chore: add local dev hygiene targets and pre-push hook" --body "$(cat <<'EOF'
## Summary
- Add `make ci` (verbatim mirror of `.github/workflows/ci.yml`)
- Add `make ci-fast` (vet+build+short tests, ~60s for pre-push)
- Add `make release VERSION=...` helper with annotated tag template
- Add `make setup-hooks` (matches libfossil/edgesync pattern)
- Add `.githooks/pre-push` running `make ci-fast`
- Add `.github/RELEASE_TEMPLATE.md` boilerplate

No remote behavior change. Lays groundwork for the upstream-bump
workflow in PR #4 and for consistent release ergonomics.

## Test plan
- [x] `make ci` exits 0 on a clean main
- [x] `make ci-fast` runs in <60s
- [x] `make release VERSION=v9.9.9-test EDITOR=true` validates and creates a local tag, then deleted
- [x] `make setup-hooks` configures `core.hooksPath` to `.githooks/`
- [x] `git push --dry-run` triggers the pre-push hook
- [x] `make release` rejects bad version formats and missing VERSION

Spec: see libfossil docs/superpowers/specs/2026-04-30-cross-repo-release-automation-design.md
EOF
)"
```
Expected: PR URL printed.

- [ ] **Step 1.6.3: Wait for CI to pass on the PR, then run local CI again**

```bash
gh pr checks --watch
```
Wait for green. Then on local main:
```bash
git checkout main
make ci
```
Expected: exits 0 (still passes locally — sanity check before merge).

- [ ] **Step 1.6.4: User reviews and merges**

User performs the merge manually per CLAUDE.md ("wait for me to merge"). After merge:

```bash
git checkout main
git pull --ff-only origin main
git branch -d chore/local-dev-hygiene
git push origin --delete chore/local-dev-hygiene
```

---

## Phase 2: PR #2 — edgesync local dev hygiene

**Repo:** `/Users/dmestas/projects/edgesync`
**Branch:** `chore/local-dev-hygiene`
**Files added:** `.githooks/pre-push`, `.github/RELEASE_TEMPLATE.md`
**Files modified:** `Makefile`

### Task 2.0: Branch setup

- [ ] **Step 2.0.1: Create feature branch**

```bash
cd /Users/dmestas/projects/edgesync
git fetch origin
git checkout main
git pull --ff-only origin main
git checkout -b chore/local-dev-hygiene
```

### Task 2.1: Inspect edgesync's CI workflows for the verbatim ci command

edgesync has both `ci.yml` and `test.yml`. Read them to extract the exact `run:` blocks:

- [ ] **Step 2.1.1: Read CI workflow**

```bash
cat /Users/dmestas/projects/edgesync/.github/workflows/ci.yml
cat /Users/dmestas/projects/edgesync/.github/workflows/test.yml
```

Note the exact commands. Per Explore snapshot, ci.yml runs:
- `go vet ./...`
- `cd leaf && go vet ./...`
- `cd ../bridge && go vet ./...`
- `cd leaf && go test ./... -short -count=1 &`
- `cd bridge && go test ./... -short -count=1 &`
- `go build -buildvcs=false ./cmd/edgesync/`
- `go test -buildvcs=false ./cmd/edgesync/ -count=1 -timeout=60s`

And test.yml adds the sim/ tests. The make ci target should mirror **ci.yml only** (sim/ DST is its own thing run separately by `test.yml`; if a `test-sim` target makes sense, add it as well).

If actual workflow content differs from the Explore snapshot, use the actual workflow content as the source of truth.

### Task 2.2: Add `ci` and `ci-fast` targets

**Files:** Modify `Makefile`

- [ ] **Step 2.2.1: Append ci targets to Makefile**

Append to `/Users/dmestas/projects/edgesync/Makefile` (preserving existing content):

```makefile

# CI mirror — must match .github/workflows/ci.yml verbatim.
.PHONY: ci ci-vet ci-leaf-bridge ci-cmd ci-fast

ci: ci-vet ci-leaf-bridge ci-cmd

ci-vet:
	go vet ./...
	cd leaf && go vet ./...
	cd bridge && go vet ./...

ci-leaf-bridge:
	cd leaf && go test ./... -short -count=1
	cd bridge && go test ./... -short -count=1

ci-cmd:
	go build -buildvcs=false ./cmd/edgesync/
	go test -buildvcs=false ./cmd/edgesync/ -count=1 -timeout=60s

# Fast subset for pre-push hook.
ci-fast:
	go vet ./...
	go build ./...
	go test -short -count=1 -timeout=30s ./...
```

Note: `ci-leaf-bridge` runs leaf and bridge tests sequentially; ci.yml runs them in background (`&`) which is a parallelism trick that doesn't translate cleanly to make. Sequential is acceptable for local mirror.

- [ ] **Step 2.2.2: Run `make ci`**

```bash
make ci
```
Expected: exits 0.

- [ ] **Step 2.2.3: Run `make ci-fast`**

```bash
time make ci-fast
```
Expected: exits 0 in <60s.

- [ ] **Step 2.2.4: Commit**

```bash
git add Makefile
git commit -m "feat(make): add ci and ci-fast targets

ci mirrors .github/workflows/ci.yml verbatim (vet + leaf/bridge tests + cmd build/test).
ci-fast runs vet + build + short tests (~60s) for pre-push hook."
```

### Task 2.3: Add `make release VERSION=...` target

**Files:** Modify `Makefile`

- [ ] **Step 2.3.1: Append release target**

Append to `/Users/dmestas/projects/edgesync/Makefile` (use the same recipe as bones Step 1.2.1 — repeating verbatim):

```makefile

.PHONY: release
release:
	@test -n "$(VERSION)" || { echo "VERSION=vX.Y.Z required"; exit 1; }
	@echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$$' || { echo "bad version format: $(VERSION)"; exit 1; }
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
	@echo ""
	@echo "Tag $(VERSION) created locally. To publish:"
	@echo "  git push origin $(VERSION)"
```

- [ ] **Step 2.3.2: Verify rejection paths**

```bash
make release          # missing VERSION
make release VERSION=foo
```
Expected: each exits 1 with the appropriate error.

- [ ] **Step 2.3.3: Verify success path on test version**

```bash
make release VERSION=v9.9.9-test EDITOR=true
git tag -d v9.9.9-test
```
Expected: tag created, then deleted.

- [ ] **Step 2.3.4: Commit**

```bash
git add Makefile
git commit -m "feat(make): add release VERSION=... helper

Validates clean tree, runs make ci, opens \$EDITOR with auto-populated
tag annotation template, creates annotated tag locally. Push is intentionally manual."
```

### Task 2.4: Create `.githooks/pre-push`

edgesync already has `.githooks/` configured (existing `setup-hooks` target sets `core.hooksPath`). Just add the pre-push file.

**Files:** Create `.githooks/pre-push`

- [ ] **Step 2.4.1: Verify .githooks dir exists**

```bash
ls /Users/dmestas/projects/edgesync/.githooks/
```
Expected: lists existing hooks (likely `pre-commit`). If dir doesn't exist, `mkdir -p` first.

- [ ] **Step 2.4.2: Create pre-push file**

Write to `/Users/dmestas/projects/edgesync/.githooks/pre-push`:

```bash
#!/usr/bin/env bash
# Pre-push hook — runs `make ci-fast` (~60s) to catch obvious breakage
# before pushing. Mirrors a fast subset of GitHub CI.
#
# Skip with: git push --no-verify

set -e
echo "[pre-push] running make ci-fast..."
cd "$(git rev-parse --show-toplevel)"
make ci-fast
```

- [ ] **Step 2.4.3: Make executable**

```bash
chmod +x /Users/dmestas/projects/edgesync/.githooks/pre-push
```

- [ ] **Step 2.4.4: Test the hook directly**

```bash
cd /Users/dmestas/projects/edgesync
./.githooks/pre-push
```
Expected: prints message, runs ci-fast, exits 0.

- [ ] **Step 2.4.5: Verify core.hooksPath is set (existing setup-hooks)**

```bash
git config core.hooksPath
```
Expected: prints `.githooks`. If not, run `make setup-hooks`.

- [ ] **Step 2.4.6: Commit**

```bash
git add .githooks/pre-push
git commit -m "feat: add pre-push hook running make ci-fast

Catches obvious breakage before push. ~60s. Skip with: git push --no-verify"
```

### Task 2.5: Create `.github/RELEASE_TEMPLATE.md`

- [ ] **Step 2.5.1: Create file**

Write to `/Users/dmestas/projects/edgesync/.github/RELEASE_TEMPLATE.md` (same content as bones — repeating verbatim):

```markdown
<!-- Edit before tagging. The `make release` target appends `git log --oneline` since the previous tag below this template, then opens $EDITOR. Save and quit to create the annotated tag. -->

## Highlights

-

## Breaking changes

None.

## Upgrade notes

-
```

- [ ] **Step 2.5.2: Commit**

```bash
git add .github/RELEASE_TEMPLATE.md
git commit -m "feat: add release annotation template

Used by 'make release VERSION=...' as the body skeleton for annotated tags."
```

### Task 2.6: Open PR

- [ ] **Step 2.6.1: Push and PR**

```bash
cd /Users/dmestas/projects/edgesync
git push -u origin chore/local-dev-hygiene
gh pr create --title "chore: add local dev hygiene targets and pre-push hook" --body "$(cat <<'EOF'
## Summary
- Add `make ci` (mirrors `.github/workflows/ci.yml` verbatim)
- Add `make ci-fast` (vet+build+short tests, ~60s for pre-push)
- Add `make release VERSION=...` helper
- Add `.githooks/pre-push` (existing core.hooksPath already configured)
- Add `.github/RELEASE_TEMPLATE.md`

No remote behavior change. Sets up local dev surface to match libfossil
and bones; lays groundwork for upstream-bump workflow in PR #5.

## Test plan
- [x] `make ci` exits 0
- [x] `make ci-fast` runs in <60s
- [x] `make release VERSION=v9.9.9-test EDITOR=true` works and is reverted
- [x] `git push --dry-run` triggers the pre-push hook
- [x] `make release` rejects bad input

Spec: see libfossil docs/superpowers/specs/2026-04-30-cross-repo-release-automation-design.md
EOF
)"
```

- [ ] **Step 2.6.2: Wait for CI green, user merges, cleanup**

```bash
gh pr checks --watch
```

After user merges:
```bash
git checkout main && git pull --ff-only origin main
git branch -d chore/local-dev-hygiene
git push origin --delete chore/local-dev-hygiene
```

---

## Phase 3: PR #3 — libfossil local dev hygiene

**Repo:** `/Users/dmestas/projects/libfossil`
**Branch:** `chore/local-dev-hygiene`
**Files added:** `.github/RELEASE_TEMPLATE.md`
**Files modified:** `Makefile`

> **Deliberate deviation from PR #1/#2 (decided 2026-04-30, option A):** libfossil's existing `.githooks/pre-commit` is comprehensive (~45s — modernc + ncruces + vet + otel + build + SDK drift) and already exceeds what `make ci-fast` would do. So PR #3 does **not** add a `pre-push` hook and does **not** add a `ci-fast` Makefile target. libfossil uses its existing pre-commit as the sole local gate; `make ci` is for manual full-CI verification before tagging. **Tasks 3.3 (pre-push hook) and the `ci-fast` recipe in Task 3.1 below are obsolete and were not implemented.** PR #19 (merged 2026-04-30) shipped only `ci`/`ci-default`/`ci-ncruces`/`ci-otel-target` + `release` + RELEASE_TEMPLATE.

> **Multi-module note for any future libfossil pre-push:** libfossil has 3 sub-modules with their own `go.mod` (`db/driver/modernc/`, `db/driver/ncruces/`, `observer/otel/`). `go test ./...` from repo root does not descend into them. Any future `make ci-fast` would need explicit `cd <subdir> && go test -short ./...` per sub-module — same gap edgesync hit and fixed.

libfossil has the most complex `make ci` because its CI runs three jobs: default driver (modernc), ncruces driver, and otel.

### Task 3.0: Branch setup

- [ ] **Step 3.0.1: Create feature branch**

```bash
cd /Users/dmestas/projects/libfossil
git fetch origin
git checkout main
git pull --ff-only origin main
git checkout -b chore/local-dev-hygiene
```

### Task 3.1: Add `make ci` target (verbatim mirror of test.yml)

libfossil's `test.yml` has two driver jobs (modernc default, ncruces) plus an otel job. The existing `test`, `test-drivers`, `test-otel` targets in the Makefile do **not** use `GOWORK=off`; the CI workflow does. So we add a new `ci` target rather than aliasing.

**Files:** Modify `Makefile`

- [ ] **Step 3.1.1: Append ci targets to Makefile**

Append to `/Users/dmestas/projects/libfossil/Makefile`:

```makefile

# CI mirror — must match .github/workflows/test.yml verbatim.
# Note: GOWORK=off is critical; the existing test/test-drivers/test-otel
# targets do NOT use it but CI does, leading to historical drift.
.PHONY: ci ci-default ci-ncruces ci-otel-target ci-fast

ci: ci-default ci-ncruces ci-otel-target

ci-default:
	GOWORK=off go test $$(GOWORK=off go list ./... | grep -v '/dst') -count=1 -timeout=120s
	GOWORK=off go test ./dst/... -count=1 -timeout=300s
	cd db/driver/modernc && GOWORK=off go test ./... -count=1
	GOWORK=off go vet ./...
	GOWORK=off go build ./cmd/libfossil/

ci-ncruces:
	GOWORK=off go test -tags test_ncruces $$(GOWORK=off go list ./... | grep -v '/dst' | grep -v 'cmd/libfossil') -count=1 -timeout=120s
	GOWORK=off go test -tags test_ncruces ./dst/... -count=1 -timeout=300s
	cd db/driver/ncruces && GOWORK=off go test ./... -count=1

ci-otel-target:
	cd observer/otel && GOWORK=off go test ./... -count=1

# Fast subset for pre-push hook (~30-60s).
ci-fast:
	GOWORK=off go vet ./...
	GOWORK=off go build ./cmd/libfossil/
	GOWORK=off go test -short -count=1 -timeout=30s $$(GOWORK=off go list ./... | grep -v '/dst')
```

Note: target name is `ci-otel-target` not `ci-otel` to avoid potential collision with the existing `test-otel` shape if the Makefile evolves.

- [ ] **Step 3.1.2: Run `make ci-default`**

```bash
make ci-default
```
Expected: exits 0.

- [ ] **Step 3.1.3: Run `make ci-ncruces`**

```bash
make ci-ncruces
```
Expected: exits 0.

- [ ] **Step 3.1.4: Run `make ci-otel-target`**

```bash
make ci-otel-target
```
Expected: exits 0.

- [ ] **Step 3.1.5: Run `make ci-fast`**

```bash
time make ci-fast
```
Expected: exits 0 in <60s.

- [ ] **Step 3.1.6: Commit**

```bash
git add Makefile
git commit -m "feat(make): add ci and ci-fast targets

ci mirrors .github/workflows/test.yml verbatim across all three jobs:
  ci-default     — modernc driver tests + DST + vet + cmd build
  ci-ncruces     — ncruces driver tests with -tags test_ncruces
  ci-otel-target — observer/otel tests

All use GOWORK=off (matching CI). Existing test/test-drivers/test-otel
targets are left alone — they don't use GOWORK=off and have drifted
from CI; ci is the single source of truth going forward.

ci-fast runs vet + build + short tests (~60s) for pre-push hook."
```

### Task 3.2: Add `make release VERSION=...` target

**Files:** Modify `Makefile`

- [ ] **Step 3.2.1: Append release target (same recipe as bones/edgesync — repeated verbatim)**

Append to `/Users/dmestas/projects/libfossil/Makefile`:

```makefile

.PHONY: release
release:
	@test -n "$(VERSION)" || { echo "VERSION=vX.Y.Z required"; exit 1; }
	@echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$$' || { echo "bad version format: $(VERSION)"; exit 1; }
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
	@echo ""
	@echo "Tag $(VERSION) created locally. To publish:"
	@echo "  git push origin $(VERSION)"
```

- [ ] **Step 3.2.2: Verify rejection and success paths**

```bash
make release
make release VERSION=foo
make release VERSION=v9.9.9-test EDITOR=true
git tag -d v9.9.9-test
```
Expected: first two exit 1; third creates tag (after running ci which is slow — be patient or test on a smaller scope by setting EDITOR=true to skip prompt).

- [ ] **Step 3.2.3: Commit**

```bash
git add Makefile
git commit -m "feat(make): add release VERSION=... helper

Validates clean tree, runs make ci, opens \$EDITOR with auto-populated
tag annotation template, creates annotated tag locally. Push is intentionally manual."
```

### Task 3.3: Create `.githooks/pre-push`

libfossil already has `.githooks/` (existing `setup-hooks` target sets `core.hooksPath`).

- [ ] **Step 3.3.1: Verify .githooks/ exists**

```bash
ls /Users/dmestas/projects/libfossil/.githooks/
```
Expected: lists existing hooks (likely `pre-commit`).

- [ ] **Step 3.3.2: Create pre-push**

Write to `/Users/dmestas/projects/libfossil/.githooks/pre-push`:

```bash
#!/usr/bin/env bash
# Pre-push hook — runs `make ci-fast` (~60s) to catch obvious breakage
# before pushing. Mirrors a fast subset of GitHub CI.
#
# Skip with: git push --no-verify

set -e
echo "[pre-push] running make ci-fast..."
cd "$(git rev-parse --show-toplevel)"
make ci-fast
```

- [ ] **Step 3.3.3: Make executable + test directly**

```bash
chmod +x /Users/dmestas/projects/libfossil/.githooks/pre-push
./.githooks/pre-push
```
Expected: exits 0.

- [ ] **Step 3.3.4: Verify core.hooksPath set**

```bash
git config core.hooksPath
```
Expected: `.githooks`. If empty, run `make setup-hooks`.

- [ ] **Step 3.3.5: Commit**

```bash
git add .githooks/pre-push
git commit -m "feat: add pre-push hook running make ci-fast

Catches obvious breakage (vet errors, build break, short test failures)
before push. ~60s. Skip with: git push --no-verify"
```

### Task 3.4: Create `.github/RELEASE_TEMPLATE.md`

- [ ] **Step 3.4.1: Create file (same content as other repos)**

Write to `/Users/dmestas/projects/libfossil/.github/RELEASE_TEMPLATE.md`:

```markdown
<!-- Edit before tagging. The `make release` target appends `git log --oneline` since the previous tag below this template, then opens $EDITOR. Save and quit to create the annotated tag. -->

## Highlights

-

## Breaking changes

None.

## Upgrade notes

-
```

- [ ] **Step 3.4.2: Commit**

```bash
git add .github/RELEASE_TEMPLATE.md
git commit -m "feat: add release annotation template

Used by 'make release VERSION=...' as the body skeleton for annotated tags."
```

### Task 3.5: Open PR

- [ ] **Step 3.5.1: Push and PR**

```bash
cd /Users/dmestas/projects/libfossil
git push -u origin chore/local-dev-hygiene
gh pr create --title "chore: add local dev hygiene targets and pre-push hook" --body "$(cat <<'EOF'
## Summary
- Add `make ci` (verbatim mirror of `.github/workflows/test.yml`):
  - `ci-default` — modernc driver + DST + vet + build
  - `ci-ncruces` — ncruces driver
  - `ci-otel-target` — observer/otel tests
  All use `GOWORK=off` matching CI. Existing `test/test-drivers/test-otel`
  targets left untouched (they don't use GOWORK=off; preserved for
  alternate use cases).
- Add `make ci-fast` (vet+build+short tests, ~60s for pre-push)
- Add `make release VERSION=...` helper
- Add `.githooks/pre-push`
- Add `.github/RELEASE_TEMPLATE.md`

No remote behavior change.

## Test plan
- [x] `make ci-default` exits 0
- [x] `make ci-ncruces` exits 0
- [x] `make ci-otel-target` exits 0
- [x] `make ci-fast` runs in <60s
- [x] `make release VERSION=v9.9.9-test EDITOR=true` works and is reverted
- [x] `git push --dry-run` triggers the pre-push hook
- [x] `make release` rejects bad input

Spec: docs/superpowers/specs/2026-04-30-cross-repo-release-automation-design.md
EOF
)"
```

- [ ] **Step 3.5.2: Wait for CI green, user merges, cleanup**

```bash
gh pr checks --watch
```

After user merges:
```bash
git checkout main && git pull --ff-only origin main
git branch -d chore/local-dev-hygiene
git push origin --delete chore/local-dev-hygiene
```

---

## Phase 4: PR #4 — bones receiver (`bump-upstream.yml`) + goreleaser changelog groups

**Repo:** `/Users/dmestas/projects/bones`
**Branch:** `feat/upstream-bump-receiver`
**Files added:** `.github/workflows/bump-upstream.yml`
**Files modified:** `.goreleaser.yml`

**Prerequisite:** Phase 0 (DISPATCH_PAT setup) does not need to be complete for this PR — bones is only a receiver, no PAT needed in bones itself. Phase 0 must be complete before PR #5 merges.

### Task 4.0: Branch setup

- [ ] **Step 4.0.1: Create feature branch**

```bash
cd /Users/dmestas/projects/bones
git fetch origin
git checkout main
git pull --ff-only origin main
git checkout -b feat/upstream-bump-receiver
```

### Task 4.1: Add `bump-upstream.yml` receiver workflow

**Files:** Create `.github/workflows/bump-upstream.yml`

- [ ] **Step 4.1.1: Create the workflow file**

Write to `/Users/dmestas/projects/bones/.github/workflows/bump-upstream.yml`:

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
      EXPECTED_REPO: EdgeSync
      EXPECTED_MODULE: github.com/danmestas/EdgeSync
    steps:
      - name: Resolve inputs
        id: in
        env:
          PAYLOAD_REPO: ${{ github.event.client_payload.repo }}
          PAYLOAD_MODULE: ${{ github.event.client_payload.module_path }}
          PAYLOAD_VERSION: ${{ github.event.client_payload.version }}
          PAYLOAD_RELEASE_URL: ${{ github.event.client_payload.release_url }}
          PAYLOAD_TAG_SHA: ${{ github.event.client_payload.tag_sha }}
          PAYLOAD_ACTOR: ${{ github.event.client_payload.actor }}
          INPUT_REPO: ${{ inputs.repo }}
          INPUT_MODULE: ${{ inputs.module_path }}
          INPUT_VERSION: ${{ inputs.version }}
          INPUT_RELEASE_URL: ${{ inputs.release_url }}
          INPUT_TAG_SHA: ${{ inputs.tag_sha }}
          INPUT_ACTOR: ${{ inputs.actor }}
        run: |
          REPO="${PAYLOAD_REPO:-$INPUT_REPO}"
          MODULE_PATH="${PAYLOAD_MODULE:-$INPUT_MODULE}"
          VERSION="${PAYLOAD_VERSION:-$INPUT_VERSION}"
          RELEASE_URL="${PAYLOAD_RELEASE_URL:-$INPUT_RELEASE_URL}"
          TAG_SHA="${PAYLOAD_TAG_SHA:-$INPUT_TAG_SHA}"
          ACTOR="${PAYLOAD_ACTOR:-${INPUT_ACTOR:-${{ github.actor }}}}"

          if [[ "$REPO" != "$EXPECTED_REPO" ]]; then
            echo "::notice::Ignoring dispatch for repo=$REPO (expected $EXPECTED_REPO)"
            echo "skip=true" >> "$GITHUB_OUTPUT"
            exit 0
          fi
          if [[ "$MODULE_PATH" != "$EXPECTED_MODULE" ]]; then
            echo "::error::module_path mismatch: got '$MODULE_PATH', expected '$EXPECTED_MODULE'"
            exit 1
          fi
          if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$ ]]; then
            echo "::error::bad version: '$VERSION'"
            exit 1
          fi

          {
            echo "repo=$REPO"
            echo "module=$MODULE_PATH"
            echo "version=$VERSION"
            echo "release_url=$RELEASE_URL"
            echo "tag_sha=$TAG_SHA"
            echo "actor=$ACTOR"
            echo "branch=chore/bump-${REPO}-${VERSION}"
          } >> "$GITHUB_OUTPUT"

      - name: Idempotency check
        if: steps.in.outputs.skip != 'true'
        id: idem
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          BRANCH: ${{ steps.in.outputs.branch }}
        run: |
          if gh pr list --repo "${{ github.repository }}" --head "$BRANCH" --state open --json number -q '.[0].number' | grep -q .; then
            echo "exists=true" >> "$GITHUB_OUTPUT"
            echo "::notice::PR already open for $BRANCH; skipping"
          fi

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true'
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true'
        uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true'
        name: Bump module
        id: bump
        env:
          BRANCH: ${{ steps.in.outputs.branch }}
          MODULE: ${{ steps.in.outputs.module }}
          VERSION: ${{ steps.in.outputs.version }}
        run: |
          git switch -c "$BRANCH"
          # GOPROXY can lag the upstream tag push by a few seconds; retry
          # before giving up so transient propagation lag self-recovers.
          for i in 1 2 3; do
            if go get "${MODULE}@${VERSION}"; then
              break
            fi
            if [ "$i" -lt 3 ]; then
              echo "::notice::go get failed (attempt $i/3) — likely GOPROXY propagation race; retrying in 15s..."
              sleep 15
            else
              echo "::error::go get failed after 3 attempts"
              exit 1
            fi
          done
          go mod tidy
          if git diff --quiet go.mod go.sum; then
            echo "::notice::No-op bump — version already pinned"
            echo "noop=true" >> "$GITHUB_OUTPUT"
          fi

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true' && steps.bump.outputs.noop != 'true'
        name: Verify build
        run: go build ./...

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true' && steps.bump.outputs.noop != 'true'
        name: Commit + push + open PR
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          BRANCH: ${{ steps.in.outputs.branch }}
          REPO: ${{ steps.in.outputs.repo }}
          VERSION: ${{ steps.in.outputs.version }}
          MODULE: ${{ steps.in.outputs.module }}
          RELEASE_URL: ${{ steps.in.outputs.release_url }}
          TAG_SHA: ${{ steps.in.outputs.tag_sha }}
          ACTOR: ${{ steps.in.outputs.actor }}
        run: |
          git config user.name  "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git add go.mod go.sum
          git commit -m "chore: bump ${REPO} → ${VERSION}"
          git push -u origin "$BRANCH"
          gh pr create \
            --title "chore: bump ${REPO} → ${VERSION}" \
            --body "Auto-bump from upstream release.

          - Module: \`${MODULE}\`
          - Version: \`${VERSION}\`
          - Upstream release: ${RELEASE_URL}
          - Tag SHA: \`${TAG_SHA}\`
          - Triggered by: @${ACTOR}

          CI must pass before merge. Review go.sum diff for transitive surprises."
```

- [ ] **Step 4.1.2: Validate workflow syntax**

If `actionlint` is installed:
```bash
actionlint .github/workflows/bump-upstream.yml
```
Expected: no errors.

If actionlint isn't installed:
```bash
brew install actionlint   # macOS
```
Or skip locally and rely on GitHub's syntax check after push.

Also verify YAML is at least parseable:
```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/bump-upstream.yml'))" && echo OK
```
Expected: `OK`.

- [ ] **Step 4.1.3: Commit**

```bash
git add .github/workflows/bump-upstream.yml
git commit -m "feat(ci): add bump-upstream receiver workflow

Listens for repository_dispatch event_type=upstream-bump from EdgeSync's
release workflow. Validates payload, checks for existing open PR
(idempotency), runs go get + go mod tidy + go build, opens a chore PR.

Falls back to workflow_dispatch with the same inputs for manual recovery
if a dispatch is ever missed."
```

### Task 4.2: Add `groups:` to existing `.goreleaser.yml` changelog

**Files:** Modify `.goreleaser.yml`

- [ ] **Step 4.2.1: Read current changelog block**

```bash
grep -A 12 'changelog:' /Users/dmestas/projects/bones/.goreleaser.yml
```
Note: existing block has `sort: asc, use: github, exclude: [...]` per Explore snapshot.

- [ ] **Step 4.2.2: Modify the changelog block**

Open `/Users/dmestas/projects/bones/.goreleaser.yml`. Replace the existing `changelog:` block with:

```yaml
changelog:
  sort: asc
  use: github
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
      - '^chore:'
      - '^test:'
      - '^ci:'
      - 'merge conflict'
      - 'Merge PR'
      - 'Merge branch'
```

(Adjust `filters.exclude` to match the existing block's actual exclude patterns — preserve any project-specific entries.)

- [ ] **Step 4.2.3: Validate goreleaser config**

```bash
goreleaser check
```
Expected: `config is valid`.

If goreleaser isn't installed:
```bash
brew install goreleaser
```

- [ ] **Step 4.2.4: Commit**

```bash
git add .goreleaser.yml
git commit -m "feat(release): add changelog groups (Features/Bug fixes/Other)

Replaces flat commit list with conventional-commit-grouped sections in
the auto-generated GitHub release notes."
```

### Task 4.3: Open PR

- [ ] **Step 4.3.1: Push and PR**

```bash
cd /Users/dmestas/projects/bones
git push -u origin feat/upstream-bump-receiver
gh pr create --title "feat(ci): add bump-upstream receiver and changelog groups" --body "$(cat <<'EOF'
## Summary
- Add `.github/workflows/bump-upstream.yml` — listens for repository_dispatch
  events from EdgeSync's release workflow, opens a chore PR bumping
  `github.com/danmestas/EdgeSync` to the new version. Includes payload
  validation, idempotency check, no-op detection, and workflow_dispatch
  fallback for manual triggers.
- Modify `.goreleaser.yml` — add `groups:` to changelog config so release
  notes are split into Features / Bug fixes / Other sections.

No external trigger yet — EdgeSync's dispatch step is added in PR #5.
This PR is testable by manual `gh api` fire (see "Test plan" below).

## Test plan
- [x] `actionlint` accepts the workflow
- [x] `goreleaser check` accepts the config
- [ ] After merge: manual fire test (see DEPLOY NOTES)

## Deploy notes
After merge, smoke-test the receiver:

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

Expected: `noop=true` (already pinned), no PR created. Idempotency:
re-fire same command, expect "PR already open" notice (also no PR).
Bad input test: fire with `version=vbogus`, expect workflow failure.

Spec: see libfossil docs/superpowers/specs/2026-04-30-cross-repo-release-automation-design.md
EOF
)"
```

- [ ] **Step 4.3.2: Wait for CI green, user merges**

```bash
gh pr checks --watch
```

User merges manually.

### Task 4.4: Post-merge smoke tests

After PR #4 merges to main:

- [ ] **Step 4.4.1: Update local main**

```bash
cd /Users/dmestas/projects/bones
git checkout main
git pull --ff-only origin main
git branch -d feat/upstream-bump-receiver
git push origin --delete feat/upstream-bump-receiver
```

- [ ] **Step 4.4.2: Test no-op path (already-pinned version)**

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

Watch the run:
```bash
gh run list --workflow bump-upstream.yml --limit 1
gh run view --log $(gh run list --workflow bump-upstream.yml --limit 1 --json databaseId -q '.[0].databaseId')
```

Expected: workflow succeeds with `::notice::No-op bump — version already pinned`. No PR created.

- [ ] **Step 4.4.3: Test wrong-repo path (silent ignore)**

```bash
gh api -X POST repos/danmestas/bones/dispatches \
  -f event_type=upstream-bump \
  -f client_payload[repo]=somethingelse \
  -f client_payload[module_path]=github.com/danmestas/somethingelse \
  -f client_payload[version]=v1.0.0 \
  -f client_payload[release_url]=https://example.com \
  -f client_payload[tag_sha]=deadbeef \
  -f client_payload[actor]=danmestas
```

Expected: workflow succeeds with `::notice::Ignoring dispatch for repo=somethingelse (expected EdgeSync)`. No PR created.

- [ ] **Step 4.4.4: Test bad-version path (loud failure)**

```bash
gh api -X POST repos/danmestas/bones/dispatches \
  -f event_type=upstream-bump \
  -f client_payload[repo]=EdgeSync \
  -f client_payload[module_path]=github.com/danmestas/EdgeSync \
  -f client_payload[version]=vbogus \
  -f client_payload[release_url]=https://example.com \
  -f client_payload[tag_sha]=deadbeef \
  -f client_payload[actor]=danmestas
```

Expected: workflow fails with `::error::bad version: 'vbogus'`.

- [ ] **Step 4.4.5: Test workflow_dispatch fallback (UI-driven)**

```bash
gh workflow run bump-upstream.yml \
  -f repo=EdgeSync \
  -f module_path=github.com/danmestas/EdgeSync \
  -f version=v0.0.7 \
  -f release_url=https://github.com/danmestas/EdgeSync/releases/tag/v0.0.7 \
  -f tag_sha=$(gh api repos/danmestas/EdgeSync/git/refs/tags/v0.0.7 -q .object.sha) \
  -f actor=danmestas
```

Expected: same no-op result as Step 4.4.2.

If any test fails: investigate the run log, fix the workflow on a follow-up PR before proceeding to Phase 5.

---

## Phase 5: PR #5 — edgesync receiver + dispatcher + changelog groups

**Repo:** `/Users/dmestas/projects/edgesync`
**Branch:** `feat/upstream-bump-and-dispatch`
**Files added:** `.github/workflows/bump-upstream.yml`
**Files modified:** `.github/workflows/release.yml`, `.goreleaser.yml`

**Prerequisite:** Phase 0 (`DISPATCH_PAT` secret in edgesync repo) must be complete before merging this PR.

### Task 5.0: Branch setup

- [ ] **Step 5.0.1: Verify Phase 0 complete**

```bash
cd /Users/dmestas/projects/edgesync
gh secret list | grep DISPATCH_PAT
```
Expected: `DISPATCH_PAT  Updated <date>`. If absent, return to Phase 0 Step 0.2.

- [ ] **Step 5.0.2: Create feature branch**

```bash
git fetch origin
git checkout main
git pull --ff-only origin main
git checkout -b feat/upstream-bump-and-dispatch
```

### Task 5.1: Add `bump-upstream.yml` receiver

**Files:** Create `.github/workflows/bump-upstream.yml`

- [ ] **Step 5.1.1: Create the workflow file**

Write to `/Users/dmestas/projects/edgesync/.github/workflows/bump-upstream.yml` — same structure as bones (Step 4.1.1) but with different `EXPECTED_REPO` / `EXPECTED_MODULE`:

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
      EXPECTED_REPO: libfossil
      EXPECTED_MODULE: github.com/danmestas/libfossil
    steps:
      - name: Resolve inputs
        id: in
        env:
          PAYLOAD_REPO: ${{ github.event.client_payload.repo }}
          PAYLOAD_MODULE: ${{ github.event.client_payload.module_path }}
          PAYLOAD_VERSION: ${{ github.event.client_payload.version }}
          PAYLOAD_RELEASE_URL: ${{ github.event.client_payload.release_url }}
          PAYLOAD_TAG_SHA: ${{ github.event.client_payload.tag_sha }}
          PAYLOAD_ACTOR: ${{ github.event.client_payload.actor }}
          INPUT_REPO: ${{ inputs.repo }}
          INPUT_MODULE: ${{ inputs.module_path }}
          INPUT_VERSION: ${{ inputs.version }}
          INPUT_RELEASE_URL: ${{ inputs.release_url }}
          INPUT_TAG_SHA: ${{ inputs.tag_sha }}
          INPUT_ACTOR: ${{ inputs.actor }}
        run: |
          REPO="${PAYLOAD_REPO:-$INPUT_REPO}"
          MODULE_PATH="${PAYLOAD_MODULE:-$INPUT_MODULE}"
          VERSION="${PAYLOAD_VERSION:-$INPUT_VERSION}"
          RELEASE_URL="${PAYLOAD_RELEASE_URL:-$INPUT_RELEASE_URL}"
          TAG_SHA="${PAYLOAD_TAG_SHA:-$INPUT_TAG_SHA}"
          ACTOR="${PAYLOAD_ACTOR:-${INPUT_ACTOR:-${{ github.actor }}}}"

          if [[ "$REPO" != "$EXPECTED_REPO" ]]; then
            echo "::notice::Ignoring dispatch for repo=$REPO (expected $EXPECTED_REPO)"
            echo "skip=true" >> "$GITHUB_OUTPUT"
            exit 0
          fi
          if [[ "$MODULE_PATH" != "$EXPECTED_MODULE" ]]; then
            echo "::error::module_path mismatch: got '$MODULE_PATH', expected '$EXPECTED_MODULE'"
            exit 1
          fi
          if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$ ]]; then
            echo "::error::bad version: '$VERSION'"
            exit 1
          fi

          {
            echo "repo=$REPO"
            echo "module=$MODULE_PATH"
            echo "version=$VERSION"
            echo "release_url=$RELEASE_URL"
            echo "tag_sha=$TAG_SHA"
            echo "actor=$ACTOR"
            echo "branch=chore/bump-${REPO}-${VERSION}"
          } >> "$GITHUB_OUTPUT"

      - name: Idempotency check
        if: steps.in.outputs.skip != 'true'
        id: idem
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          BRANCH: ${{ steps.in.outputs.branch }}
        run: |
          if gh pr list --repo "${{ github.repository }}" --head "$BRANCH" --state open --json number -q '.[0].number' | grep -q .; then
            echo "exists=true" >> "$GITHUB_OUTPUT"
            echo "::notice::PR already open for $BRANCH; skipping"
          fi

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true'
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true'
        uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true'
        name: Bump module
        id: bump
        env:
          BRANCH: ${{ steps.in.outputs.branch }}
          MODULE: ${{ steps.in.outputs.module }}
          VERSION: ${{ steps.in.outputs.version }}
        run: |
          git switch -c "$BRANCH"
          # GOPROXY can lag the upstream tag push by a few seconds; retry
          # before giving up so transient propagation lag self-recovers.
          for i in 1 2 3; do
            if go get "${MODULE}@${VERSION}"; then
              break
            fi
            if [ "$i" -lt 3 ]; then
              echo "::notice::go get failed (attempt $i/3) — likely GOPROXY propagation race; retrying in 15s..."
              sleep 15
            else
              echo "::error::go get failed after 3 attempts"
              exit 1
            fi
          done
          go mod tidy
          if git diff --quiet go.mod go.sum; then
            echo "::notice::No-op bump — version already pinned"
            echo "noop=true" >> "$GITHUB_OUTPUT"
          fi

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true' && steps.bump.outputs.noop != 'true'
        name: Verify build
        run: go build ./...

      - if: steps.in.outputs.skip != 'true' && steps.idem.outputs.exists != 'true' && steps.bump.outputs.noop != 'true'
        name: Commit + push + open PR
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          BRANCH: ${{ steps.in.outputs.branch }}
          REPO: ${{ steps.in.outputs.repo }}
          VERSION: ${{ steps.in.outputs.version }}
          MODULE: ${{ steps.in.outputs.module }}
          RELEASE_URL: ${{ steps.in.outputs.release_url }}
          TAG_SHA: ${{ steps.in.outputs.tag_sha }}
          ACTOR: ${{ steps.in.outputs.actor }}
        run: |
          git config user.name  "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git add go.mod go.sum
          git commit -m "chore: bump ${REPO} → ${VERSION}"
          git push -u origin "$BRANCH"
          gh pr create \
            --title "chore: bump ${REPO} → ${VERSION}" \
            --body "Auto-bump from upstream release.

          - Module: \`${MODULE}\`
          - Version: \`${VERSION}\`
          - Upstream release: ${RELEASE_URL}
          - Tag SHA: \`${TAG_SHA}\`
          - Triggered by: @${ACTOR}

          CI must pass before merge. Review go.sum diff for transitive surprises."
```

- [ ] **Step 5.1.2: Validate**

```bash
actionlint .github/workflows/bump-upstream.yml
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/bump-upstream.yml'))" && echo OK
```
Expected: no errors; `OK`.

- [ ] **Step 5.1.3: Commit**

```bash
git add .github/workflows/bump-upstream.yml
git commit -m "feat(ci): add bump-upstream receiver workflow

Listens for repository_dispatch event_type=upstream-bump from libfossil's
release workflow. Validates payload, checks for existing open PR
(idempotency), runs go get + go mod tidy + go build, opens a chore PR.

EXPECTED_REPO=libfossil; rejects dispatches from other sources."
```

### Task 5.2: Modify `release.yml` to dispatch on tag

**Files:** Modify `.github/workflows/release.yml`

- [ ] **Step 5.2.1: Read current release.yml**

```bash
cat /Users/dmestas/projects/edgesync/.github/workflows/release.yml
```
Note the existing job name (`release`) and last step.

- [ ] **Step 5.2.2: Append dispatch step to the release job**

Edit `/Users/dmestas/projects/edgesync/.github/workflows/release.yml`. Add a new step at the end of the `release` job's `steps:` list:

```yaml
      - name: Dispatch downstream bump
        env:
          GH_TOKEN: ${{ secrets.DISPATCH_PAT }}
          DOWNSTREAM: bones
          MODULE_PATH: github.com/danmestas/EdgeSync
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

- [ ] **Step 5.2.3: Validate workflow**

```bash
actionlint .github/workflows/release.yml
```
Expected: no errors.

- [ ] **Step 5.2.4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "feat(ci): dispatch upstream-bump to bones on tag

After goreleaser publishes a release, fires repository_dispatch
to danmestas/bones so it can open a chore-bump PR. Uses DISPATCH_PAT
(fine-grained, scoped to bones contents:write)."
```

### Task 5.3: Add `groups:` to `.goreleaser.yml` changelog

**Files:** Modify `.goreleaser.yml`

- [ ] **Step 5.3.1: Read current changelog block**

```bash
grep -A 12 'changelog:' /Users/dmestas/projects/edgesync/.goreleaser.yml
```

- [ ] **Step 5.3.2: Modify changelog block**

Replace the existing `changelog:` block in `/Users/dmestas/projects/edgesync/.goreleaser.yml` with:

```yaml
changelog:
  sort: asc
  use: github
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
      - '^chore:'
      - '^test:'
      - '^ci:'
      - 'merge conflict'
      - 'Merge PR'
      - 'Merge branch'
```

(Preserve any project-specific `filters.exclude` patterns from the existing block.)

- [ ] **Step 5.3.3: Validate**

```bash
goreleaser check
```
Expected: `config is valid`.

- [ ] **Step 5.3.4: Commit**

```bash
git add .goreleaser.yml
git commit -m "feat(release): add changelog groups (Features/Bug fixes/Other)

Replaces flat commit list with conventional-commit-grouped sections in
the auto-generated GitHub release notes."
```

### Task 5.4: Open PR

- [ ] **Step 5.4.1: Push and PR**

```bash
cd /Users/dmestas/projects/edgesync
git push -u origin feat/upstream-bump-and-dispatch
gh pr create --title "feat(ci): receive libfossil dispatches, dispatch to bones, group changelog" --body "$(cat <<'EOF'
## Summary
- Add `.github/workflows/bump-upstream.yml` (receiver listening for libfossil dispatches)
- Modify `.github/workflows/release.yml` (dispatches to bones on tag)
- Modify `.goreleaser.yml` (group changelog into Features/Bug fixes/Other)

Requires `DISPATCH_PAT` secret already in place (Phase 0 prerequisite).

## Test plan
- [x] `actionlint` accepts both workflow files
- [x] `goreleaser check` accepts the config
- [ ] After merge: receiver smoke test via manual `gh api` fire to edgesync
- [ ] After merge: full sender test by tagging edgesync v0.0.8-rc1 and verifying bones receives a PR; close PR; delete rc tag

## Deploy notes
After merge, smoke-test the receiver:

```bash
gh api -X POST repos/danmestas/EdgeSync/dispatches \
  -f event_type=upstream-bump \
  -f client_payload[repo]=libfossil \
  -f client_payload[module_path]=github.com/danmestas/libfossil \
  -f client_payload[version]=v0.4.5 \
  -f client_payload[release_url]=https://github.com/danmestas/libfossil/releases/tag/v0.4.5 \
  -f client_payload[tag_sha]=$(gh api repos/danmestas/libfossil/git/refs/tags/v0.4.5 -q .object.sha) \
  -f client_payload[actor]=danmestas
```

Expected: noop (already pinned). Then test the sender by tagging
edgesync `v0.0.8-rc1` and watching bones open a bump PR (close PR
without merging; delete the rc tag and prerelease GitHub release).

Spec: see libfossil docs/superpowers/specs/2026-04-30-cross-repo-release-automation-design.md
EOF
)"
```

- [ ] **Step 5.4.2: Wait for CI green, user merges**

```bash
gh pr checks --watch
```

User merges manually.

### Task 5.5: Post-merge smoke tests

- [ ] **Step 5.5.1: Update local main and clean branch**

```bash
cd /Users/dmestas/projects/edgesync
git checkout main && git pull --ff-only origin main
git branch -d feat/upstream-bump-and-dispatch
git push origin --delete feat/upstream-bump-and-dispatch
```

- [ ] **Step 5.5.2: Test edgesync receiver with manual fire**

```bash
gh api -X POST repos/danmestas/EdgeSync/dispatches \
  -f event_type=upstream-bump \
  -f client_payload[repo]=libfossil \
  -f client_payload[module_path]=github.com/danmestas/libfossil \
  -f client_payload[version]=v0.4.5 \
  -f client_payload[release_url]=https://github.com/danmestas/libfossil/releases/tag/v0.4.5 \
  -f client_payload[tag_sha]=$(gh api repos/danmestas/libfossil/git/refs/tags/v0.4.5 -q .object.sha) \
  -f client_payload[actor]=danmestas
```

Watch:
```bash
gh run list --workflow bump-upstream.yml --limit 1
```
Expected: succeeds with no-op (libfossil v0.4.5 already pinned).

- [ ] **Step 5.5.3: Test edgesync→bones sender by tagging an rc**

Make sure tree is clean and on main:
```bash
git checkout main && git status
```

Tag and push:
```bash
git tag -a v0.0.8-rc1 -m "Test release for cascade validation"
git push origin v0.0.8-rc1
```

- [ ] **Step 5.5.4: Watch the cascade**

In one terminal:
```bash
gh run list --workflow release.yml --repo danmestas/EdgeSync --limit 1
gh run watch --repo danmestas/EdgeSync $(gh run list --workflow release.yml --repo danmestas/EdgeSync --limit 1 --json databaseId -q '.[0].databaseId')
```

After release.yml completes, check bones:
```bash
gh run list --workflow bump-upstream.yml --repo danmestas/bones --limit 3
```
Expected: a recent bump-upstream run dispatched. Check it opened a PR:
```bash
gh pr list --repo danmestas/bones --state open --head 'chore/bump-EdgeSync-v0.0.8-rc1'
```
Expected: one PR.

- [ ] **Step 5.5.5: Cleanup the test cascade**

Close the test PR without merging:
```bash
gh pr close --repo danmestas/bones $(gh pr list --repo danmestas/bones --state open --head 'chore/bump-EdgeSync-v0.0.8-rc1' --json number -q '.[0].number') --delete-branch
```

Delete the rc tag locally and remotely:
```bash
cd /Users/dmestas/projects/edgesync
git tag -d v0.0.8-rc1
git push origin --delete v0.0.8-rc1
```

Delete the prerelease GitHub release:
```bash
gh release delete v0.0.8-rc1 --yes --repo danmestas/EdgeSync
```

If any test fails: investigate, fix on a follow-up PR before proceeding to Phase 6.

---

## Phase 6: PR #6 — libfossil release.yml + dispatch to edgesync

**Repo:** `/Users/dmestas/projects/libfossil`
**Branch:** `feat/release-workflow`
**Files added:** `.github/workflows/release.yml`

This is the smallest PR but the most consequential — it creates libfossil's first-ever release pipeline.

**Prerequisite:** `DISPATCH_PAT` secret in libfossil repo (Phase 0 Step 0.1) must be in place.

### Task 6.0: Branch setup

- [ ] **Step 6.0.1: Verify Phase 0 complete for libfossil**

```bash
cd /Users/dmestas/projects/libfossil
gh secret list | grep DISPATCH_PAT
```
Expected: `DISPATCH_PAT  Updated <date>`. If absent, return to Phase 0 Step 0.1.

- [ ] **Step 6.0.2: Create feature branch**

```bash
git fetch origin
git checkout main
git pull --ff-only origin main
git checkout -b feat/release-workflow
```

### Task 6.1: Create `release.yml`

**Files:** Create `.github/workflows/release.yml`

- [ ] **Step 6.1.1: Create the workflow file**

Write to `/Users/dmestas/projects/libfossil/.github/workflows/release.yml`:

```yaml
name: Release
on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Create GitHub release
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          gh release create "${{ github.ref_name }}" \
            --generate-notes \
            --verify-tag

      - name: Dispatch downstream bump
        env:
          GH_TOKEN: ${{ secrets.DISPATCH_PAT }}
          DOWNSTREAM: EdgeSync
          MODULE_PATH: github.com/danmestas/libfossil
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

- [ ] **Step 6.1.2: Validate**

```bash
actionlint .github/workflows/release.yml
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))" && echo OK
```
Expected: no errors; `OK`.

- [ ] **Step 6.1.3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "feat(ci): add release workflow with downstream dispatch

On tag push (v*):
  1. Create GitHub release with auto-generated notes from PRs/commits
     since previous tag. Source tarball auto-attached.
  2. Dispatch repository_dispatch event to danmestas/EdgeSync to trigger
     a chore-bump PR there.

libfossil is a library — no binaries are published, so goreleaser is
overkill. 'gh release create --generate-notes' provides equivalent
auto-changelog and source archives in 5 lines."
```

### Task 6.2: Open PR

- [ ] **Step 6.2.1: Push and PR**

```bash
git push -u origin feat/release-workflow
gh pr create --title "feat(ci): add release workflow with downstream dispatch" --body "$(cat <<'EOF'
## Summary
First release pipeline for libfossil. On tag push:
- Creates GitHub release with auto-generated notes (`gh release --generate-notes`)
- Dispatches `repository_dispatch` to `danmestas/EdgeSync` for downstream bump

Closes the cascade chain. Once merged, the next libfossil tag will fire
PRs in EdgeSync (and via PR #5 also in bones).

## Test plan
- [x] `actionlint` accepts the workflow
- [ ] After merge: tag `v0.4.6-rc1` and watch:
  - libfossil release.yml runs both steps
  - GitHub release `v0.4.6-rc1` created (prerelease)
  - EdgeSync's bump-upstream.yml runs and opens PR `chore/bump-libfossil-v0.4.6-rc1`
- [ ] Cleanup: close EdgeSync PR; delete rc tag from libfossil + EdgeSync; delete libfossil prerelease

## Deploy notes
Test cascade after merge:

```bash
cd /Users/dmestas/projects/libfossil
git checkout main && git pull
git tag -a v0.4.6-rc1 -m "Test release for cascade validation"
git push origin v0.4.6-rc1
```

Then watch:
```bash
gh run list --workflow release.yml --limit 1
```

After release.yml succeeds, watch EdgeSync:
```bash
gh run list --workflow bump-upstream.yml --repo danmestas/EdgeSync --limit 1
gh pr list --repo danmestas/EdgeSync --head 'chore/bump-libfossil-v0.4.6-rc1'
```

Cleanup (do not merge the rc PR):
```bash
gh pr close --repo danmestas/EdgeSync $(gh pr list --repo danmestas/EdgeSync --head 'chore/bump-libfossil-v0.4.6-rc1' --json number -q '.[0].number') --delete-branch
git tag -d v0.4.6-rc1
git push origin --delete v0.4.6-rc1
gh release delete v0.4.6-rc1 --yes
```

Spec: docs/superpowers/specs/2026-04-30-cross-repo-release-automation-design.md
EOF
)"
```

- [ ] **Step 6.2.2: Wait for CI green, user merges**

```bash
gh pr checks --watch
```

User merges manually.

### Task 6.3: End-to-end cascade validation

After PR #6 merges:

- [ ] **Step 6.3.1: Update local main and clean branch**

```bash
cd /Users/dmestas/projects/libfossil
git checkout main && git pull --ff-only origin main
git branch -d feat/release-workflow
git push origin --delete feat/release-workflow
```

- [ ] **Step 6.3.2: Tag a release candidate**

```bash
git tag -a v0.4.6-rc1 -m "Test release for cascade validation"
git push origin v0.4.6-rc1
```

- [ ] **Step 6.3.3: Watch libfossil release.yml**

```bash
gh run list --workflow release.yml --limit 1
gh run watch $(gh run list --workflow release.yml --limit 1 --json databaseId -q '.[0].databaseId')
```
Expected: succeeds; both `Create GitHub release` and `Dispatch downstream bump` steps complete.

- [ ] **Step 6.3.4: Verify GitHub release created**

```bash
gh release view v0.4.6-rc1
```
Expected: prerelease with auto-generated notes.

- [ ] **Step 6.3.5: Verify EdgeSync bump-upstream.yml fired**

```bash
gh run list --workflow bump-upstream.yml --repo danmestas/EdgeSync --limit 1
gh pr list --repo danmestas/EdgeSync --head 'chore/bump-libfossil-v0.4.6-rc1' --json number,title,url
```
Expected: a recent bump-upstream run; one open PR titled `chore: bump libfossil → v0.4.6-rc1`.

- [ ] **Step 6.3.6: Cleanup the test cascade**

```bash
gh pr close --repo danmestas/EdgeSync $(gh pr list --repo danmestas/EdgeSync --head 'chore/bump-libfossil-v0.4.6-rc1' --json number -q '.[0].number') --delete-branch
git tag -d v0.4.6-rc1
git push origin --delete v0.4.6-rc1
gh release delete v0.4.6-rc1 --yes
```

Verify cleanup:
```bash
gh release list --limit 5
gh pr list --repo danmestas/EdgeSync --state open
```
Expected: no `v0.4.6-rc1` artifacts remain.

### Task 6.4: Update memory with cascade-active state

- [ ] **Step 6.4.1: Note the cascade is now live**

The deferred CI improvements memory file (`/Users/dmestas/.claude/projects/-Users-dmestas-projects-libfossil/memory/project_pending_ci_improvements.md`) now has additional context: the cascade automation has shipped, the `-race`/SDK-drift/DST-sweep work can be added to `make ci` rather than just CI.

This is a memory-update step; perform via the auto-memory system in conversation rather than as a code commit.

---

## Self-review checklist

After plan completes (all 6 PRs merged + post-merge tests passed):

- [ ] **Cascade lag:** time from a libfossil tag to bones PR open is <5 min (was 1-3 days).
- [ ] **PR count per cascade:** bones gets exactly one bump PR per cascade (was two).
- [ ] **Local CI mirror:** `make ci` in each repo exits 0 on a clean main; matches what GitHub CI runs.
- [ ] **Pre-push hook:** `git push --dry-run` triggers `make ci-fast`; failures block push.
- [ ] **Release helper:** `make release VERSION=...` validates clean tree, runs ci, creates annotated tag locally with template body.
- [ ] **Auto-changelog:** edgesync v0.0.9 and bones v0.3.4 release notes are grouped (Features/Bug fixes/Other) without manual annotation work.
- [ ] **PAT rotation reminder set** for ~10 months out.

If any item fails after the rollout: investigate that piece in isolation; the cascade can be repaired one workflow file at a time without touching the others.
