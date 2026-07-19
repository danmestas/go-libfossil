# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **Breaking:** `Checkout.Update` now returns `(UpdateResult, error)` instead
  of a bare `error`. The internal 3-way merge already tracked which files
  were written, removed, and left with conflict markers; the old signature
  discarded all of it, so a caller could not tell a clean update from one
  that silently wrote `<<<<<<<` conflict markers into working-tree files.
  `UpdateResult.Conflicted` lists the paths that were merged but not
  cleanly — this is a successful update (`err == nil`), not an error; a
  genuine failure still returns a non-nil `error` with a zero-value
  `UpdateResult`. `Checkout.Extract` is unchanged.
- **Breaking:** `Repo.Timeline` now enumerates the repository's `event`
  table newest-first (every event kind by default, or a single kind via
  `TimelineOpts.Type`), matching canonical `fossil timeline`. The previous
  behavior — a first-parent walk from a required start rid — is preserved
  under a new, honestly-named method, `Repo.Ancestry(LogOpts)`. The old
  `Repo.Timeline(LogOpts)` was actually an ancestry walk masquerading as an
  enumeration: it never visited a second parent or a sibling branch head,
  so it silently omitted any check-in that wasn't a first-parent ancestor
  of the given start rid. There is no deprecated shim; callers of the old
  `Timeline` should switch to `Ancestry` if they want the walk, or adopt
  the new `Timeline(TimelineOpts)` if they want a full enumeration.
- `LogEntry` gains a `Kind` field (`EventKind`) identifying which of
  `event.type`'s six kinds (`ci`, `e`, `f`, `g`, `t`, `w`) an entry is.
  `Parents` is only populated for `Kind == EventKindCheckin`.
- `Repo.Timeline`'s pagination cursor (`TimelineOpts.Before`/`After`) orders
  by `(mtime DESC, rid DESC)`, a deliberate improvement over canonical
  fossil's bare `mtime DESC` with no tie-break, which can repeat or skip
  rows at a page boundary.

## [0.1.0] - 2026-04-20

Initial open-source release of `libfossil`, a pure-Go implementation of the
Fossil SCM that reads and writes the same `.fossil` SQLite repository format.

### Added

- Repository lifecycle: create new repos and clone from existing ones.
- Working-tree operations: checkout and checkin.
- Timeline traversal over commits and events.
- Merge and rebase primitives.
- Diff and annotate (blame) over tracked content.
- Manifest parsing and content-addressed blob storage.
- Sync protocol client/server for pulling and pushing between repos.
- Observer interfaces for sync and checkout, allowing external hooks into
  both network sync events and working-tree state transitions.
- SQLite driver abstraction with support for both `modernc.org/sqlite` (pure
  Go) and `ncruces/go-sqlite3` (cgo-free, wasm-based) backends.
- Deterministic simulation test harness with BUGGIFY-style fault injection
  for exercising concurrency and failure paths.
- OpenTelemetry observer provided as a separate submodule to keep the core
  dependency footprint small.
- `wasip1/wasm` build target for running `libfossil` under WASI runtimes.
