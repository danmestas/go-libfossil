# wasm (WASI Preview 1)

Minimal example of building `libfossil` for a generic WASI Preview 1
wasm target. The produced `.wasm` binary is not tied to any specific
runtime and is intended to run under `wasmtime`, `wazero`, or any other
WASI P1 host.

## Driver choice

The default `modernc.org/sqlite` driver pulls in syscalls (`pthread`,
`signal`, `stdio`, `sys/types`, `unistd`, ...) that have no Go files
under `GOOS=wasip1`, so `_ "github.com/danmestas/go-libfossil/db/driver/modernc"`
will not compile for wasip1. This example blank-imports the
`ncruces` driver instead:

```go
_ "github.com/danmestas/go-libfossil/db/driver/ncruces"
```

`ncruces/go-sqlite3` ships SQLite itself as WebAssembly and is the
supported driver for `wasip1` and browser (`js`) builds.

## Build

From the repo root:

```
GOOS=wasip1 GOARCH=wasm go build -o libfossil-demo.wasm ./examples/wasm/
```

The output is a generic WASI P1 module (`~22 MiB`) with no runtime-specific
imports.

## Run

### wasmtime

`wasmtime` requires an explicit `--dir` map so the guest can see a
writable directory. The simplest form maps the current host directory
to the guest root:

```
wasmtime run --dir=.::/ libfossil-demo.wasm
```

`--dir=.` alone works in newer wasmtime releases but some versions
require the explicit `HOST::GUEST` form shown above.

### wazero (CLI)

```
wazero run -mount .:/ libfossil-demo.wasm
```

## What the example demonstrates

- Registering the `ncruces` driver via a blank import.
- `libfossil.Create` to create a fresh `.fossil` file in the mapped
  directory.
- `libfossil.Open` to re-open it.
- `repo.Config("project-code")` to read a config row.
- `repo.Commit` to write one file.
- `repo.Timeline` and `repo.ListFiles` to read back the commit.

## Runtime limitations under WASI

WASI P1 does not expose POSIX / OFD / BSD file locking, which SQLite
relies on for concurrency control. `libfossil` detects `GOOS=wasip1`
at compile time and switches to a WAL-free, exclusive-locking pragma
profile (see `db/config_wasm.go`), but some combinations of host
runtime and SQLite VFS still surface I/O errors when `PRAGMA
journal_mode` is set through the DSN. If you hit
`sqlite3: invalid _pragma: sqlite3: disk I/O error` under your runtime,
the issue is in the SQLite VFS glue, not in this example — the build
itself is a clean WASI P1 artifact and loads fine under wasmtime /
wazero.

The example is designed for single-process, single-connection usage,
which is the supported mode for `libfossil` on WASI.
