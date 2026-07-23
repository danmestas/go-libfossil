# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `libfossil version` prints a single, stable, machine-parseable build
  identifier -- module version (or a `-ldflags -X
  github.com/danmestas/go-libfossil/cli.buildVersion=...` override for
  release builds), Go toolchain version, and platform -- and exits 0.
  Previously `version` was not a recognized command at all and fell
  through to the unrecognized-argument path. There is no global
  `--version` flag: `repo extract` already owns `--version` as a
  command-scoped flag (the source version to extract), and kong's global
  flags are visible in every subcommand's context, so a root-level
  `--version` would collide with it.
- `libfossil repo serve` CLI command wires the already-public
  `Repo.ServeHTTP` xfer HTTP server into the command surface. A repository
  created or cloned with this tool can now be served to a peer -- including
  a stock `fossil clone` -- without writing Go. Binds to `127.0.0.1:8080`
  by default; override with `--addr host:port`. Process interrupt
  (Ctrl-C/SIGTERM) cancels the same context `ServeHTTP` already shuts down
  on; no new shutdown mechanism was introduced.

### Fixed

- The message decoder now selects a body's framing from its §4 Content-Type
  rather than by trying each framing and seeing which parses. A body sent as
  `application/x-fossil` is decoded as the §4.1 compressed container; every
  other type, including `application/x-fossil-uncompressed` (used by clone-v3
  replies), is decoded as plain card text — the same rule canonical fossil
  applies. Trial-based dispatch made a decompression failure indistinguishable
  from "wrong framing"; a compressed body that now fails to inflate is reported
  as the fault it is and never re-fed to the card parser. The former unprefixed
  raw-zlib framing was removed: it was checked before the prefixed container on
  the belief that real fossil emits it, but a fossil 2.28 server never does —
  its pull replies are the prefixed container and its clone replies are plain
  text — so the framing was only ever right by accident. Removing it also
  retires the length-prefix/zlib-header aliasing guard, which existed solely to
  disambiguate the two compressed framings that trial-dispatch conflated.
  `xfer.Decode` now takes the Content-Type; call sites that carry no header
  (the byte-oriented NATS transport, both ends libfossil) pass the compressed
  type explicitly, matching what they always emit. (#106)
- A large artifact now clones regardless of its position within a clone round.
  The server charged its batch budget before each artifact and sent the one
  that crossed it whole, so a round carried `budget + one whole artifact` — and
  when ordinary sub-budget filler preceded a large artifact, that sum exceeded
  what the client could decode (`xfer.MaxDecompressedBytes`), failing the clone
  with a spurious "declared container length exceeds" error. The size at which
  a clone failed depended on incidental filler: 8–15 MB of filler ahead of a
  58.7 MB artifact all failed, while 17 MB passed by pushing the artifact into a
  round of its own. `emitCloneBatch` now flushes the round and sends an
  over-bound artifact alone when adding it would cross the client's decode
  bound, so the ceiling is a property of the artifact alone. The `2 * budget`
  compile-time guard now certifies what it appears to: filler plus one
  budget-sized artifact always fits in a decodable round. (#109)
- A clone from a libfossil server could fail with a card-syntax error
  (`decode card 16303: empty line after split`) naming a card the response
  never contained. The message decoder tried three body framings in order and
  treated any decompression failure as "not this framing", so a well-formed
  compressed body that exceeded the decoder's own size bound fell through to
  the uncompressed branch and the still-compressed bytes were parsed as card
  text — thousands of nonsense cards until one split into no fields. Worse than
  the misleading error, an oversize or truncated compressed body was *accepted*:
  the garbage cards decoded without error and the artifacts they carried failed
  their hash check downstream. A body recognizable as zlib is now decompressed
  or reported, never reparsed. The bound is also raised to 64 MiB, since it sat
  below what this implementation's own server emits in a single clone round —
  two libfossil peers could not reliably clone from each other even though each
  could clone from fossil. A compile-time guard keeps the bound at least twice
  the server's clone *batch budget*, so filler plus one budget-sized artifact
  always fits in a round the client can decode. (#104)
- Committing a file with zero-length content panicked in `blob.Store`,
  which treated an empty artifact -- a normal, well-known Fossil blob -- as
  invalid input; the panic then triggered `manifest.Checkin`'s postcondition
  defer, which re-panicked with an unrelated "manifestRid must be positive"
  message that masked the real cause. `blob.Store` now accepts zero-length
  (and nil) content, and `Checkin`'s defer re-panics with the original panic
  value instead of asserting a postcondition that was never going to hold
  mid-unwind (#68).
- A commit no longer silently carries a tracked file's last-committed
  content forward when that file has been deleted from disk without
  `Unmanage`/`Remove` -- it now fails clearly, naming the file, matching
  fossil's own behavior for a missing file that is in scope for the commit
  being made. A missing file outside the commit's scope (relevant only to
  an explicit `Enqueue`) is left untouched exactly as before (#79).

### Changed

- **Breaking:** `StatusOpts`, `MergeOpts`, and `CheckoutOpts` have been
  removed. No function anywhere accepted any of the three, and nothing
  constructed one: `Checkout.Status()` takes zero arguments, `Repo.Merge`
  takes positional string arguments, and `CheckoutOpts` had no construction
  site at all. A public type with no call site documents a capability that
  does not exist — a consumer reading `MergeOpts{Strategy: ...}` in the
  docs could reasonably conclude a strategy-selecting merge API exists; it
  never did. `Repo.Merge` and `Checkout.Status` keep their current
  signatures — an options-struct refactor for `Merge` was considered and
  declined. `CheckoutOpts.Force` was also a third same-named `Force` field
  in this package, alongside the already-removed `UpdateOpts.Force` and the
  real, fully-wired `ExtractOpts.Force`; removing it also removes that
  readability hazard. `ExtractOpts.Force` is unrelated and unaffected.
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
