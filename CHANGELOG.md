# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.10.0] - 2026-10-03

### Changed

- `Checkout.Revert` keeps a never-committed file on disk when it reverts the
  add (#234). It used to delete the file, destroying the only copy. It now
  un-manages it and leaves it in place, as fossil's `revert` does. `Revert`
  also checks the disk first, so an edit that no scan has recorded yet is
  reverted instead of silently skipped; restores a file that is missing from
  disk; and, when reverting everything, drops a pending merge completely
  (merged-in versions and the merge record), as fossil does.
- `Checkout.Rename` refuses to move a file onto one that already exists on
  disk, as `fossil mv --hard` does, before changing anything. It used to
  overwrite it.

### Fixed

- `Checkout.Revert` undoes a pending rename, as fossil's `revert` does
  (#242). It used to leave a rename made with `fossil mv` in place, and
  restored a renamed, edited file's content under its new name. The file now
  goes back to its old name with its committed content, and either name can
  be given. When undoing a rename would land on a name another file now
  holds (a swap, a new file added under the old name, or an untracked file
  there), `Revert` refuses and changes nothing rather than overwrite it:
  go-libfossil has no undo copy to recover it from (#248). Naming several
  files reverts them as one: if one is refused, none is reverted.

- A dry-run `Extract` or `Update` no longer changes the checkout (#236). Both
  used to replace the checkout's file list and move its version to the
  target, losing pending adds, renames and merge state, even though no file
  was written. Switching versions with `Extract` now also ends any pending
  merge, as fossil's `checkout` does, so a forced switch can't leave a merge
  record behind to give the next commit a bogus merge parent; re-extracting
  the current version without `Force` keeps it. A forced `Extract` starts the
  checkout's file list fresh, as `fossil checkout --force` does, so a pending
  rename or removal no longer survives it.
- A database fault while checking whether content is present no longer reads
  as "content missing" where missing content is tolerated (#239). Since #231,
  `CreateCheckout` on a partly synced tip leaves the checkout's file list
  empty instead of failing; a busy or failed database during that check was
  treated the same way, so `Create` quietly succeeded with an empty checkout.
  The fault is now reported. New `content.CheckAvailableByUUID` returns the
  error; the existing boolean checks still treat one as unavailable.

- The CLI's `add`, `rm`, `rename` and `revert` commands call the library
  instead of writing the checkout database with raw SQL (#234). `rename` now
  moves the file, like `fossil mv --hard`, instead of leaving it under its old
  name where the next commit couldn't find it. `revert` restores committed
  content instead of only clearing change flags. Errors, including from
  closing the checkout, are reported instead of ignored. Without `-R` they
  find the repository the checkout records, including a relative path as
  `fossil open` stores it.

- The internal stash no longer drops files changed by a merge (#233). It
  selected only `chnged=1`, leaving out fossil's merge states, and stashed a
  rename as a plain edit. It now selects changes the same way `Status` does,
  and refuses a pending merge, a permission change or a rename by name, since
  it can't yet stash those faithfully. The stash isn't reachable through the public API yet.
  Checkin and stash now read the checkout through one internal package,
  `internal/vfile`, which is the only place fossil's checkout encoding is
  interpreted.

- `Status` reports tracked files that are gone from disk as `missing`, as
  fossil's `changes` command reports MISSING (#232). They used to be left out,
  or shown as `added` or `modified`. `HasChanges` now sees a pending add made
  by the fossil binary without a scan first. Both now share one classifier,
  so they can no longer disagree about what counts as a change, and Status
  lists files by name. A directory where a tracked file belongs is reported
  as `missing` rather than failing the scan. A checkout written by an older
  go-libfossil may still hold `rid=0` rows for content that was missing from
  the repository (see #231); `HasChanges` now counts those, and `Extract` of
  a complete version replaces them.

- Checking out a version whose content is partly missing, as a partial sync
  leaves it, is now refused up front with an error naming the files (#231).
  go-libfossil used to record such files with `rid=0`, which in fossil's
  checkout format means "added", and `Extract` then panicked trying to read
  them. This matches fossil, which refuses with "missing content". A
  `CreateCheckout` whose tip is still partly synced no longer loads the tip's
  files, so extracting an older, complete version still works.

- A non-forced `Extract` now checks before it changes anything (#230). It
  used to replace the checkout's file list first, so a refusal lost pending
  adds, renames and merge state and left `Status` seeing an empty checkout.
  It also compared disk with the target version, so a clean checkout could
  not switch to a version that changed a file. Extract now follows fossil's
  `checkout`: it refuses while the current version has unsaved changes that
  the switch would lose, or when an untracked file sits where the target
  would write different content. A tracked file that is missing from disk no
  longer blocks a switch, as in fossil.

- Reading a stored artifact could return compressed bytes (#235). `blob.Load`
  treated a row as stored uncompressed whenever its stored length equalled
  `blob.size`. For a delta row those are different things (the compressed
  delta and the full artifact's length) and can be equal by chance, so the
  delta step then failed. Fossil always inflates `blob.content`, and so does
  go-libfossil now. Go 1.27's new compressor made one test fixture hit the
  case, but any repository could.

- Checkouts created by the fossil binary (`fossil open`) now work through
  `OpenCheckout` (#228). `Status` failed outright with `converting NULL to
  string` on `vfile.mhash`, because go-libfossil judged every file against
  that column, which fossil only fills while a merge is pending. Change
  detection now follows fossil's own scan: a file is compared with the
  artifact `vfile.mrid` names, adds and merge states move through the same
  `chnged` transitions, and fossil adds, renames and `origname==pathname`
  rows are classified the way fossil classifies them.

- A non-forced `Extract` no longer overwrites uncommitted work in a
  fossil-made checkout. The local-edit guard swallowed the same NULL scan
  error and skipped its check, and it never considered a merge that fossil
  had applied but not yet committed; both now refuse unless `Force` is set.

- `Add` writes `vfile.islink`, which fossil's checkout schema declares with
  no default, so a file added by go-libfossil to a fossil-made checkout
  reads back cleanly in both tools. New rows no longer populate `mhash`.

## [0.9.0] - 2026-08-07

### Fixed

- New artifacts are named with the algorithm the repository's `hash-policy`
  selects, rather than always SHA1. Committing into a repository whose policy
  is sha3 -- the `fossil init` default since 2.10 -- wrote SHA1 artifact IDs,
  so every F-card in the new manifest carried a differently-derived hash from
  the one already in the parent manifest and `fossil diff` reported every
  tracked file as CHANGED even when one file had been edited. Repository
  integrity was never affected and canonical fossil could always read the
  result; the damage was unreadable diffs and a permanently mixed-hash
  repository, plus artifacts a `shun-sha1` peer would reject.

  Content already stored under the other algorithm's name is reused rather
  than stored a second time, unless the policy forbids it (`sha3-only`,
  `shun-sha1`). Without that, a repository holding SHA1 artifacts under a
  sha3 policy would churn every F-card on its next commit -- the same
  every-file-CHANGED symptom, mirrored. Only a stored blob qualifies: a
  phantom is a name the repository knows of but has no content for.

  `checkout.Manage` and `uv.Write` read the same policy instead of each
  guessing from the width of an existing name. `uv.Write`'s guess read
  `project-code`, which is 40 hex characters in every repository whatever the
  policy, so it could only ever choose SHA1.

- Repositories created by this library are seeded with `hash-policy`, so they
  name artifacts the way `fossil init` does. Without the row, naming derived
  the algorithm from the repository's own artifacts, and a repository with no
  artifacts yet settled on SHA1 -- then stayed there permanently, because the
  first artifact it wrote was SHA1 too.

  A clone is the exception and keeps no seeded policy: it adopts its source's,
  which the absent row expresses by deriving the algorithm from the artifacts
  the clone actually receives. Canonical fossil propagates the source's policy
  value into a clone; the sync protocol carries no configuration, so this
  reaches the same answer by inference. The two differ only for a source
  explicitly set to `sha1` that nonetheless holds a SHA3 artifact.

### Added

- `db.HashPolicySHA1`, `db.HashPolicyAuto`, `db.HashPolicySHA3`,
  `db.HashPolicySHA3Only` and `db.HashPolicyShunSHA1` name Fossil's
  `hash-policy` values, so the seed path and the naming path read one
  definition instead of each carrying its own copy of the enum.

### Changed

- Repositories created by this library now name their artifacts SHA3 rather
  than SHA1. No API changed, but the same content committed into a newly
  created repository produces a different artifact ID than it did in 0.8.1.
  Existing repositories are unaffected: their artifacts keep their names, and
  their own `hash-policy` continues to decide how new ones are named.

## [0.8.1] - 2026-07-30

### Fixed

- Crosslink reports the underlying cause when it fails, instead of replacing
  it with a generic message.
- Test fossil servers are reaped before their temporary directories are
  removed, and the unit-test timeout was raised; leftover servers holding
  files open made teardown flaky.

### Changed

- Generated and local-only files are no longer tracked.

## [0.8.0] - 2026-07-29

### Fixed

- `mlink`, `plink`, `tagxref` and `leaf` are derived the way canonical fossil
  derives them, in both the sweep and the commit path, including deriving
  `leaf` by canonical's branch rule. mlink derivation, previously duplicated,
  is now unified in one place.
- Tag propagation stops at branch boundaries instead of crossing them, and the
  three tag-apply paths share one propagation helper.
- Authenticated sync interoperates with canonical fossil.
- Clone retries a dropped exchange round instead of aborting.
- The project code is derived, and artifact visibility is written inside the
  storing transaction rather than after it.
- Ticket artifacts are crosslinked, and ordinary file blobs no longer emit a
  warning.
- Checkins cascade-linked while referencing unavailable blobs are deferred
  until those blobs arrive.
- Submodules are pinned to v0.7.0 rather than an orphaned pseudo-version.

### Added

- The CLI selects its SQLite driver by build tag.

### Performance

- Clone no longer re-seeks `blob.uuid` per F-card, and no longer over-allocates
  delta output.
- The post-sync crosslink sweep no longer re-examines the whole repository.
- Content received during sync is reused in crosslink instead of being read
  back.

## [0.7.0] - 2026-07-27

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

- The xfer HTTP server now sends an explicit `Content-Length` on every clone
  and sync reply. The full response is already materialised by `Encode` before
  the first byte is written, so its exact length is known for free; setting the
  header keeps `net/http` from silently switching to `Transfer-Encoding:
  chunked` once the buffered body passes its internal flush threshold.
  Release-tagged fossil ≤2.23 reads the reply length only from `Content-Length`
  (`src/http.c` leaves its `iLength` negative otherwise) and aborts with
  "server did not reply" on any chunked reply above ~256KB, so those clients
  could not clone a repository larger than that from a libfossil server. No
  buffering was introduced — the response was always fully in memory — so
  #88's per-clone bound (`DefaultCloneBatchBytes`) still caps each reply. The
  fix is unconditional rather than negotiated: because the server never streams,
  chunked framing offers no memory saving over `Content-Length`, and
  `Content-Length` is what canonical fossil's own server emits, so it is
  strictly the more compatible framing for every client (issue #101).
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
  type explicitly, matching what they always emit. The HTTP server rejects a
  request body whose Content-Type is absent or neither §4 type with 415
  Unsupported Media Type instead of decoding it as plain card text: bodies come
  from untrusted peers, and without a framing signal decoding either way is a
  guess. Real fossil always sets the header, so no conforming peer is affected.
  (#106)
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
- Clone deadline now interrupts the mid-round phantom-fill crosslink cascade,
  ensuring the clone does not stall with phantoms outstanding. The same deadline
  is now threaded into the general push/pull phantom-fill cascade for consistency.
- Sync replies are now decoded by their wire's real `Content-Type` instead of a
  hardcoded assumption. The HTTP server sends the correct type on each response;
  call sites receiving frames without a header (the byte-oriented NATS transport)
  pass the type explicitly.
- PGP/SSH clearsigned manifests are now accepted: the deck module strips the
  clearsign framing before verification.
- Manifest now emits delete rows for files omitted by the check-in, matching
  fossil's own behavior. Renames now resolve their parent blob from the
  pre-rename path to ensure the rename-source blob exists and is reachable.

### Changed

- **Breaking:** The module path has changed from `github.com/danmestas/libfossil`
  to `github.com/danmestas/go-libfossil`. All `replace` directives in `go.mod`
  have been dropped. Consumers must update their imports and remove any
  `replace` directives pointing to the old path.
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
- Performance improvements throughout the sync and manifest subsystems: the
  phantom-fill crosslink cascade now batches onto a shared content cache,
  `db.Querier` caches prepared statements to stop per-call SQL re-parse overhead,
  and delta-chain walks bound their hash-collision chain to prevent quadratic
  behavior on low-entropy input. History-walking expansions are routed through
  the content cache, cutting sub-64KiB delta-chain expansion allocation ~18x
  and bounding `Apply` growth per-command to cut ~5x allocation overhead.

## [0.6.3] - 2026-05-13

### Fixed

- The SQLite DSN now sets `_txlock=immediate`, so writers serialize at `BEGIN`
  rather than discovering the conflict partway through a transaction (#33).

## [0.6.2] - 2026-05-12

### Fixed

- `Repo.Commit` now preserves the parent check-in's tracked files, instead of
  dropping files the commit did not itself touch (#30).

## [0.6.1] - 2026-05-12

### Added

- `CreateOpts.ProjectCode` accepts an explicit project code when creating a
  repository, rather than always generating one (#31).

## [0.6.0] - 2026-05-05

### Fixed

- The WAL is checkpointed on `Close`, so a repository written by this library is
  readable by canonical fossil without a recovery pass (#28).

## [0.5.0] - 2026-05-04

- Added support for whole-checkin diff via empty filePath parameter.
- Added `Repo.UUIDFromRID` for public rid-to-uuid lookup.
- Added cross-repo release automation specification and implementation.
- Added release workflow with downstream dispatch for automated releases.

## [0.4.5] - 2026-04-30

### Fixed

- A clone against a hub that is being written to concurrently now converges,
  instead of looping without making progress (#17).

## [0.4.4] - 2026-04-27

### Fixed

- A partial xfer no longer panics with `rid=0`; the incomplete transfer is
  handled rather than dereferenced (#14).

## [0.4.3] - 2026-04-26

- Stabilized xfer encoder and multi-round sync with deferred manifest crosslink.
- Added `libfossil version` command to print build identifier including module
  version, Go toolchain version, and platform.
- Wired live documentation URL into README and hugo baseURL.
- Added SVG architecture diagram and SDK refresh.
- Launched documentation site and Cloudflare auto-deploy.

## [0.4.2] - 2026-04-26

- Fixed manifest crosslink deferral to handle referenced blobs arriving later.

## [0.4.1] - 2026-04-25

- Implemented xfer encoder and multi-round sync foundation with manifest crosslink.

## [0.4.0] - 2026-04-25

- Added `Repo.Pull` public wrapper with HTTP transport and hostile-input assertions.
- Added Tiger Style test coverage for `Checkout.Update`.
- Improved documentation and test coverage for `Repo.Pull`.

## [0.3.0] - 2026-04-21

- Added `Repo.ReadFile` for reading tracked file content from the repository at
  any version.

## [0.2.0] - 2026-04-20

- Added `Repo.Merge` for branch-to-branch 3-way merge operations.
- Added `Repo.Diff` for computing diffs between tree states.

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
