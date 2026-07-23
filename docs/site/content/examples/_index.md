---
title: Examples
weight: 60
---

Runnable examples live in the repo. Each is a self-contained `main.go` you can clone and run.

## embed-repo-api

Minimal example of embedding libfossil as a library in a Go application. Walks the basic repo lifecycle — register the default pure-Go SQLite driver via blank import, create a `.fossil` in a temp directory, close, re-open, write a commit, read the timeline, list files, clean up.

→ [`examples/embed-repo-api`](https://github.com/danmestas/go-libfossil/tree/main/examples/embed-repo-api)

## wasm

Building libfossil for a generic WASI Preview 1 target. The compiled `.wasm` is not tied to any specific runtime and runs under `wasmtime`, `wazero`, or any WASI P1 host. Uses the [`ncruces` driver](../docs/reference/sdk/db/driver/ncruces) — the default `modernc` driver pulls in syscalls (`pthread`, etc.) that WASI doesn't provide.

→ [`examples/wasm`](https://github.com/danmestas/go-libfossil/tree/main/examples/wasm)
