# go-libfossil

[![Docs](https://img.shields.io/badge/docs-online-c63a0f)](https://libfossil-docs.daniel-mestas.workers.dev/)
[![Go Reference](https://pkg.go.dev/badge/github.com/danmestas/libfossil.svg)](https://pkg.go.dev/github.com/danmestas/libfossil)
[![Go Report Card](https://goreportcard.com/badge/github.com/danmestas/libfossil)](https://goreportcard.com/report/github.com/danmestas/libfossil)
[![Tests](https://github.com/danmestas/go-libfossil/actions/workflows/test.yml/badge.svg)](https://github.com/danmestas/go-libfossil/actions/workflows/test.yml)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)](https://go.dev)

Pure-Go library and CLI for [Fossil](https://fossil-scm.org) repositories. Drop-in compatible with Fossil's `.fossil` SQLite format, with no CGo and no upstream `fossil` binary required.

> The repository is named **go-libfossil**, but the module is imported as `libfossil` (`go get github.com/danmestas/libfossil`) and the CLI installs as the `libfossil` command — the import path is unchanged.

## Why libfossil

- **Pure Go, no CGo.** Static binaries, trivial cross-compilation, works under `GOOS=wasip1 GOARCH=wasm`.
- **Drop-in compatible.** Repositories produced by upstream Fossil are readable, and vice versa — it's the same SQLite schema.
- **Embeddable.** Use the full `Repo` API directly in your Go service. No subprocess calls, no temp files.
- **Instrumented.** Sync and checkout observer interfaces make OpenTelemetry (or your own metrics) a few lines of code.
- **Deterministically testable.** Deterministic simulation testing (DST) with [BUGGIFY](./docs/site/content/docs/testing.md#buggify) fault injection.

## Installation

### CLI

```bash
go install github.com/danmestas/libfossil/cmd/libfossil@latest
```

The binary is named `libfossil` to avoid `$PATH` collisions with upstream `fossil`.

### Library

```bash
go get github.com/danmestas/libfossil
```

## CLI quick start

```bash
libfossil repo new my.fossil
libfossil -R my.fossil repo ci -m "first commit" hello.txt
libfossil -R my.fossil repo timeline
libfossil -R my.fossil repo ls
libfossil -R my.fossil repo annotate hello.txt
libfossil -R my.fossil repo verify
```

Full command list: `libfossil --help`. See [docs/migration-from-fossil.md](./docs/site/content/docs/migration-from-fossil.md) for a command-by-command comparison with upstream Fossil.

## Library quick start

```go
package main

import (
    "log"

    libfossil "github.com/danmestas/libfossil"
    _ "github.com/danmestas/libfossil/db/driver/modernc"
)

func main() {
    r, err := libfossil.Create("my.fossil", libfossil.CreateOpts{User: "admin"})
    if err != nil {
        log.Fatal(err)
    }
    defer r.Close()

    rid, uuid, err := r.Commit(libfossil.CommitOpts{
        Files:   []libfossil.FileToCommit{{Name: "hello.txt", Content: []byte("hello")}},
        Comment: "initial commit",
        User:    "admin",
    })
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("committed rid=%d uuid=%s", rid, uuid[:12])

    entries, err := r.Timeline(libfossil.LogOpts{Limit: 10})
    if err != nil {
        log.Fatal(err)
    }
    for _, e := range entries {
        log.Printf("  %s  %s", e.UUID[:12], e.Comment)
    }
}
```

Working example: [examples/embed-repo-api/](./examples/embed-repo-api/).

## Sync

```go
// Sync with a remote Fossil server.
result, err := r.Sync(ctx,
    libfossil.NewHTTPTransport("http://host/repo"),
    libfossil.SyncOpts{Push: true, Pull: true},
)

// Serve the sync protocol (e.g. for leaf agents, bridges, mirrors).
http.Handle("/", r.XferHandler())
```

The `Transport` interface is pluggable — swap HTTP for NATS, WebSocket, libp2p, or anything that delivers bytes round-trip.

## Embedding the CLI

`cli/` exposes [Kong](https://github.com/alecthomas/kong) command structs you can mount inside your own CLI:

```go
import (
    "github.com/alecthomas/kong"
    "github.com/danmestas/libfossil/cli"
    _ "github.com/danmestas/libfossil/db/driver/modernc"
)

type MyCLI struct {
    cli.Globals
    Repo cli.RepoCmd `cmd:"" help:"Fossil repo operations"`
    // Add your own commands here.
}

func main() {
    var c MyCLI
    ctx := kong.Parse(&c)
    ctx.FatalIfErrorf(ctx.Run(&c.Globals))
}
```

## Project layout

| Package | Purpose |
|---------|---------|
| root (`libfossil`) | `Repo` handle, `Transport`, `SyncObserver`, `CheckoutObserver` |
| `cli/` | Embeddable Kong command structs for the `repo` subcommands |
| `cmd/libfossil/` | Standalone CLI binary |
| `db/` | SQLite abstraction with pluggable drivers |
| `db/driver/modernc` | Pure-Go SQLite driver (default) |
| `db/driver/ncruces` | wasm-capable SQLite driver |
| `observer/otel/` | Optional OpenTelemetry observer (separate submodule) |
| `dst/` | Deterministic simulation tests + BUGGIFY harness |
| `simio/` | Clock, Rand, Storage interfaces for deterministic testing |
| `testutil/` | Shared test helpers |

Deep dive: [docs/architecture.md](./docs/site/content/docs/architecture.md).

## SQLite drivers

| Driver | Import | When to use |
|--------|--------|-------------|
| `modernc` (default) | `_ "github.com/danmestas/libfossil/db/driver/modernc"` | Default for any server, CLI, or desktop use |
| `ncruces` | `_ "github.com/danmestas/libfossil/db/driver/ncruces"` | wasm targets (`GOOS=wasip1` or browser/OPFS) |

Driver selection happens via blank import at link time. See [docs/extension-points.md](./docs/site/content/docs/extension-points.md#sqlite-driver-interface) for the registration contract.

## Observers

```go
// Built-in, zero dependencies.
libfossil.NopSyncObserver()     // silent
libfossil.StdoutSyncObserver()  // human-readable stderr

// Optional OpenTelemetry (separate submodule — no OTel deps in core).
import "github.com/danmestas/libfossil/observer/otel"
obs := otel.NewSyncObserver()
```

Both `SyncObserver` and `CheckoutObserver` are small interfaces you can implement directly. Details: [docs/extension-points.md](./docs/site/content/docs/extension-points.md#observer-interfaces).

## WASI / wasm

libfossil compiles to a generic WASI Preview 1 module via the `ncruces` driver:

```bash
GOOS=wasip1 GOARCH=wasm go build -o libfossil.wasm ./examples/wasm/
```

See [examples/wasm/](./examples/wasm/) for a runnable demo and current runtime caveats (the non-js ncruces driver variant has a known locking-mode quirk under wasmtime).

## Testing

- `make test` — unit tests with the default `modernc` driver
- `make test-drivers` — unit tests against both `modernc` and `ncruces`
- `make test-otel` — OTel submodule tests (runs with `GOWORK=off`)
- `make test-all` — everything, including the binary build
- `make setup-hooks` — install the pre-commit hook (~45s/commit)

Test strategy — unit tests, dual-driver matrix, and deterministic simulation (DST) with BUGGIFY fault injection: [docs/testing.md](./docs/site/content/docs/testing.md).

## Documentation

Full documentation: **<https://libfossil-docs.daniel-mestas.workers.dev>**.

Local preview: `make docs-serve`, then visit http://localhost:1313/.

Markdown sources: [`docs/site/content/`](./docs/site/content/).

| Doc | Audience |
|-----|----------|
| [docs/architecture.md](./docs/site/content/docs/architecture.md) | Contributors learning the codebase |
| [docs/testing.md](./docs/site/content/docs/testing.md) | Contributors writing or debugging tests |
| [docs/extension-points.md](./docs/site/content/docs/extension-points.md) | Consumers adding observers or custom drivers |
| [docs/migration-from-fossil.md](./docs/site/content/docs/migration-from-fossil.md) | Users coming from upstream `fossil` |
| [CONTRIBUTING.md](./CONTRIBUTING.md) | Anyone submitting a PR |
| [CHANGELOG.md](./CHANGELOG.md) | Release notes |

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md).

## Acknowledgements

This project stands on the work of others.

- **[Stephan Beal](https://wanderinghorse.net)** — author of **[libfossil](https://fossil.wanderinghorse.net/r/libfossil)**, the C library that gives this project its name. This is an *indirect* Go port: it follows the shape and intent of Stephan's libfossil rather than transliterating it, and leans on the original as a reference for how a Fossil library ought to be organized.
- **[Dr. D. Richard Hipp](https://www.hwaci.com/drh/)** and the **[Fossil SCM](https://fossil-scm.org)** project — the [Fossil source code](https://fossil-scm.org) was the implementation guide of record. Where this library and upstream Fossil had to agree on the bytes — SQLite schema, manifest format, delta and sync wire formats — Fossil's source was the authority.
- **[TigerBeetle](https://tigerbeetle.com)** and **[TigerStyle](https://github.com/tigerbeetle/tigerbeetle/blob/main/docs/TIGER_STYLE.md)** — the testing philosophy: assertions everywhere, bounded everything, and treating tests as a first-class engineering surface.
- **[John Ousterhout](https://web.stanford.edu/~ouster/)** and **[*A Philosophy of Software Design*](https://web.stanford.edu/~ouster/cgi-bin/aposd.php)** — the engineering principles for keeping modules deep and interfaces narrow as the codebase grew.
- **[Antithesis](https://antithesis.com)** and **[deterministic simulation testing](https://antithesis.com/docs/resources/deterministic_simulation_testing/)** — the model behind this project's own DST harness, where time, randomness, and faults are controlled so that every failure is perfectly reproducible.

## License

MIT — see [LICENSE](./LICENSE).
