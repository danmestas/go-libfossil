# tinygo (js/wasm)

The whole public API, including the HTTP sync client and signing, compiled
with [TinyGo](https://tinygo.org) for the `wasm` (js) target.

```
tinygo build -target=wasm -no-debug -opt=z -stack-size=1MB -o libfossil.wasm ./examples/tinygo/
wasm-opt -Oz --detect-features --strip-debug --strip-producers libfossil.wasm -o libfossil.wasm
```

`-stack-size=1MB`: TinyGo's default goroutine stack is too small for
go-libfossil's deeper paths.

Measured on TinyGo 0.43.0 (Go 1.27.3, LLVM 22) and Binaryen 133, for this
program:

| Build | Raw | gzip -9 |
|---|---|---|
| TinyGo, then `wasm-opt -Oz` | 3.83 MB | 1.40 MB |
| TinyGo | 4.02 MB | 1.44 MB |
| Standard Go, `GOOS=js GOARCH=wasm -ldflags='-s -w'` | 16.1 MB | 4.06 MB |

A program that leaves out sync (no `net/http`, no TLS) is smaller.

No SQLite driver is imported here. A TinyGo host registers a
`database/sql` driver over its own SQLite. The `ncruces` driver runs SQLite
as an embedded wasm module and is for standard Go builds; the `modernc`
driver has no wasm build.

Under TinyGo two things behave differently, because TinyGo's standard
library lacks what they use: a sync redirect ends the request with an error
instead of being followed, and DNS failures are not classified as transient.
