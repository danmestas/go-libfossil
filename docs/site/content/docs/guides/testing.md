---
title: Testing
weight: 50
aliases: ["/docs/testing/"]
---

This guide describes libfossil's test strategy for contributors adding tests or
investigating failures. For the module layout referenced throughout, see
[Architecture](/docs/concepts/architecture/).

## Overview

libfossil's test suite has these layers:

1. **Unit tests** — standard `go test ./...` coverage for individual packages
   under the root module and `internal/...`.
2. **Conformance tests** — `./conformance`, which checks the library against
   the Fossil Internet-Drafts in `conformance/rfcs/`, requirement by
   requirement, and against real `fossil` and `zit` binaries where they are
   present. See [Conformance](/docs/reference/conformance/).
3. **DST simulation tests** — deterministic simulation scenarios under
   `./dst/...` that drive multiple repos through a virtual clock with fault
   injection and invariant checks.
4. **Fuzz targets** — Go native fuzz tests for the parsers and codecs.
5. **Dual-driver matrix** — the whole suite runs against two SQLite drivers
   (`modernc` and `ncruces`) to catch driver-specific behavior.

## Running tests

The `Makefile` wraps the common targets:

- `make test` — runs `go test ./... -count=1 -timeout=120s` with the default
  `modernc` driver.
- `make test-drivers` — runs the full suite twice:
  `go test ./... -count=1 -timeout=120s` followed by
  `go test -tags test_ncruces ./... -count=1 -timeout=120s`.
- `make test-otel` — runs the out-of-workspace OTel submodule with
  `cd observer/otel && GOWORK=off go test ./... -count=1`.
- `make test-all` — chains `test-drivers`, `test-otel`, and `go build
  ./cmd/libfossil/`.
- `make ci` — mirrors `.github/workflows/test.yml`. It uses `GOWORK=off`,
  runs the packages other than `dst` with a 120-second timeout, runs `./dst/...`
  with a 300-second timeout, runs the driver submodule tests, `go vet` and
  the CLI build, then repeats the tests under `-tags test_ncruces` and runs
  the OTel submodule. Run it before opening a PR. `make release VERSION=vX.Y.Z`
  runs it before it tags.

`make setup-hooks` points `core.hooksPath` at `.githooks/`. The installed
pre-commit hook runs both drivers with `-short`, `go vet`, the OTel submodule,
the CLI build, and a check that the generated SDK reference pages are in sync,
in roughly 45 seconds. Skip with `git commit --no-verify` only in emergencies.

Tests that compare against real binaries skip when the binary is missing.
`REQUIRE_FOSSIL_BIN=1` turns a missing `fossil` into a failure, and CI sets
it. `fossil` keeps a global config in `$HOME`, so run those tests with a
scratch `HOME` if you do not want your own touched.

## Fuzzing

There are seven fuzz targets. Each is also a regular test over its seed
corpus, so `go test` runs the seeds without `-fuzz`.

| Target | Package | Exercises |
|---|---|---|
| `FuzzUnified` | `internal/diff` | Unified diff output. |
| `FuzzTableSyncPayload` | `internal/sync` | Extension-table sync payload parsing. |
| `FuzzParseXDelete` | `internal/xfer` | The `xdelete` card parser. |
| `FuzzDecode` | `internal/xfer` | The xfer message decoder and its size limits. |
| `FuzzArtifactRoundTrip` | `conformance` | Artifact parse and re-encode. |
| `FuzzDeltaApply` | `conformance` | Applying arbitrary deltas. |
| `FuzzDeltaRoundTrip` | `conformance` | Delta create then apply. |

Run one with, for example:

```
go test ./conformance -run '^$' -fuzz FuzzDeltaApply -fuzztime 60s
```

CI does not run `-fuzz`, and no Makefile target or script runs the fuzz
targets or a seed sweep. They are run by hand.

## Corpus parity gate

`TestCloneCorpusParity` (`apitest/` package, `apitest/clone_corpus_parity_test.go`) clones one
repository two ways — with the canonical `fossil` binary and with
`libfossil.Clone` — and compares what each derived. It is opt-in:

```
LIBFOSSIL_CORPUS_PARITY=1 go test -run TestCloneCorpusParity -v .
```

Without the variable it skips, so `make test` is unchanged in both runtime and
outcome. It also needs a fossil binary and skips without one (or fails, under
`REQUIRE_FOSSIL_BIN=1`). Budget roughly a second and under a megabyte of temp
space; everything lands in `t.TempDir()` and is removed on exit.

It exists because the per-package parity tests compare our derivation against
fossil's for fixtures built in-test, which only covers the shapes someone
thought to build. Issue #184 — a silent 100% loss of the ticket-change artifact
shape — passed that suite and was found by an external benchmark instead.

The gate asserts two things:

- **Per-`event`-type counts match**, not just the total. This distinction is the
  point: on the real Fossil repository the totals read 23,396 vs 27,099, a 14%
  shortfall easy to wave off, while the per-type breakdown put ticket events at
  96 vs 3,808.
- **Row totals match for `blob`, `event`, `mlink`, `plink`, `tagxref` and
  `leaf`**, reported in both directions. A surplus matters as much as a
  shortfall — our `tagxref` ran +17,119 over fossil's on the real repository,
  which is what writing a tag without finishing the work it marks looks like.

Failures name the table or type and the delta, e.g.
`event type "t": got 0, want 10 (-10)`.

The corpus is built by the canonical binary at test time rather than
downloaded: seconds and kilobytes instead of the ~10 minutes and ~1 GB the real
Fossil repository costs, no network dependency, and contents that are a
property of the test rather than of whatever a remote server holds today. It
covers check-ins (including a branch and a merge), ticket creations and
updates, a wiki page, a technote, an attachment, and a tag control artifact.

`requireCorpusShapes` classifies every blob by its card set and fails if any
shape falls below its expected floor. Without that check a corpus that quietly
stopped producing ticket artifacts would let the gate pass while covering
nothing — the exact blind spot it exists to remove.

Not covered: forum posts (`f` events), because fossil exposes no CLI to create
them; the scale-dependent behaviour of a large repository; and per-row content
comparison — this gate counts rows, and `internal/manifest` diffs them.

## Driver matrix

libfossil abstracts SQLite behind `db/` and two driver submodules. Both must
pass identical tests:

- **`modernc`** (default) — pure-Go port of SQLite. Used by every build
  that doesn't set a tag. Registered via `internal/testdriver/modernc.go`
  under `//go:build !test_ncruces && !test_mattn`.
- **`ncruces`** (wasm-capable) — selected with `-tags test_ncruces`, suitable
  for WASM targets. Registered via `internal/testdriver/ncruces.go` under
  `//go:build test_ncruces`.

GitHub Actions (`.github/workflows/test.yml`) runs `test`, `test-ncruces`,
`build-wasm`, `build-tinygo`, `test-otel` and `test-docs` as parallel jobs. The first two
install the pinned fossil 2.28, build every package, then run the unit tests,
DST and the driver submodule tests under modernc and ncruces respectively.
`build-wasm` compiles the packages for `js/wasm` with the ncruces tag,
`build-tinygo` builds `examples/tinygo` with TinyGo 0.43, and
`test-docs` builds this site with Hugo. For the canonical local invocation,
use `make test-drivers` or `make ci` rather than hand-rolling tag flags.

## DST (Deterministic Simulation Testing)

DST is inspired by FoundationDB and TigerBeetle: run many repos in a single
process under a simulated clock, inject faults, and replay a failing seed
deterministically. The harness lives in `dst/simulator.go` and
`dst/invariants.go`.

### Seeds and reproducibility

`dst.SimConfig.Seed` drives every random source used by the simulator: the
event queue, network fault decisions, and a `SeededBuggify` PRNG that gates
fault-injection sites. Any failure on seed N reproduces exactly on seed N.

`dst/scenario_test.go` exposes three flags driving `TestDST`:

```
go test ./dst -run 'TestDST$' -seed=42 -level=hostile -steps=10000
```

`TestDST` skips unless at least one flag is set. It builds a master repo
with 50 artifacts, 5 unversioned files and 3 private blobs, starts a
simulator with 3 leaves, and checks safety invariants as it runs.

- `-seed=<int64>` — seed passed to `SimConfig.Seed`. In `TestDST`, 0 means
  seed 1; other scenarios use their own fixed seeds.
- `-level={normal,adversarial,hostile}` — picks a `severity` struct setting
  `DropRate` and `Buggify`: `normal` is no drops and no BUGGIFY,
  `adversarial` is 10% drops with BUGGIFY, `hostile` is 20% drops with
  BUGGIFY. An empty or unknown level is `normal`.
- `-steps=<int>` — caps the number of events processed (default 10000 in
  `TestDST`).

Seed-parameterized scenarios (e.g. `TestCloneDSTSeedSweep` in
`dst/clone_test.go`, which runs seeds 0 to 19) loop over seeds via `t.Run(fmt.Sprintf("seed_%d", seed),
...)`. To reproduce a single sub-seed: `go test ./dst -run
TestCloneDSTSeedSweep/seed_7 -v`.

### Invariants

`dst/invariants.go` defines per-node and cross-node checks. `Simulator.CheckSafety`
runs the per-node set after every `SafetyCheckInterval` steps; convergence
checks run once the sim has quiesced:

- **Safety (anytime):** `CheckBlobIntegrity` (UUID matches hash of expanded
  content), `CheckDeltaChains` (`delta.srcid` resolves to a blob),
  `CheckNoOrphanPhantoms`, `CheckUVIntegrity`, `CheckTableSyncIntegrity`.
  `CheckTagxrefIntegrity` is a separate check that scenarios call
  directly.
- **Convergence (after fault-free sync):** `CheckConvergence` (leaves match
  master blob-for-blob), `CheckSubsetOf`, `CheckUVConvergence`,
  `CheckTableSyncConvergence`, `CheckTombstoneConvergence`.

Failures return a `*InvariantError{Invariant, NodeID, Detail}` naming the
failing invariant and the offending node. The scenarios add the seed and
level to the failure message, so a log line is enough to replay the run.
The checks run with BUGGIFY suspended, so they see what is persisted and
not faults injected into their own reads.

## BUGGIFY

BUGGIFY marks code points where faults can be injected — shrunk send
budgets, dropped or rejected cards, bad nonces, byte-flipped content. About
twenty sites exist in `internal/sync`, `internal/blob` and
`internal/content`. In production
the check is a single boolean read; under DST the seeded PRNG decides whether
to fire.

Two styles appear in the tree:

1. **Process-global, used outside the sync path.** `simio.Buggify(probability)`
   consults a process-global state toggled by `simio.EnableBuggify(seed)` /
   `DisableBuggify()` (see `simio/buggify.go`). Example from
   `internal/content/content.go`:

   ```go
   // BUGGIFY: flip a byte in expanded content to exercise the hash check
   // below under DST.
   if check && simio.Buggify(0.01) && len(content) > 0 {
       corrupted := make([]byte, len(content))
       copy(corrupted, content)
       corrupted[0] ^= 0xFF
       content = corrupted
       verified = false
   }
   ```

2. **Scoped, passed through session options.** `BuggifyChecker` is an interface
   (`Check(site string, probability float64) bool`) defined in
   `internal/sync/session.go` and re-exported from the root as
   `libfossil.BuggifyChecker`. Sync code threads a checker through so each site
   has a stable name:

   ```go
   // BUGGIFY: drop the last file card to simulate partial send.
   if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.buildFileCards.skip", 0.05) && len(cards) > 0 {
       cards = cards[:len(cards)-1]
   }
   ```

`dst.SeededBuggify` in `dst/simulator.go` implements `BuggifyChecker` with a
seeded `*rand.Rand` (it ignores the site name). `Simulator.New` enables both styles when `SimConfig.Buggify`
is true, wiring independent PRNG streams into the client, the server-side
`MockFossil`, and the global `simio` state.

## Interop with fossil 2.28 and zit

Two other implementations check libfossil's output.

**Fossil.** CI downloads the official fossil 2.28 release tarball
(`FOSSIL_VERSION` in `.github/workflows/test.yml`) and verifies its SHA-256
and its reported version, so every run compares against the same build.
Tests in `conformance/`, `cli/`, `apitest/` and the root package create repositories with
stock `fossil`, have it open and verify what libfossil wrote, and compare
the rows libfossil derives with the rows fossil derives. Locally they use
the `fossil` on `PATH`; `fossil version` should report 2.28.

**zit.** zit is a Zig reimplementation of Fossil. The `TestZit*` tests in
`conformance/*zit*_test.go` (20 of them) cover the newer drafts: sub-manifest
trees (`zit_interop_test.go`, `zit_writer_test.go`), sparse cones and sparse
stores, and horizons (serve, structural seeds, fallback, push check, cursor,
nodes, history URL and merge base). Each runs both ways where it can:
go-libfossil clones from and pushes to `zit serve`, and zit clones from and
pushes to go-libfossil's handler. They need a `zit` on `PATH` (or `ZIT_BIN`)
at a recent enough commit and skip otherwise. `REQUIRE_ZIT_BIN=1` makes a
missing or older zit a failure. CI has no zit, so there the bundle tests
(`TestZitTreeBundle`, `TestZitWriterBundle`) read recorded copies of what
zit wrote from `conformance/testdata/`. `ZIT_REGENERATE=1` rewrites those
copies from a live zit.

Signed check-ins are covered the same way: `TestSignedCheckin` has fossil
and zit clone and read libfossil's signed check-ins, and checks that a
check-in zit signs verifies in libfossil.

## Writing new tests

- Prefer table-driven tests in unit code; include success and failure rows.
- If you touch sync, merge, clone, or checkin, add a DST scenario under
  `dst/` and run it across a seed range (see `TestCloneDSTSeedSweep` for the
  pattern).
- Mark any new fault-sensitive code point with BUGGIFY. Use the scoped
  `BuggifyChecker` style when the site lives inside the sync session; reserve
  `simio.Buggify` for code that runs outside an explicit session scope.
- Assert invariants. If the new behavior warrants one, add it to
  `dst/invariants.go` and wire it into `CheckSafety` or a convergence helper.
- Confirm tests pass under both drivers (`make test-drivers`) before
  requesting review.
