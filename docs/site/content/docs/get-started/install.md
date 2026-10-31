---
title: Install
weight: 10
---

go-libfossil is a Go module and a command. Neither needs cgo, a C toolchain or an installed `fossil`.

## The command

```sh
go install github.com/danmestas/go-libfossil/cmd/libfossil@latest
libfossil --help
```

This needs the Go toolchain named in `go.mod` (Go 1.26 or later). The binary is statically linked and runs on Linux, macOS and Windows. Every command is listed in the [CLI reference](/docs/reference/cli/).

## The library

```sh
go get github.com/danmestas/go-libfossil@latest
```

The library talks to SQLite through `database/sql` and does not pick a driver for you. Blank-import exactly one of the two driver modules in your `main` package; importing both panics at start-up.

| Driver | Import | Use it for |
|---|---|---|
| modernc | `_ "github.com/danmestas/go-libfossil/db/driver/modernc"` | servers, command-line tools, desktop: SQLite translated to Go |
| ncruces | `_ "github.com/danmestas/go-libfossil/db/driver/ncruces"` | `GOOS=js` and `GOOS=wasip1` builds: SQLite itself as WebAssembly |

Each driver is its own module, so `go get` it too:

```sh
go get github.com/danmestas/go-libfossil/db/driver/modernc@latest
```

## WebAssembly

With the ncruces driver the module builds for `GOOS=js GOARCH=wasm` (a browser) and `GOOS=wasip1 GOARCH=wasm` (any WASI Preview 1 host). CI builds the module's packages for `js/wasm` on every change. The [wasm example](https://github.com/danmestas/go-libfossil/tree/main/examples/wasm) builds and runs a WASI module.

### TinyGo

The whole public API, the HTTP sync client and signing included, also builds with [TinyGo](https://tinygo.org) for the `wasm` target, which is what makes it small enough to ship with a page or an edge function:

```
tinygo build -target=wasm -no-debug -opt=z -stack-size=1MB -o libfossil.wasm ./examples/tinygo/
wasm-opt -Oz --detect-features --strip-debug --strip-producers libfossil.wasm -o libfossil.wasm
```

- **Size:** for [`examples/tinygo`](https://github.com/danmestas/go-libfossil/tree/main/examples/tinygo), about 1.3 MB gzipped as CI builds it, against about 4.1 MB from the standard toolchain. A program without sync is smaller.
- **Driver:** a TinyGo build imports no SQLite driver. The host registers a `database/sql` driver over its own SQLite; ncruces is for standard Go builds and modernc has no wasm build.
- **Stack:** `-stack-size=1MB`, because TinyGo's default goroutine stack is too small for the deeper paths.
- **Differences:** under TinyGo a sync redirect ends the request with an error instead of being followed, and DNS failures are not classified as transient.
- **Without HTTP:** a program that serves sync through `HandleSyncWithOpts` and syncs, if at all, through a `Transport` its host supplies can build with `-tags libfossil_nohttp`. That leaves out `NewHTTPTransport`, `WithHTTPClient`, `Repo.Pull`, `Repo.XferHandler` and `Repo.ServeHTTP`, and with them `net/http`, whose TLS and X.509 dependencies are about 97 KB of a gzipped TinyGo binary. Everything else is unchanged. The tag is for library embedders: the `cli` package and `examples/tinygo`, which calls `NewHTTPTransport`, do not build with it. A tagged embedder supplies its own transport and serving, for example a `Transport` for `Sync` and its own request handling around `HandleSyncWithOpts`.
- **Without slog:** `-tags libfossil_noslog` leaves `log/slog` out, about 33 KB of a gzipped TinyGo binary. The check-in linker then writes its info and warning messages to stderr as plain `LEVEL message key=value` lines and drops its debug messages; nothing else changes. It combines with `libfossil_nohttp`.

CI builds `examples/tinygo` with TinyGo 0.43 on every change, and builds the library under both tags.

#### Smaller still

The [html-artifact-publisher](https://github.com/danmestas/html-artifact-publisher) engine, which serves Fossil repositories from Cloudflare Durable Objects, uses both tags and goes further. It is 754,725 bytes gzipped with TinyGo 0.43 and Go 1.27.3, where the same program built as above is about 1.2 MB; in the publisher's measurements on Go 1.26 it also ran its test workload about three times faster. Its [`fossil/build.sh`](https://github.com/danmestas/html-artifact-publisher/blob/main/fossil/build.sh) is the worked example. Beyond the tags it:

- builds with `-scheduler=none`, which drops TinyGo's asyncify pass, the largest single cost;
- builds as a reactor (`-buildmode=c-shared`) and exports its entry point with `//go:wasmexport`, because a `js.FuncOf` callback cannot run without a scheduler;
- adds `-panic-unwind=explicit`, so `recover` still works without asyncify;
- compiles against a patched copy of the Go standard library ([`fossil/goroot.patch`](https://github.com/danmestas/html-artifact-publisher/blob/main/fossil/goroot.patch)) that removes the goroutines `database/sql`, `context` and `time.AfterFunc` start, since a build without a scheduler cannot run them. The patch changes behavior: `database/sql` opens connections on demand and does not watch contexts, a context derived from a custom parent does not inherit its cancellation, and an `AfterFunc` callback runs inline. It also leaves out the FIPS self-tests.

These steps are the publisher's, not the library's, and this repository's CI does not build that way. They hold for a program that, like the publisher's engine, opens repositories, commits and serves sync through `HandleSyncWithOpts`; other paths, such as the HTTP sync client, may start goroutines of their own that such a build cannot run.

## Next

- [Quickstart: the CLI](/docs/get-started/quickstart/)
- [Embed in Go](/docs/get-started/embed-in-go/)
