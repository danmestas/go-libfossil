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

## Next

- [Quickstart: the CLI](/docs/get-started/quickstart/)
- [Embed in Go](/docs/get-started/embed-in-go/)
