# go-libfossil

> A Fossil repository engine in Go without cgo: it reads, writes and syncs Fossil repositories (the artifact format, the derived tables and the sync protocol). Go module `github.com/danmestas/go-libfossil` (package `libfossil`), command `libfossil`. A repository is an ordinary `.fossil` SQLite file that stock fossil reads.

This file is generated from `docs/site/llms-header.md` plus the docs site. Every link below is a page on the docs site; [llms-full.txt](https://libfossil-docs.daniel-mestas.workers.dev/llms-full.txt) holds all pages in one file.

## Key concepts

- **Artifact**: a text manifest made of cards (check-in, tag, wiki, cluster, ...). Its name is the SHA1 or SHA3-256 hash of its bytes.
- **Repository state**: the tables (`event`, `plink`, `mlink`, `tagxref`, `leaf`, ...) derived from the artifacts by crosslinking, matching what `fossil rebuild` writes.
- **Sync**: Fossil's card protocol over HTTP (`igot`, `gimme`, `file`, ...), as a client (`Clone`, `Repo.Sync`, `Repo.Pull`) and a stateless server (`libfossil repo serve`).
- **Sparse cone**: a set of paths a checkout or a store holds; content outside it is elided, not lost.
- **Horizon**: a check-in below which a store leaves history on the origin; at the structural depth the clone verifies a seed the origin can sign.
- **Sub-manifests**: an extension that writes a check-in as a tree of manifests instead of one flat file list, shared with zit.
- **Driver**: the `database/sql` SQLite driver, chosen by blank import: `db/driver/modernc` (pure Go) or `db/driver/ncruces` (SQLite as WebAssembly, for js/wasm and wasip1).

## Quick start

1. `go install github.com/danmestas/go-libfossil/cmd/libfossil@latest`
2. `libfossil repo new notes.fossil`, then `libfossil -R notes.fossil repo open .`, `libfossil repo add README.md`, `libfossil repo ci -m "first notes"`.
3. From Go: blank-import a driver, then `libfossil.Create(path, libfossil.CreateOpts{User: "ana"})` or `libfossil.Open(path)`, and `r.Commit(libfossil.CommitOpts{...})`, `r.Timeline(libfossil.TimelineOpts{...})`.
4. Sync: `libfossil.Clone(ctx, path, libfossil.NewHTTPTransport(url), libfossil.CloneOpts{})`, then `r.Sync(ctx, t, libfossil.SyncOpts{Pull: true})`.

## Go packages

- `.` (package `libfossil`): `Repo`, `Create`, `Open`, `Clone`, check-ins, checkouts, history, merge, sync.
- `cli`: the Kong command structs behind `libfossil`, embeddable in another binary.
- `db`: the SQLite abstraction and driver registration.
- `db/driver/modernc`: pure-Go SQLite driver (separate module).
- `db/driver/ncruces`: SQLite-as-WebAssembly driver (separate module).
- `observer/otel`: OpenTelemetry observer (separate module).
- `dst`: deterministic simulation of sync between nodes.
- `simio`: clock, randomness and storage seams for deterministic tests.
