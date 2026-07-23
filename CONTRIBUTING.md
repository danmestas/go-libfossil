# Contributing to libfossil

Thanks for your interest in `libfossil`, a pure-Go implementation of Fossil SCM. This
document covers everything you need to get a working checkout, run the tests, and
submit changes.

## Development setup

Requires Go 1.26 or newer.

```
git clone https://github.com/danmestas/go-libfossil
cd libfossil
make setup-hooks
```

The repository ships with a `go.work` file at the root, so the main module and the
submodules (`db/driver/modernc`, `db/driver/ncruces`, `observer/otel`) resolve each
other locally without any extra configuration. `make setup-hooks` points
`core.hooksPath` at `.githooks/` so the pre-commit hook runs on every commit. The
hook runs both SQLite drivers, `go vet`, the OTel submodule, and the CLI build in
roughly 45 seconds; skip it with `--no-verify` only in emergencies.

## Running tests

The main targets are:

- `make test` runs `go test ./...` against the default `modernc` SQLite driver.
- `make test-drivers` runs the full suite twice: once with `modernc` and once with
  `ncruces` via `go test -tags test_ncruces ./...`. Any change that touches the
  database layer should pass both.
- `make test-otel` runs the OTel observer submodule in isolation with
  `GOWORK=off` so it resolves against its own `go.mod` rather than the workspace.
- `make test-all` chains `test-drivers`, `test-otel`, and the CLI build. This is the
  target CI uses and the one to run before opening a pull request.

Deterministic simulation tests live under `./dst/...`. They are not excluded from
the default run but are slow - roughly a minute per driver - so expect `make
test-drivers` to take a few minutes end to end.

## Code layout

- Root module (`github.com/danmestas/go-libfossil`) holds the public API: `Repo`,
  checkout, sync, merge, and history helpers. See `fossil.go`, `repo.go`, and the
  `repo_*.go` files.
- `cli/` hosts the Kong-based command-line surface; the `libfossil` binary is
  assembled in `cmd/libfossil/`.
- `internal/` contains the implementation packages that back the public API -
  `sync`, `merge`, `checkout`, `manifest`, `content`, and friends. These are not
  part of the stable surface.
- `db/` is the SQLite abstraction plus two driver submodules: `db/driver/modernc`
  (the default, pure-Go) and `db/driver/ncruces` (WASM-based, selected with the
  `test_ncruces` build tag).
- `observer/otel/` is a separate Go module providing an optional OpenTelemetry
  observer; it lives out of the workspace path so apps can take it as a narrow
  dependency.
- `dst/` contains the deterministic simulation test harness and scenarios.

## Submitting changes

Keep pull requests small and focused on a single change. Before opening one:

- Make sure the pre-commit hook passes (equivalent to `make test-all`).
- If your change touches the database layer, verify it runs under both the
  `modernc` and `ncruces` drivers.
- New behavior needs tests. Bug fixes should include a regression test that fails
  without the fix.
- If you are touching fault-sensitive code (sync, merge, checkout, or anything
  that writes to disk), consider whether a BUGGIFY site is appropriate so DST can
  exercise the new path. See `docs/testing.md` for the conventions.

Commit messages should explain the "why" as well as the "what". Reference issue
numbers where relevant.

## Reporting issues

Please open issues on GitHub. Include your Go version (`go version`), operating
system, and minimal reproduction steps. If a specific `.fossil` repository
triggers the bug and it is not sensitive, attach a sample - it speeds up triage
considerably.
