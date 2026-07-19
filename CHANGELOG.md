# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **Breaking:** `CloneOpts.ProjectCode` and `CloneOpts.ServerCode` have been
  removed. Neither was ever wired to anything — `Clone` accepted both fields
  but never forwarded them to the internal clone path, so setting either had
  no effect and gave no error. The internal clone options struct has no
  matching fields at all, so wiring them would mean designing semantics
  first (is a caller-supplied code a validation assertion or an identity
  override?) and no caller has asked for that. `CloneResult.ProjectCode` and
  `CloneResult.ServerCode` are unrelated and unaffected — they continue to
  report both, populated from the remote via the clone protocol negotiation,
  which is the only place they meaningfully originate.
- **Breaking:** `UpdateOpts.Force` has been removed. It was never wired to
  anything — `Checkout.Update` accepted the field but never forwarded it
  to the internal update path, so setting it had no effect and gave no
  error. Deleting it is honest about what the API actually does; real
  forcing semantics for `Update` can be designed and added later as a
  new, deliberately-wired field if a caller needs them. `ExtractOpts.Force`
  is unrelated and unaffected — it is fully wired and unchanged.
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
- **Breaking:** `Repo.Timeline` orders by `(mtime DESC, rid DESC)`, a total
  order with rid as a true tie-break at exact mtime equality — a
  deliberate improvement over canonical fossil's bare `mtime DESC` with no
  tie-break, which can repeat or skip rows at a page boundary. Pagination
  uses a new opaque `Cursor` type: take one from a returned
  `LogEntry.Cursor` and pass it back as `TimelineOpts.After` to resume
  immediately after that entry. **`TimelineOpts.Before time.Time` and
  `TimelineOpts.After FslID` are deleted, not deprecated** — callers
  constructing either field will fail to compile. `Cursor`'s
  representation is intentionally hidden — it can only be obtained from a
  `LogEntry`, never built from a timestamp and a rid by hand, because a
  hand-built cursor derived from a rounded `time.Time` is not guaranteed
  to match its row exactly, which is what reintroduces skipped or
  duplicated rows at a page boundary in the first place.
- **Breaking:** `LogEntry` no longer has a `Cursor` field, and
  `Repo.Timeline` now returns `[]TimelineEntry` instead of `[]LogEntry`.
  `LogEntry` is `Repo.Ancestry`'s result type; its cursor was always the
  zero value, which is structurally identical to a legitimate Timeline
  first-page call — so feeding an `Ancestry` entry's cursor into
  `TimelineOpts.After` silently paginated from page one forever with no
  error, and documenting the field as invalid on `Ancestry` entries did
  nothing to stop it. `TimelineEntry` (which embeds `LogEntry` and adds
  `Cursor Cursor`) is now `Repo.Timeline`'s result type, so the cursor
  lives only on the type that has one to give. Callers that read
  `entry.Cursor`, `entry.UUID`, `entry.User`, etc. off a `Repo.Timeline`
  result need no source change — those fields are still reachable through
  `TimelineEntry`'s embedded `LogEntry`. Callers that stored a
  `Repo.Timeline` result as `[]LogEntry` (rather than letting `:=` infer
  the type, or using `[]TimelineEntry`) will fail to compile.

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
