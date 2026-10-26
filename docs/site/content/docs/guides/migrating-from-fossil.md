---
title: Migrating from fossil
weight: 20
aliases: ["/docs/migration-from-fossil/"]
---

This guide is for users of upstream [Fossil SCM](https://fossil-scm.org) (the C
binary distributed by `fossil-scm.org`) who are evaluating `libfossil` — a
pure-Go implementation of the Fossil repository format and sync protocol.

It is intentionally honest about gaps. If a capability is not listed under
"Command coverage" below, assume it is not implemented yet.

For a structural overview of how libfossil is organised — the package layout, storage model, and pluggable axes — see [Architecture](/docs/concepts/architecture/).

## Repo format compatibility

`libfossil` reads and writes the same on-disk `.fossil` SQLite repository
format as upstream Fossil. A repository created by either tool can be opened
by the other at the file level — there is no import/export step and no data
migration is needed. Both implementations share the same blob table layout,
manifest/control artifact encoding, and delta chain representation.

One caveat: features introduced in very recent upstream Fossil releases
(bleeding-edge artifact card types, new Fossil-specific SQL functions used in
triggers) may not yet be parsed by `libfossil`. The tests that compare
against stock fossil run against the pinned release, 2.28. See
[Conformance](/docs/reference/conformance/) and [Testing](/docs/guides/testing/).

## Binary name and invocation

The compiled binary is named `libfossil` (not `fossil`) to avoid `$PATH`
collisions when both tools are installed on the same machine:

```
go install github.com/danmestas/go-libfossil/cmd/libfossil@latest
```

`libfossil` uses a subcommand-grouped syntax: every command lives under the `repo` group,
and individual operations live beneath it. Upstream Fossil uses a flat
command style. Examples:

| Upstream Fossil              | libfossil                              |
|------------------------------|----------------------------------------|
| `fossil clone URL repo.fossil` | `libfossil repo clone URL repo.fossil` |
| `fossil ci -m "msg" file.txt`  | `libfossil -R repo.fossil repo ci -m "msg" file.txt` |
| `fossil timeline`              | `libfossil -R repo.fossil repo timeline` |

The `-R <path>` flag selects the repository and is a global flag (before the
`repo` subcommand group).

## Command coverage

The following `repo` subcommands are implemented today. Every entry below
corresponds to a registered Kong command in `cli/repo.go` in the libfossil
source tree. `libfossil --help` lists them all.

### Repository creation

- `repo new` — create a new, empty repository file.
- `repo clone` — clone a remote Fossil server over HTTP(S). With `--cone`
  it clones a sparse store; with `--structural` it derives branch and tag
  state from the origin's horizon seed; `--seed-key` pins the key the seed
  must be signed by. See [Sparse cones](/docs/concepts/sparse-cones/) and
  [Horizons](/docs/concepts/horizons/).

### Working tree and checkout

- `repo open` — materialize a checkout in a directory (creates `.fslckout`).
- `repo co` — check out a named version.
- `repo ls` — list files in a version.
- `repo status` — show pending changes in the working directory.
- `repo add` — stage files for addition.
- `repo rm` — stage files for removal.
- `repo rename` — rename a tracked file.
- `repo revert` — undo staging changes.
- `repo ci` — commit the checkout's changes, or only the named files,
  with any pending merge as a merge parent. `--nosign` stores one check-in
  unsigned; see [Signing](/docs/concepts/signing/).
- `repo cone` — show or edit the checkout's sparse cone (`show`, `add`,
  `remove`, `set`, `dense`, `test`) and the repository's store cone
  (`store`, `deepen`, `compact`, `cost`).

### History and inspection

- `repo timeline` — repository history (`-n` sets the count, default 20).
- `repo log --path` — the check-ins that changed a path (`fossil finfo`).
- `repo snapshot` — commit one file's bytes onto trunk without a checkout;
  see [Embedding the CLI](/docs/guides/cli-embedding/).
- `repo diff` — show changes vs. a version.
- `repo annotate` / `repo blame` — annotate file lines with commit history.
- `repo cat` — output artifact content.
- `repo hash` — hash files (SHA1 or SHA3-256).
- `repo info` — repository statistics.
- `repo resolve` — resolve a symbolic name to a UUID.
- `repo schema` — operate on the synced table schema.
- `repo query` — run ad-hoc SQL against the repository DB.

### Branching and merging

- `repo branch` — `ls`, `new` and `close`.
- `repo tag` — `ls` and `add`.
- `repo merge` — merge a divergent version into the current checkout.
- `repo conflicts` — `ls` lists files holding merge conflict markers and
  conflict-fork entries; `show`, `pick`, `merge` and `extract` inspect and
  resolve one conflicted file.
- `repo mark-resolved` — resolve a conflict-fork entry. A merge conflict
  needs no marking: as in fossil, editing the markers out resolves it.
- `repo bisect` — binary-search commits for bug introductions (`good`,
  `bad`, `next`, `skip`, `reset`, `ls`, `status`).
- `repo undo` / `repo redo` — undo/redo the last operation.
- `repo stash` — `save`, `pop`, `apply`, `ls`, `drop` and `clear`.

### Collaboration and admin

- `repo user` — user management.
- `repo invite` — generate an invite token for a user.
- `repo config` — get/set/list repository configuration.
- `repo wiki` — wiki page operations.
- `repo uv` — unversioned file operations.
- `repo extract` — extract files from a version.
- `repo delta` — create/apply delta operations.
- `repo verify` — verify repository integrity.
- `repo serve` — serve the repository over HTTP for clone and sync
  (`-P host:port`, default `127.0.0.1:8080`). Access follows the user
  table. It is a sync endpoint, not a web UI.
- `repo submanifests` — `enable`, `retune`, `disable` and `status` for
  sub-manifest trees; see [Sub-manifests](/docs/concepts/sub-manifests/).

### Known gaps vs. upstream Fossil CLI

The following upstream commands have no `libfossil` equivalent today:

- `fossil ui` / `fossil http` — no web UI or HTTP admin server is shipped.
  `repo serve` answers sync and clone requests only; it is the counterpart
  of `fossil server` for the sync protocol and nothing else.
- `fossil sync` / `fossil push` / `fossil pull` — there is no CLI verb; sync
  is exposed via the Go API (`Repo.Sync`, `Repo.Pull`, `Repo.XferHandler`)
  but not as a subcommand.
- `fossil update` — merging a newer trunk into a checkout has no dedicated
  CLI command; use `repo co` plus `repo merge` as needed.
- `fossil search` — no CLI front-end for full-text search.
- `fossil fts-config` / FTS index management — no CLI command.

## What's not supported yet

Beyond the gaps listed above, several broader features of upstream Fossil are
not yet exposed by `libfossil`:

- **Web UI / HTTP admin** — no equivalent of `fossil ui`. The sync protocol
  handler exists as `Repo.XferHandler()` and `repo serve`, but the HTML UI,
  wiki viewer, ticket tracker, and forum are not implemented.
- **Interactive conflict resolution** — there is no interactive merge
  tool. `repo conflicts` lists, shows and resolves conflicts by picking a
  version or re-merging; otherwise edit out fossil's conflict markers in
  your editor.
- **Full-text search CLI** — `internal/search` contains indexing and query
  primitives, but no CLI command surfaces them. Use the Go API directly.
- **Ticket and forum subsystems** — no commands or pages. Ticket and forum
  artifacts are synced and stored like any other artifact. Tickets have a Go
  API: `Repo.TicketAdd` and `Repo.TicketSet` write the change artifact `fossil
  ticket add` and `fossil ticket set` write, and `Repo.TicketList` and
  `Repo.TicketChanges` read tickets back. Report formats, TH1 ticket scripts
  and `private_` fields are not supported.
- **Mirror / export bridges** to Git, svn, and other systems — not implemented.

If any of these are load-bearing for your workflow, stay on upstream Fossil
for now.

## Interop patterns

Where `libfossil` shines is in environments where the upstream C binary is
inconvenient to run:

- **CGo-free CI pipelines** — `libfossil` links statically with a pure-Go
  SQLite driver (`db/driver/modernc`), so clone/sync steps work on stripped
  container images and cross-compiled targets with no system dependencies.
- **Embedding in Go services** — the `Repo` handle can be used directly from
  a Go program to read history, extract artifacts, or accept pushes over a
  custom transport (WebSocket, NATS, libp2p).
- **WASI / browser deployments** — the `db/driver/ncruces` SQLite driver
  compiles to WebAssembly, enabling repo access from edge runtimes.

See [Examples](/docs/examples/) for runnable starting points. The
`cmd/libfossil` package is also a short working example: `main.go` wires the
CLI, while `driver_modernc.go` registers the default driver and
`driver_ncruces.go` shows the tagged WebAssembly-capable alternative.
