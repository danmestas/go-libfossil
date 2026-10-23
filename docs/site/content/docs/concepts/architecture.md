---
title: Architecture
weight: 10
aliases: ["/docs/architecture/"]
---

<svg viewBox="0 0 640 260" role="img" aria-labelledby="arch-title arch-desc" preserveAspectRatio="xMidYMid meet" style="display:block;width:100%;height:auto;margin:0 auto 1.5rem;color:currentColor;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;max-width:720px">
  <title id="arch-title">libfossil architecture</title>
  <desc id="arch-desc">Callers (cli, library, wasm) invoke methods on the Repo handle. The Repo delegates to three pluggable axes: db driver, transport, observer. Repository state lives on disk in a .fossil SQLite file.</desc>
  <style>
    .a-rule { stroke: currentColor; stroke-width: 1; opacity: 0.4; }
    .a-box  { fill: none; stroke: currentColor; stroke-width: 1; opacity: 0.7; }
    .a-head { font-size: 11px; letter-spacing: 1.4px; text-transform: uppercase; opacity: 0.65; }
    .a-item { font-size: 13px; }
    .a-flow line { stroke: #c63a0f; stroke-width: 1.5; fill: none; }
    .a-arrow { fill: #c63a0f; }
    .a-flow-label { font-size: 10px; fill: #c63a0f; letter-spacing: 0.6px; text-transform: uppercase; }
  </style>
  <defs>
    <marker id="arch-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
      <path class="a-arrow" d="M0,0 L10,5 L0,10 Z"/>
    </marker>
  </defs>
  <g class="a-rule">
    <line x1="40" y1="38" x2="160" y2="38"/>
    <line x1="240" y1="38" x2="400" y2="38"/>
    <line x1="480" y1="38" x2="600" y2="38"/>
  </g>
  <g class="a-head" fill="currentColor">
    <text x="100" y="28" text-anchor="middle">callers</text>
    <text x="320" y="28" text-anchor="middle">core</text>
    <text x="540" y="28" text-anchor="middle">plugins</text>
  </g>
  <g class="a-item" fill="currentColor">
    <text x="100" y="76" text-anchor="middle">cli</text>
    <text x="100" y="100" text-anchor="middle">library</text>
    <text x="100" y="124" text-anchor="middle">wasm</text>
  </g>
  <rect class="a-box" x="240" y="60" width="160" height="100"/>
  <g class="a-item" fill="currentColor">
    <text x="320" y="88" text-anchor="middle">Open</text>
    <text x="320" y="112" text-anchor="middle">Sync</text>
    <text x="320" y="136" text-anchor="middle">Timeline</text>
  </g>
  <rect class="a-box" x="490" y="60" width="100" height="100"/>
  <g class="a-item" fill="currentColor">
    <text x="540" y="88" text-anchor="middle">db driver</text>
    <text x="540" y="112" text-anchor="middle">transport</text>
    <text x="540" y="136" text-anchor="middle">observer</text>
  </g>
  <rect class="a-box" x="240" y="200" width="160" height="40"/>
  <g class="a-item" fill="currentColor">
    <text x="320" y="225" text-anchor="middle">.fossil (SQLite)</text>
  </g>
  <g class="a-flow">
    <line x1="160" y1="100" x2="234" y2="100" marker-end="url(#arch-arrow)"/>
    <line x1="400" y1="88" x2="486" y2="88" marker-end="url(#arch-arrow)"/>
    <line x1="490" y1="136" x2="404" y2="136" marker-end="url(#arch-arrow)"/>
    <line x1="320" y1="160" x2="320" y2="196" marker-end="url(#arch-arrow)"/>
  </g>
  <g class="a-flow-label">
    <text x="197" y="92" text-anchor="middle">calls</text>
    <text x="443" y="80" text-anchor="middle">delegate</text>
    <text x="447" y="154" text-anchor="middle">events</text>
    <text x="334" y="183" text-anchor="start">persist</text>
  </g>
</svg>

## Overview

libfossil is a pure-Go library and CLI that reads and writes Fossil SCM's
`.fossil` SQLite repositories. The public surface (package `libfossil`)
exposes `Repo`, `Create`, `Open`, `Clone`, checkout, and check-in
operations; internals live under `internal/`. SQLite access is mediated
by a pluggable driver layer in `db/`, with two shippable drivers
(`modernc`, `ncruces`) selected by blank import. Instrumentation is
pulled in by implementing the `SyncObserver` / `CheckoutObserver`
interfaces; an optional OTel adapter lives in its own submodule. A
deterministic simulator under `dst/` plus a `BUGGIFY` fault-injection
harness in `simio/` exercise concurrent repo operations under a seed.

## Module layout

The library is a Go workspace (`go.work`) of four modules: the root, the
two driver modules, and the OTel observer. (The documentation site under
`docs/site` has its own `go.mod` and is not in the workspace.) Layout:

- **Root package `libfossil`** — thin facade over `internal/repo`,
  `internal/sync`, and `internal/checkout`. Main files: `fossil.go`
  (Create, Open, Clone), `repo.go` (Repo handle + DB accessor),
  `checkout.go`, `checkout_sparse.go` (cones and horizons),
  `repo_checkout.go`, `repo_merge.go`, `repo_sync.go`, `repo_pull.go`,
  `repo_sparse.go`, `repo_submanifest.go`, `repo_history.go`,
  `repo_admin.go`, `repo_extras.go`, `required_content*.go`,
  `horizon_*.go`, `signing.go`, `transport.go`, `observer.go`, `types.go`,
  `errors.go`, `julian.go`.
- **`cli/`** — Kong command definitions. `repo.go` aggregates 43
  subcommands (`new`, `clone`, `ci`, `co`, `timeline`, `diff`, `merge`,
  `tag`, `branch`, `uv`, `stash`, `bisect`, `annotate`, `user`, `serve`,
  `cone`, `submanifests`, etc.). Intended to be embedded by any `main` that
  wants a fossil-shaped CLI.
- **`cmd/libfossil/`** — the shipped binary. `main.go` wires Kong and
  dispatches to `cli`; `driver_modernc.go` and `driver_ncruces.go` select
  the driver by build tag (`modernc` by default, `ncruces` under
  `test_ncruces`).
- **`internal/`** — private implementation, grouped by concern:
  - Storage layer: `blob`, `content`, `manifest`, `deck`, `delta`,
    `hash`, `fsltype`, `repo` (schema + DB boot), `tag`, `leaf`,
    `clearsign`.
  - Sparse and horizon stores: `cone`, `horizon` (held set and seed).
  - Working-tree operations: `checkout` (checkin, extract, update,
    revert, fork, rename, status), `vfile`, `merge` (three-way, ancestor,
    detect, resolve, strategies), `diff`, `annotate`, `undo`, `stash`,
    `bisect`, `branch`, `path`.
  - Sync / transport: `sync` (client, handler, clone, session,
    ckin-lock, serve-http, table-sync, UV, sparse and horizon serving),
    `xfer` (card codec).
  - Housekeeping: `auth`, `shun`, `verify`, `search`, `uv`, `testdriver`.
- **`db/`** — SQLite abstraction (see below).
- **`db/driver/modernc`**, **`db/driver/ncruces`** — separate modules,
  each registers exactly one `database/sql` driver.
- **`observer/otel/`** — separate module implementing the observer
  interfaces against OpenTelemetry.
- **`dst/`** — deterministic simulator (`simulator.go`, `node.go`,
  `network.go`, `peer_network.go`, `mock_fossil.go`, `invariants.go`,
  `event.go`) with scenario-style tests.
- **`simio/`** — deterministic IO primitives: `SimClock`, seeded
  `Rand`, in-memory storage, and the global `Buggify` switch.
- **`testutil/`** — shared test helpers for the rest of the tree.

## Storage model

A `.fossil` file is a SQLite database. All repository state — blobs,
manifests, tags, branches, unversioned files, tickets, wiki, config,
users — lives in that file. libfossil does not maintain any sidecar
state for the repo itself (a working tree has its own SQLite DB).

Content is stored **content-addressed** in the `blob` table, keyed by
either SHA1 or SHA3-256 names (the repository's hash policy picks which
names new content; see [Artifacts](/docs/concepts/artifacts/)). Every blob is
zlib-compressed with a 4-byte big-endian uncompressed-size prefix;
`internal/blob.Compress` and `Decompress` handle that framing. Blobs may
be stored as full content or as deltas against a parent; reconstruction
walks the chain and applies deltas via `internal/content.Expand`, with
cycle detection. `internal/content.Verify` rehashes expanded content
against the stored UUID.

**Structural artifacts** are themselves blobs whose text describes a
check-in (a manifest), a tag change, a wiki edit, a ticket change, an
attachment, a technote, a forum post, or a cluster. `internal/deck`
parses and marshals them and `internal/manifest` builds check-ins;
"crosslinking" (`internal/manifest/crosslink.go`) is the step that
projects an artifact's effects into derived tables (`mlink`, `plink`,
`event`, `tagxref`, etc.) so queries like timeline and checkout work
without re-parsing blobs. See [Repository state](/docs/concepts/repository-state/).

## Driver layer

`db/` is a thin wrapper over `database/sql`. See
[`db/doc.go`](https://github.com/danmestas/go-libfossil/blob/main/db/doc.go). A driver registers at `init` time via
`db.Register(DriverConfig{Name, BuildDSN})`; exactly one registration is
allowed (a second call panics). Library embedders blank-import exactly one
driver. `db.Open` / `db.OpenWith` look up the registered driver, run
`BuildDSN(path, pragmas)` to produce the DSN, and then call `sql.Open` with
that driver name. Both shipped drivers construct
`file:<path>?_pragma=k(v)&...` DSNs — differing only in the `database/sql`
driver name they register under (`"sqlite"` for modernc, `"sqlite3"` for
ncruces).

Shipped drivers:

- **`db/driver/modernc`** — default for `cmd/libfossil`. Blank-imports
  `modernc.org/sqlite`, a pure-Go, CGo-free translation of SQLite with full
  filesystem access.
- **`db/driver/ncruces`** — blank-imports `github.com/ncruces/go-sqlite3`,
  which runs SQLite as WebAssembly. Has a `ncruces_js.go` variant for
  `GOOS=js` / browser targets. Used for `GOOS=wasip1` builds and by
  `go build -tags test_ncruces ./cmd/libfossil`.

## Sync protocol

`internal/sync` implements Fossil's sync-over-HTTP protocol as a client
and a stateless server, with the card codec in `internal/xfer`. See
[Sync](/docs/concepts/sync/) for the protocol and where each part lives.

## Observer model

The core keeps instrumentation as a non-goal; hooks are plain Go
interfaces on the public API. `SyncObserver` and `CheckoutObserver` in
[`observer.go`](https://github.com/danmestas/go-libfossil/blob/main/observer.go) define lifecycle events (session
start/end, per-round stats, extract/scan/commit phases). `NopSyncObserver`
/ `NopCheckoutObserver` are zero-cost defaults; `StdoutSyncObserver` /
`StdoutCheckoutObserver` log to stderr for quick debugging.

For OpenTelemetry, [`observer/otel`](https://github.com/danmestas/go-libfossil/tree/main/observer/otel/) is a separate Go
module so the core doesn't pull in the OTel SDK. Users import it
explicitly when they want traces, metrics, or structured logs.

See [Extension Points](/docs/guides/extension-points/) for the full observer
contract and a worked example of a custom observer.

## DST / simulation

`dst/` runs many Fossil nodes deterministically under a seed. A
`Simulator` (`dst/simulator.go`) owns a priority-queue event loop, a
`SimNetwork`, a `simio.SimClock`, and a set of `Node` actors. Scenarios
schedule clones, check-ins, merges, UV pushes, and partitions, and
tests assert invariants (`dst/invariants.go`, `crosslink_completeness_test.go`).

Fault injection has two forms. `simio.Buggify(p)` is a global
probability gate that returns `false` in production but flips `true`
with probability `p` once the simulator enables it; the blob and content
layers use it. The sync code instead asks a `BuggifyChecker` passed in
its options, at named sites such as `clone.emitCloneBatch.truncate` and
`handler.handleGimme.skip`; a nil checker injects nothing. `SeededBuggify`
in `dst` implements `BuggifyChecker` from a seeded random source.

See [Testing](/docs/guides/testing/) for running the DST matrix, seed sweeps,
and the driver test matrix.
