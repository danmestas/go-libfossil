# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Sparse stores (draft-fossil-sparse-cone-00 §5, Appendix A step 3). The
  new `Repo.SetStoreCone` and `Repo.StoreCone` methods set and read a
  store cone, kept in a `storecone` table that exists only while a cone
  is set. An artifact the store lacks is now classified as a transit
  phantom, a shunned artifact or an elided phantom, by zit's rule: a
  blob is elided when F cards name it only outside the cone and no held
  delta uses it as its base. Elision is computed from the cone and never
  stored. Changing the cone reclassifies; losing it, or a cone this
  version cannot read, makes every elided phantom a transit phantom, and
  the store heals toward dense. `fossil rebuild` drops the table, so it
  heals there too. Sync never requests an elided phantom, as a client or
  as a server. `verify.Report` gains `Phantoms` (transit) and `Elided`.
  Elision is never an issue, so a store complete within its cone
  verifies OK, and `Rebuild` keeps the cone. A commit over a sparse
  store has no R card and carries the files the store lacks by hash.
  `VerifyRCard` skips over a sparse store, and skips an absent R card. A
  checkout opens over a sparse store when its own cone stays inside the
  store's. Manifests, sub-manifest nodes and control artifacts are always
  held, as zit holds them. Cone-scoped transfer (Appendix A step 4):
  `Repo.CommitContext`, `Checkout.CheckinContext`, `Checkout.UpdateContext`
  and `Checkout.SetConeContext` take a `RequiredContentOpts` naming a peer
  and recover the sub-manifest closure a sparse store lacks for a commit,
  update or widening through an authenticated gimme exchange: in one
  transaction, never widening the store cone, refusing incomplete,
  corrupt, shunned, unauthorized-private or wrong-project content, and
  never fetching an outside F payload. `repo ci` and `repo cone
  add|remove|set` take `--remote`, `--sync-user`, `--sync-pass`,
  `--sync-private` and `--timeout` (default 10m); the peer defaults to the
  saved last-sync-url and no password is saved. A cone-aware server
  answers a cone request from a prepared index of what the cone keeps,
  visiting only the needed candidates over long excluded runs, with the
  dense path's private and shun notices and the same cards; it refuses to
  prepare beyond fixed stored, inflated and card budgets, and serves
  densely after any foreign commit until rebuilt. The server-side walk
  keeps the depth and visit bounds of single-name lookup (§8), and a
  cone-scoped clone round stops where a dense round would, charging the
  rows it skips, and resumes from the cursor. A server serves a store it
  holds sparsely only when its new `partial-mirror` setting
  (`SettingPartialMirror`, `repo config set partial-mirror on`) says so;
  otherwise it refuses, as §5.3 requires. `Repo.SetStoreConeContext` and
  `repo cone store [PATH... | --not | --dense]` replace the store cone and
  pull what the widening covers from the peer, the saved last-sync-url by
  default, refusing with the cone unchanged when there is no peer.
  `Repo.CompactStore` and `repo cone compact` drop the held blobs the store
  cone elides, as zit's `pack --drop-elided` does; sub-manifest nodes stay
  held, as zit holds every structural artifact. Widening a checkout cone
  over a sparse store fetches the payloads the widening covers from the
  peer and materializes them; with no peer the widening is refused and
  nothing changes. The private checkout marker moves crash-safely: the move
  is recorded in vvar before it starts and completed on the next open, so a
  crash at any step leaves a checkout fossil and go-libfossil both read.
  Checking out verifies the manifest's R card as fossil does, warning
  through the new `ExtractOpts.Warn` (`repo open` prints a WARNING line)
  and succeeding, skipped over a sparse store or with `repo-cksum` off.
- Sparse checkouts (draft-fossil-sparse-cone-00 §3 and §4). A cone is a
  set of paths, each a directory subtree or a single file, included or
  excluded; the longest member covering a path decides. `NewCone` builds
  one, normalized and serialized byte for byte as zit does;
  `CheckoutCreateOpts.Cone` makes a checkout sparse, and `Checkout.SetCone`
  widens, narrows or drops it (`libfossil repo open --cone/--cone-not`,
  `libfossil repo cone show|add|remove|set|dense|test`). A sparse checkout
  materializes, scans and tracks only the covered files, over flat and tree
  manifests; adding, removing, renaming or reverting an uncovered path is
  refused with `ErrOutsideCone`, naming the cone. A commit carries the
  uncovered files from its parent unchanged and is the same check-in a
  dense checkout makes of the same change. `Update` and `Merge` apply
  changes to uncovered files to the record without writing them, counted
  in `UpdateResult.OutOfCone`; the pending merge survives `Update` as
  fossil's does, a partial commit leaves record-only changes pending, and a
  partial commit of a pending merge is refused, as fossil refuses it.
  `Revert` and undo restore record-only rows too, and an undo image carries
  the cone it was saved under and is refused under another. The checkout
  stays readable by fossil, which sees the uncovered files as missing and
  will not commit until they are restored. New `sparsecone` and `horizon`
  conformance suites pin both drafts; with zit installed, a test checks the
  serialized cones, the materialized files and commits in both directions
  against zit, and its recorded answers are replayed without it.
- go-libfossil reads sub-manifest trees (draft-fossil-submanifests-00), the
  check-in form zit writes. It parses the S card and
  the SUBMANIFEST artifact type, and every reader of a check-in's file set
  -- `ListFiles`, `LookupFile`, checkout, diff, annotate, crosslink,
  rebuild, verify and commit -- walks a tree through one enumeration
  function: depth first, at most 16 levels, never presenting a tree it
  cannot walk in part. A delta manifest may name a tree as its baseline. A
  tree check-in whose sub-manifest has not arrived is deferred, with
  phantoms for its missing references, and linked when they arrive;
  `Verify` reports it as a missing reference, not damage. The new
  `submanifest` conformance suite covers 72 reader and writer requirements,
  the Appendix A vectors included, and a test reads trees zit wrote and,
  with zit installed, syncs with zit in both directions.
- go-libfossil writes sub-manifest trees when a repository opts in. Emission
  is off by default, and every commit stays byte-identical to before.
  `Repo.EnableSubmanifests(k)`, or `libfossil repo submanifests enable
  [--k N]`, turns it on. It keeps an existing pin of the partition
  parameter k and refuses a different one. With no pin it takes an explicit
  k from 1 to 12, or selects one as zit does: the draft's cost rule over the
  main-branch tip, refined by replaying recent history. It refuses, changing
  nothing, when no k partitions the file set. It returns the warning an
  operator must see, worded as zit's: peers that do not announce
  sub-manifest support are refused trees, trees cannot be retired, and the
  setting must not be enabled on a repository such a peer serves (§8.1).
  `Repo.RetuneSubmanifests(k)` (`retune --k N`) moves the
  pin for later commits, `Repo.DisableSubmanifests()` (`disable`) turns
  emission off again, leaving existing trees in place, and `status` shows
  the settings and whether the repository holds a tree. A commit then stores its leaves and interior
  sub-manifests, rebuilt in full and shared by content addressing, before a
  manifest that carries only S cards and never a B card. A stored k outside
  1..12 falls back to flat manifests with a warning. With
  `submanifest-auto-retune` on, a commit re-pins k when another value costs
  more than 15% less. A commit made in a sync session whose server sent
  `avoid-submanifests` stays flat. The construction reproduces every
  Appendix A vector, and a recorded set of zit check-ins byte for byte:
  3,000 files at k = 1, 3 and 5, deep paths, and names with spaces, tabs,
  newlines and renames. Enabling pins the k zit pins on the same history.
  With zit installed, zit verifies and lists go-libfossil's trees, and
  verifies a tree go-libfossil commits on top of one of zit's.
- Sync follows zit for trees: a peer that reads them says so with
  `pragma send-submanifests`, which go-libfossil sends in every request
  right after `client-version` and in every reply, clone replies included,
  right after `server-version`, whatever the sub-manifest settings. Once a
  repository holds a tree, a tree manifest or sub-manifest is refused to a
  peer that has not sent the pragma, whatever version it announces, with
  the error `Peer does not accept sub-manifests.`, so no fossil release
  receives one. A server decides from the request it answers, a client from
  the replies of its session. An artifact stored as a delta of a withheld
  tree goes to such a peer as full content. A server whose
  `enable-submanifests` is off sends `pragma avoid-submanifests` in the same
  leading run of every reply; it binds only a commit made inside the
  session, so commits, made outside any session, follow the repository's
  own setting, and a server stores a pushed tree whatever its setting. A
  clone asks once, in its cleanup round, for `enable-submanifests`,
  `submanifest-k` and `submanifest-auto-retune` with zit's
  `reqconfig /config`, applying all three or none; a pull asks for none.
  Enabling emission over a stored `submanifest-k` outside 1 to 12 is
  refused, naming it, unless an explicit k replaces it. The conformance
  suite pins the draft's 19 October 2026 revision, which applies the
  2026-10-19 errata and settles its sub-manifest deviations. Tracked in #452.
### Changed

- Tag propagation after a crosslink now starts only from the primary
  parents the batch wrote plink rows for, and from the new parents of a
  reparent, rather than from every primary parent in the repository.
  It runs in the transaction that wrote those rows, as fossil's crosslink
  propagates from a check-in's parent as it links it, so a link and its
  propagation commit or roll back together. The same transaction runs the
  reparent pass and applies the leaf rechecks it scheduled, as fossil's
  manifest_crosslink_end does: no committed link leaves scheduled rechecks
  pending. Neither a sweep nor a sync runs a redundant gate at its end,
  or a repair after a failed round. A rebuild links every check-in, so it
  propagates from every parent as before. Tracked in #432.
- A sync that fails after earlier rounds' phantom fills linked artifacts no
  longer runs a crosslink sweep before it returns. The artifacts earlier
  rounds committed outside a phantom fill stay unlinked until the next
  crosslink (the next sync, clone or `rebuild`), which links them. No tag or
  leaf state is lost: everything linked was repaired in its own transaction.

- The conformance suite no longer reads erratum files. zit applied every
  erratum in its draft text and retired `docs/rfcs/erratum/` at check-in
  `62a7d1bb3f`; each draft now records its corrections in its own Changes
  appendix, and the draft text is the authority. The `# erratum` pins and
  `conformance/rfcs/erratum/` are gone. The artifact-format, repo-state,
  sync-protocol and delta-format drafts are unchanged. The sub-manifest
  draft is re-pinned with only its expiry date and a reflow changed. The
  sparse-cone draft is re-pinned to its 19 October 2026 revision on zit's
  `horizon-rfc` branch, where §3.6 is removed and §3.4 states the literal
  prefix rule once: a metacharacter in a member is a literal character,
  never expanded, and the member may be refused. `SC-3.6-1` is replaced by
  `SC-3.4-3`, which the same case covers.
- `S` is no longer rejected as an undefined card letter, and a manifest
  with no `D` card is reclassified as a sub-manifest before its cards are
  checked, so it is rejected for its first illegal card rather than for the
  missing `D` card. The crosslink sweep's record of examined blobs is
  versioned; the first sweep after upgrading drops the old record and
  examines every blob again, once, so trees an earlier version could not
  parse are linked.
- The three-way merge behind `Merge` and `Update` is a port of fossil
  2.28's `merge3.c` and the diff it runs on, and writes what fossil writes
  for the same three files, byte for byte. A conflict's markers carry
  fossil's `(line N)` numbers and include the suggested resolution fossil
  computes by merging the conflicting lines token by token; marker lines end
  in CRLF when both versions' first line does. Content is aligned as
  fossil's diff aligns it, so clean merges and conflict regions match too.
  As in fossil, a UTF-8 byte-order mark is dropped and UTF-16 text is
  merged as UTF-8, and a file fossil cannot diff as text (a NUL byte, or a
  line over 32767 bytes) is not merged: the local copy is kept and the
  merge reports a conflict. `HasConflictMarkers` now skips `\r` as well as
  `\n` between lines, as fossil's `contains_merge_marker` does.
- The conformance suite is re-pinned to zit check-in `68a3f76cb7`, where the
  artifact-format, repo-state and sync-protocol drafts name themselves
  `-00` instead of `-01` throughout and their Changes appendices are headed
  "Changes in This Revision". No normative text changed and no section
  number moved, so the requirement tables and the coverage counts are
  unchanged; the pinned copies take zit's file names, and the comments and
  tables that cited the `-01` drafts cite the `-00` ones. The sparse-cone
  copy is re-pinned to `d497c2432f`, whose text is the same.

### Fixed

- `verify.Verify` audits leaves by repo-state §§7.2–7.3's same-branch
  rule and branch-tag recheck schedule instead of assuming every check-in
  without children must be a leaf. Arrival-order retained verdicts are
  accepted; decidable stale, missing and stray rows are reported. Both the
  draft's branch reading and Fossil 2.28's literal-trunk reading must agree
  that a row is wrong, although their same-branch witnesses may differ.
  Parent tags replace P-card ancestry only when valid and active; retained
  ancestry after cancellation is checked against recorded parent-tag history.
  Parent-tag tokenization now follows Fossil's single-byte separators rather
  than collapsing whitespace; malformed lists retain their previous ancestry.
  Uppercase hashes and NUL-terminated values follow Fossil too. Leaf and
  parent-parser conformance cases run Verify on both implementations' tables.
  Tracked in #433.

- `Checkout.Update` no longer merges a symlink: one edited locally and
  changed by the target is left as it is and listed in
  `UpdateResult.Unmerged`, as fossil's update reports "Cannot merge
  symlink", instead of writing conflict markers through the link into the
  file it points at. In a sparse checkout, a rename outside the cone no
  longer refuses the update: the rename moves the record, or writes the
  file when its new name is in the cone.
- `Checkout.Update` saves undo state as fossil's `update` does, and the new
  `Checkout.Undo` and `Checkout.Redo` reverse and replay it: the files the
  update wrote, merged or removed come back, with the file list, pending
  merges and the checked-out version (`checkout-hash` included). The undo
  tables now use fossil's schema (`isExe`, a unique `pathname`, replayed in
  `rowid` order), so `fossil undo` reverses a go-libfossil update and
  `Checkout.Undo` reverses `fossil update`; the executable bit saved is the
  file's own, as fossil reads it. Like fossil, the update saves a file
  merged into a renamed one under its old name only, and saves neither a
  `NAME-original` backup nor a change of executable bit alone. An update
  whose writes fail partway puts the files it changed back, as fossil's
  `undo_rollback` does. Under js/wasm, which has no undo, Update saves none.
  Tracked in #446.

- `Checkout.Revert` undoes a rename whose old name another file now holds:
  a swap, a chain (`a.txt` to `c.txt`, then `b.txt` to `a.txt`), a new
  file added under the old name, or an untracked file there. It used to
  refuse. Like fossil's `revert`, it now saves each file it writes or
  deletes for undo first, so `Checkout.Undo` or `fossil undo` brings back
  what it overwrote, and a revert that fails partway puts the files back.
  Every committed file ends under its committed name with its committed
  content, and the add at the old name is dropped, as fossil drops it.
  Fossil's own revert leaves a swap's files renamed and stops tracking the
  second file of a chain; go-libfossil reverts both. Revert now changes
  vfile in one transaction. Under js/wasm, which has no undo, it saves
  none. Tracked in #248.

- A public check-in makes every file blob it names public, as fossil's
  commit does. Content first stored privately, for example by syncing a
  private branch, stayed private when a public check-in reused it, so a
  peer without private access never received it and the check-in's file
  stayed a phantom there. Tracked in #439.

- The checkout scan sets `vfile.chnged` as fossil's `vfile_check_signature`
  does in every case. A file missing from disk is marked changed, with
  mtime -1, and so is a removed file. A change to the executable bit is
  recorded as 6 (became executable) or 8 (lost it), and a change to or from
  a symlink as 7 or 9, and a mode change that is undone returns to 0.
  Symlinks follow fossil's `allow-symlinks` setting, off by default: off,
  a symlink is seen as what it points to; on, as itself, its content being
  its target's name. Status reports the mode changes as fossil's `changes`
  command does, with the new change types `executable`, `symlink`, `unexec`
  and `unlink` (`CheckoutChange.Change`). Commit records the permission it
  finds on disk, `l` included, and a symlink's target as its content. As on
  Windows in fossil, permissions are not read where the storage has no file
  modes (`MemStorage`) or on Windows: the recorded ones are kept, where
  commit used to drop every `x`. `ScanENotFile` now refuses something other
  than a file where a tracked file belongs, as fossil's merge does, except
  at a pending add or removal, which fossil does not check.
  Extract and Update write a symlink entry as a symlink while
  `allow-symlinks` is on and the storage has symlinks, and otherwise as a
  plain file holding the target's name, as fossil does. They delete a symlink
  before writing a file over it, and never change a symlink's mode.
  With `allow-symlinks` off, fossil reports such a file `UNLINK`, and its
  checkout then refuses without `--force`; Extract does the same. Extract
  still writes a missing file without asking for `Force`, an unlinked
  symlink entry included, since nothing on disk is lost. That keeps
  Create followed by Extract working, as fossil's open does.
- Stash and undo read and write working files through the checkout's
  storage (`simio.Storage`) instead of the `os` package, so they work on a
  `MemStorage` checkout and under simulation (tracked in #448). Their
  functions take a `*simio.Env`. Stash save records each reverted file's
  mtime on its now unchanged row, as fossil's revert does. Stash apply and
  pop add a stashed added file again, as fossil's `stash_apply` does, and
  mark a restored edit's row changed with the mtime it was written with,
  where fossil leaves the reverted row (unchanged, the revert's mtime) for
  its next scan to correct; fossil misses an edit of the same size whose
  write lands in the revert's second. Undo and redo restore vfile as fossil
  does, and record each written file's mtime on its restored row when that
  row's mtime matched the file when it was saved, the case where the row
  vouches for the content; otherwise the row is restored with mtime 0 and
  the next scan hashes the file.
- Stash save scans the checkout first, as fossil's `stash_create` does, so
  an edit no scan has recorded yet is stashed too; before, it was left out,
  or the save failed with nothing to stash. `stash.Save` takes the scanner,
  a `*checkout.Checkout`, as its first argument.
- Undo and redo no longer print to standard output, and neither does
  Update's rollback of a failed update. `Checkout.Undo` and `Checkout.Redo`
  return an `UndoResult` listing the files they restored, created and
  removed, in fossil's order, for the caller to show as fossil's `UNDO`,
  `NEW` and `DELETE` lines.
- `verify.Rebuild` and `verify.Verify` check blob integrity in time linear
  in the repository, as fossil's rebuild does. They expanded blob by blob
  through a bounded cache, so on a real repository, whose delta chains run
  hundreds of revisions deep, every evicted chain was walked again from its
  root: the blob check on the Fossil SCM repository ran for over 45 minutes.
  The new `content.ExpandAll` walks each delta tree once from its root,
  expanding every blob from its parent's content, and reports for each blob
  what an expansion of it alone reports. On the same repository the blob
  check takes about 12 seconds and a whole `Verify` about 40; the issues
  reported are unchanged.
- `Checkout.Update` follows renames between the checkout's version and the
  target, as fossil's `update` does (`find_filename_changes`). A file with
  local changes that the target renamed and changed is merged under the
  new name, and the old name is removed; before, the edit was left behind
  as an untracked file under the old name and the new name got the
  target's content. The rename path runs through the two versions' common
  ancestor, so a rename on the checkout's own branch is followed backwards.
  Two cases follow fossil rather than the issue's wish: when the target
  only renamed the file, the edit stays under the old name, untracked, and
  the new name is not written (it shows as missing; the file is listed in
  `UpdateResult.Unmerged`); and a local rename of a file the target edited
  is not followed: the target's file is written under the old name, and the
  renamed copy is removed if unedited or kept, untracked, if edited.
  Tracked in #445.

- Each arrival of content is recorded in `rcvfrom` as fossil records it:
  one row per check-in, tag, sync, clone, served request that stores
  something, and unversioned write, made when the first content arrives, with
  the time as a Julian day, the credited user, the login card's nonce on a
  server, and the peer's address (or `#!fossil unversioned add` for a local
  unversioned write). Every blob and unversioned file stored names its row in
  `rcvid`, a phantom names none, and an applied deletion keeps the row's
  `rcvid`. A new repository no longer starts with a placeholder row that every
  blob pointed at; an existing repository keeps it, and new receipts follow
  it. `HandleOpts.RemoteAddr` passes the client's address to a server that
  is not `ServeHTTP` or `XferHandler`.
- The event rows of control, attachment and other non-check-in artifacts
  match the ones fossil derives. A tag's control artifact ('g') now carries
  fossil's comment byte for byte: each target introduced by an
  `Edit [hash|hash]:` link, its changes ordered by target and tag name,
  `sym-` tags named as tags, a branch move linked to its timeline and the
  move's own `sym-` changes left out, values HTML-escaped, and no leading
  space. Attachment comments link the attached artifact and the ticket or
  tech note as fossil 2.28 does, and a deleted attachment's `src` is NULL,
  not empty. `Repo.Tag` and closing a branch write the 'g' row fossil's
  `tag add` writes; they wrote none until the next rebuild or clone. Hash prefixes in these comments, and in ticket comments, follow the
  `hash-digits` setting up to 64 digits, where they stopped at 40.
  `Timeline` shows a tag change with the new text, and lists a tag added
  through this library as soon as it is added, as fossil's timeline does.
- A check-in with no C card has a NULL event comment, as fossil stores it,
  not an empty one.
- Event, plink, attachment and forum post times match, in every bit, the
  ones fossil 2.28 stores when it is built for the same platform. fossil
  writes these times as 16-digit SQL text and reads them back with its
  bundled SQLite 3.52.0, whose text-to-double conversion is not correctly
  rounded. go-libfossil parsed the text with a correctly rounded
  conversion, so about one time in 3,000 was one unit in the last place
  away from fossil's (13 rows of fossil's own repository); it now uses
  SQLite 3.52.0's conversion. fossil's printf also computes the text in C
  `long double`, which is a 64-bit double on macOS arm64 and Windows but
  80-bit on x86-64 Linux, so the 16th digit differs between platforms for
  about one time in five. go-libfossil now computes in the long double of
  the platform it runs on, and matches fossil 2.28 bit for bit on macOS arm64
  and linux-x64. Times written on one platform and rebuilt by fossil on
  another can still differ in the last place, as two fossils do; never by
  a millisecond.

## [0.13.0] - 2026-10-20

### Added

- A checkout-free interface for apps that embed the binary, documented in
  `docs/site/content/docs/cli-embedding.md` with its stable JSON shapes and
  exit statuses (0 success, 1 failure, 2 not found). `repo snapshot` commits
  one file's bytes onto trunk, a no-op that prints the tip when they match;
  `repo log --path` lists the check-ins that changed a path, as `fossil
  finfo` does; `repo cat --at VERSION PATH` reads a file as a version holds
  it, as `fossil cat -r`; `repo diff --json A B [PATH]` diffs two versions.
  Concurrent snapshots from several processes build one linear trunk.
  Tracked in #451.

### Fixed

- `repo new` accepts the repository path as `-R`, as the other embedding
  commands do (it had required the argument and exited 80), and a path whose
  parent directory does not exist is refused with exit 1 and a message naming
  the directory, where it had failed with a SQLite "out of memory" error. The
  behaviour is documented in `cli-embedding.md`. Tracked in #451.
- `repo log --path`, `repo cat --at` and `repo diff` match a PATH as fossil
  does: they drop a leading `./`, a trailing `/` and doubled or `/./`
  separators, and compare names with the repository's `case-sensitive`
  setting (fossil's default when unset: off on macOS and Windows, on
  elsewhere), so `notes/a.md` finds `Notes/A.md` when it is off. They had
  rejected both with exit 2 and compared case-sensitively. Not found stays
  exit 2, where fossil exits 1. Tracked in #451.
- Opening a repository runs every pragma in a fixed order (`busy_timeout`,
  `journal_mode`, `locking_mode`, `synchronous`, `foreign_keys`, then the
  rest by name), not only `busy_timeout` first. Tracked in #451.
- A stored Julian day converts to the millisecond SQLite's own date
  functions give it, `(sqlite3_int64)(r*86400000.0+0.5)`
  (`fsltype.MillisFromJulianDay`). `db.ScanTime` truncated, so most times it
  returned were 1ms before the check-in's D card.
- Opening a repository sets `busy_timeout` before any other pragma. The
  drivers ran the DSN's pragmas in map order, so an open while another
  process held a lock could fail at once with "database is locked".

### Changed

- A file's history (`manifest.FileHistory`) orders check-ins that share a
  millisecond by rid, newest first, and applies its limit after dropping rows
  that list the file unchanged.
- Check-ins, tags and branches record their time to the millisecond, as
  fossil and zit write it. The default clock time, which carried nanoseconds
  that zit cannot read (danmestas/zit#244), is truncated, as fossil reads its
  clock. A caller time with finer precision is rounded to the nearest
  millisecond, as fossil rounds a `--date-override`, except that a fraction
  past `.999` stays at `.999` rather than carrying into the next second, as
  in fossil. An exact half millisecond rounds up; fossil's result there
  depends on how its bundled SQLite prints the seconds (fossil 2.28 writes
  `.124` for `.1245` but `.020` for `.0195`), so it is not followed.
- The conformance suite is re-pinned to the -01 drafts as zit check-in
  `3f86ff4261` holds them, with six more errata applied. The artifact format
  says the Z card is last by the ordering and required-card rules, and §6.2
  decides the Z verification state from the bytes at n-35 and n-34 (AF-6.2-7).
  §4.7.3 filters the named delta's delete markers out of a delta-of-delta's
  file set (AF-4.7.3-2), and §4.7.8 reads an absent and a present empty `J`
  value alike (AF-4.7.8-1). In repo-state §5.3, a cancel keeps its card's
  value on the row it writes, while the event table and propagation treat
  it as null (RS-5.3-3, RS-5.3-6). An artifact's tag cards apply in card
  order (RS-5.3-7), so a branch root carrying `*sym-NAME` and `-sym-NAME`
  keeps the tag (§6.2, RS-6.2-4). In sync-protocol §6.6, a client recognizes
  a refusal by its leading text (SP-6.6-4). go-libfossil already behaved this
  way; six requirements are new, two are reworded, and each has a case
  checked against fossil 2.28.
- Checkouts record each file's mtime in `vfile.mtime` and scan the way
  fossil's `vfile_check_signature` does: a file whose size matches its
  artifact and whose mtime matches the recorded one is taken as unchanged
  without being read, unless `ScanHash` is given or the repository's
  `mtime-changes` setting is off. A global `mtime-changes` in `~/.fossil`,
  which fossil falls back to, is not read: go-libfossil reads no global
  settings. Status, commit, merge, revert and extract use this scan, as fossil's commands do; before, every scan hashed every
  file and `vfile.mtime` was always 0, so fossil rehashed a go-libfossil
  checkout on every command too. Extract, update, revert, commit and
  `SetMTime` record mtimes where fossil does. One deliberate difference:
  fossil would miss an edit that keeps a file's size within the same second
  its mtime was recorded, in status and in commit; go-libfossil records such
  an mtime as 0 and rehashes the file instead, and fossil reading those rows
  rehashes it too. On the sqlite
  repository's 2,220-file checkout, status takes about 30 ms instead of
  about 250 ms. `simio.MemStorage` now gives each write a new mtime, as a
  filesystem does.

### Fixed

- Ticket artifacts link into a repository whose ticket schema lacks any of
  the columns fossil treats as optional: the ticket table,
  `ticket.tkt_ctime`, the ticketchng table, `ticketchng.tkt_rid` and
  `ticketchng.tkt_user`. Crosslink always wrote `tkt_user` and `tkt_ctime`,
  so rebuilding fossil's own repository, which has no `tkt_user`, failed.
  The ticket and ticketchng rows now follow fossil's `ticket_insert`: a
  change row is written only when it has a field or bookkeeping column to
  fill, `tkt_user` only when the artifact names a user, a field stored in
  both tables is copied from the ticket row when the artifact leaves it
  alone, and a `baseline for <field>` column records the rid that last
  assigned the field. Without a ticket table nothing is derived for a
  ticket, including its timeline rows, as in fossil.
- `Checkout.Update` keeps uncommitted changes, as fossil's `update` does.
  It used to compare only committed versions, so it wrote the target's
  content over a locally edited file, deleted an edited file the target
  removed, and merged the current and target check-ins with their common
  ancestor while ignoring the working file. Visible changes:
  - Local edits are preserved. A locally edited file is merged three ways,
    with the checkout's version as the base, the working file as the local
    side and the target as the other; conflicts are written as markers and
    listed in `Conflicted`.
  - An update to another branch now takes the target's content for every
    file without local changes, as fossil does; it used to merge the two
    branches' committed changes, which could write conflict markers.
  - An edited file the target deletes is kept, untracked; pending adds and
    removals stay pending; a missing file is restored; a file the target
    adds over an untracked one, read-only or not, is written after the
    untracked one is copied to `NAME-original`.
  - A binary file (a NUL byte, or a line over 32,767 bytes) is never
    line-merged. Update saves the working copy as `NAME-original` and
    writes the target's content; Merge keeps the checkout's copy, whatever
    the file's strategy. Before,
    both spliced the versions line by line and reported a clean merge.
  - The new `UpdateResult.Unmerged` lists files left to reconcile without
    markers: kept edited-but-deleted files, files added both locally and
    by the target, and binary conflicts.
  - Every file takes the target's executable bit, including a change to the
    bit alone. An update that could not finish (a directory where a file
    must be written, or a file or directory the process may not change) is
    refused before anything is written, the checkout database moves in one transaction, and a dry run
    changes nothing, the database included.

  Update saves no undo state yet, so unlike fossil's update it cannot be
  undone: a removed file restored from the target, or a working copy merged
  or replaced, cannot be brought back with undo. Files are matched by name:
  a file the target renamed is not followed, as fossil follows it. Tracked
  in #443.

- `config.mtime` and `user.mtime` hold seconds since 1970, as fossil's
  `now()` writes them. `SetConfig`, `SetHashPolicy` and `CreateUser` wrote a
  Julian day there, which fossil, comparing those times when it merges
  configuration, read as a moment in 1970. `SetCaps` and `SetPassword` now
  stamp the user's mtime too, as `fossil user capabilities` and
  `fossil user password` do. Opening a repository converts the Julian days
  already written there to seconds, or leaves them for a later open while
  another connection holds the write lock.
- A sync login no longer reads `user.cexpire`. That column is the expiry of
  fossil's web-login cookie, which fossil's own sync login ignores; reading
  it refused any user who had logged into or out of fossil's web UI, or
  whose cookie had lapsed (#441). `repo invite --ttl` keeps its expiry in a
  config row of its own, `invite-expire:LOGIN`, in UTC, and a sync login
  checks that instead. Creating or deleting a user clears that row. The expiry used to be written in local time and read
  as UTC, so it was off by the local offset.
- `SetMTime` on a checkout's extract and update now does what fossil's
  `--setmtime` does: each file takes the time of the newest check-in in the
  version's primary ancestry that recorded its content, in whole seconds,
  and every unedited file gets it, not only the files the update wrote. A
  file with local edits is left alone. It used to give every written file
  the target check-in's time, to the millisecond.
- A stash records its creation time as a Julian day and lists it as
  `YYYY-MM-DD HH:MM:SS`, as fossil's stash does; it stored SQLite's
  `CURRENT_TIMESTAMP` text, which the drivers handed back in different forms,
  and listed a fossil-written time as a bare number. Stashes list oldest
  first, by creation time, as `fossil stash ls` orders them, rather than
  newest first by id. A bisect listing shows
  each check-in's date as `fossil bisect ls` does, instead of a raw Julian
  day.
- `ResolveVersion("NAME:TIMESTAMP")` bounds the search with SQLite's
  `julianday()` of the rounded-up time, as fossil does, rather than the text
  form of that time that event rows hold. On a millisecond where the two
  differ, a check-in at the bound itself resolved here but not in fossil.

## [0.12.0] - 2026-10-13

### Added

- `CloneOpts.PullPhantoms` makes `Clone` fetch the artifacts its clone rounds
  left missing with a follow-up pull session, and fail if any are still
  missing after it, as every clone did before. That pull departs from
  sync-protocol-01 Section 8.2, which sends no clone, pull or push card after
  `clone_seqno 0`, so it is off by default (tracked in #424).
- `Repo.SetHashPolicy` sets the hash policy by its name, as
  `fossil hash-policy NEW-POLICY` does: sha1, auto, sha3, sha3-only or
  shun-sha1, matched case-sensitively and exactly (artifact-format-01 7.1).
  Any other string, a wrong-case or empty one included, is an error that
  leaves the policy unchanged. Asking for auto in a repository that already
  holds a SHA3-named artifact stores sha3 (7.3), as fossil does. New
  conformance cases drive each name, wrong-case names, unknown names and
  the empty name through it, so AF-7.1-2 is now covered rather than partial.
- `Checkout.ResolveVersion` resolves a version name the way `Repo.ResolveVersion`
  does and also resolves the names relative to the checkout: `current`,
  `prev`/`previous` (its primary parent) and `next` (a child, primary first,
  then the newest), as fossil does inside a checkout (repo-state-01 §8.3).
  A keyword that names nothing, such as `prev` at a root, falls through to the
  other forms. `ckout` stays an ordinary name: it is a keyword only in the
  checkout-inclusive request, `Checkout.ResolveWorking`.
  `Repo.ResolveVersion` is unchanged. A checkout with no recorded version has
  no relative names, and every other form still resolves. The `merge` command
  resolves its version through the checkout, so `merge prev` and `merge next`
  work.
- `Checkout.ResolveWorking` is fossil's checkout-inclusive request (`ci+`,
  repo-state-01 §8.3) and returns a `WorkingVersion`. While the checkout has a
  version, `ckout` names the working files: `Working` is set and `RID` is the
  checkout's version. No pseudo-RID is exported. Every other name, and `ckout`
  in a checkout with no version, resolves as `Checkout.ResolveVersion` does,
  except that only check-ins can answer: the request is a check-in request,
  so a technote carrying the tag does not. `ckout` is a keyword only as the
  whole name, so `ckout.` and `root:ckout` read it as an ordinary name.
  `ckout` is a keyword only in `ci+` (as the 2026-10-13 erratum has it, and
  fossil), so `ResolveVersion` and `ResolveBranchStart` read it as an
  ordinary name.
  New `repostate.8.3` cases run the request against `fossil test-name-to-id
  --type ci+`: `ckout` over a tag of that name, with and without a checkout
  version, and `ckout` as the tag under the untyped request (RS-8.3-4;
  tracked in #419).
- `Repo.ResolveBranchStart` is fossil's branch-type request: it resolves a
  name, then returns the first check-in of the branch the result is on
  (repo-state-01 §8.3). It follows the draft where fossil 2.28 differs:
  fossil walks back only from a `tag:NAME` or plain-name result, so a hash,
  `tip` or a date gives the named check-in itself there. As the draft and
  fossil both require, only check-ins can match, so a technote tagged with the
  branch name does not shadow the branch.
  The `repostate.8.3` checkout and branch-type cases now run against these
  and pass (RS-8.3-4, RS-8.3-12; tracked in #386).
- `Repo.CurrentTrunk` returns the current trunk of repo-state-01 §9: the
  `main-branch` setting's name (`trunk` when unset) resolved as
  `ResolveVersion` resolves it, as fossil does for `update` outside a checkout
  and `open` with no version. Only check-ins can answer, so a technote tagged
  with the name does not.
- `Repo.BranchOpenLeaf` names one open leaf of a branch, the one with the
  greatest mtime, or reports the branch not open (repo-state-01 §9). It
  follows the draft where fossil 2.28 differs: fossil's `branch info` takes
  whichever open leaf its query meets first. An empty name is an error, as in
  `BranchTip`. `BranchTip` is unchanged and still returns the newest check-in on the branch, leaf or not.
  RS-9-1 and RS-9-4 now have cases (`repostate.9.current-trunk*`,
  `repostate.9.open-leaf-*`) and are no longer out of scope.
- `db.DB.WithBoundTx` runs a function in one write transaction and hands it
  a DB bound to that transaction: every statement on it joins the
  transaction, and a `WithTx` nested on it is a savepoint there, which its
  error rolls back alone, instead of a second connection waiting for the
  write lock. `WithTx` on a bound DB behaves that way too; on any other DB
  it is unchanged. The sync passes of #418 run on it.
- `SyncOpts.ShareLinks` turns on share-links mode (sync-protocol-01 §9.5),
  fossil's `--share-links`. Off by default. A pulling session sends
  `pragma req-links` in its first request, after the operation cards, unless
  `SyncOpts.ProjectCode` names another project, and keeps each
  `pragma link URL ARG MTIME` as config `link:URL` = ARG: URL and ARG
  defossilized and otherwise verbatim, an existing link replaced only by a
  strictly newer MTIME. ARG is never parsed or sent on. A link with the wrong
  token count, a URL that is not HTTP(S) or an MTIME that is not a positive
  decimal is dropped, as is every link with the mode off. Two of these follow
  the draft where fossil 2.28 differs: fossil fails fatally on a link URL its
  parser rejects (a bad port, or a form that is neither a URL nor a local
  repository) instead of dropping the link, and it reads MTIME with
  `strtoll` base 0, so it keeps `0x10` as 16 and `12z` as 12. Like fossil, a
  push-only session ignores the option. The server is unchanged: it emits no
  link, which meets the draft whether or not a request asks for links. New
  `sync.9.5` client cases run against `fossil sync --share-links` as well,
  and SP-9.5-4 is now covered rather than partial, with two new
  requirements, SP-9.5-5 (which links a client keeps) and SP-9.5-6
  (`req-links` only in share-links mode). Tracked in #425.

### Changed

- `Clone` now ends at `clone_seqno 0` and its cleanup round, as
  sync-protocol-01 Section 8.2 requires (SP-8.2-5): by default it no longer
  runs a follow-up pull for the phantoms a clone leaves, and it succeeds with
  them in place, as fossil(1)'s clone does. A caller that relies on a
  complete clone sets `CloneOpts.PullPhantoms`. Only the tests of that pull
  set it: the `repo clone` command, the DST clone harness and the parity
  tests all clone as fossil(1) does. The `sync.8.2.clone-zero-phantom` case
  passes and is no longer a deviation (tracked in #424).
- The sync server answers a client-sent `clone_seqno` with
  `error bad command` and goes on with the cards after it, as
  sync-protocol-01 Section 5.2 says for a reply-only card sent by a client
  (SP-5.2-6), and as fossil's `page_xfer` does, including a `clone_seqno`
  whose argument is not decimal. The error quotes the line as received. It
  used to ignore the card.
  The `sync.5.2.client-clone-seqno` case passes and is no longer a deviation
  (tracked in #424).
- The conformance suite is re-pinned to the -01 drafts as zit check-in
  `5a1b741b25d8` holds them, with five more errata applied: repo-state §5.3
  (an inherited row blocks an older direct tag, so the result follows
  arrival order), §7.3 (leaves are rechecked only on branch-tag events),
  §8.3 (`ckout` is a keyword only for `ci+` and only as the whole name),
  sync-protocol §9.4/§9.5 (`pragma avoid-delta-manifests` answers ci-lock)
  and §10.1 (a file whose content does not match its hash fails the round).
  zit now keeps each -01 text under the draft's -00 file name; the copies
  keep their -01 names and `conformance/rfcs/README.md` records the zit path
  and check-in. The new errata are pinned. The requirement tables are re-read
  against every changed hunk: RS-5.3-1, RS-7.3-1, RS-7.3-3, RS-8.3-4,
  SP-9.4-1, SP-10.1-6 and SP-10.1-7 are reworded, and RS-5.3-5, SP-9.4-3,
  SP-9.5-7 and SP-10.1-8 are new. The §7.3 and §8.3 cases that were marked
  as deviations, because go-libfossil already followed fossil, now pass as
  written, and new cases cover arrival order for a date tag, an untagged
  chain, one-batch leaf derivation and a merge parent's recheck.
- A pull that receives a `uvfile` whose content does not match its hash now
  records the error, commits the cards before it and evaluates the continuation
  rules, as sync-protocol-01 Section 10.1 and fossil(1) do (SP-10.1-7), where it
  used to end the session with an error. A file or cfile that does not match
  its hash still fails the round (tracked in #421).
- The sync server answers every `pragma ci-lock` that passes its argument
  guard with `pragma avoid-delta-manifests` while the repository's
  `forbid-delta-manifests` setting is on, after any `ci-lock-fail`
  (sync-protocol-01 §9.4). The setting is read as fossil's `db_get_boolean`
  reads it, defaulting to on as the draft and fossil 2.29 do; fossil 2.28's
  server defaults it to off, so against 2.28 the cases that rely on the
  default record that difference. The client recognizes the pragma and,
  as §9.5 now says, changes nothing about its transfer; go-libfossil commits
  without an autosync, so no commit is pending when it arrives.
- A file or cfile whose content does not match its hash, found when it
  arrives or later in the reply, now fails the round on the client
  (sync-protocol-01 §10.1): nothing the reply carried is committed, the
  igots and files before it included, and no further round starts. `Sync`
  and `Clone` return the error as before. Content errors that leave the
  stored artifacts consistent, such as a malformed file card or an
  unversioned file with a wrong hash, still commit the prefix and continue.
  The `sync.10.1.integrity-error-*` cases now check that, against fossil
  2.28 as well, and the gap tracked in #421 is gone with the text it rested
  on.
- The conformance suite now checks the -01 drafts of the artifact-format,
  repo-state and sync-protocol specifications (the delta format stays on -00;
  there is no -01). The -00 errata are folded into -01 and no longer pinned;
  the six -01 errata are pinned for the fossil 2.28 behavior they record.
  `conformance/rfcs/README.md` records the zit check-in each copy came from.
  New cases cover the date tag's event time, the Julian-day grammar, a
  delta manifest naming a delta, dropped reserved F cards, the
  trailing-punctuation retry, truncated payloads, `errors N`, refused pushes
  and the private-content gimme answer. No library behavior changes. Where
  -01 departs from fossil 2.28, the cases follow the draft: a cancelled,
  unparsable or `0` date tag keeps the D-card time under repo-state-01 §4.2
  step 6, where go-libfossil, like fossil, stores no time or a time of 0;
  those cases are marked as deviations. The draft non-conformances the
  re-pin exposes are tracked in #370, #371, #372, #373, #374 and #375.
- More conformance cases for the artifact and delta suites: single-name
  lookup in a delta manifest (8.4), a lowercase K and R card from the writer
  (4.7.9, 6.3.4), date-only timestamps (4.6.4), mismatching delta checksums
  beyond the first (3.4), and the 2^32 limit on delta sizes (3.1), checked
  with an unread 4 GiB mapping (skipped where it cannot be mapped).
  AF-4.7.9-1, AF-8.4-1, DF-3-2, DF-3.1-6 and DF-3.4-3 are now covered.
  AF-6.3-5 stays uncovered (go-libfossil does not check the R card at commit,
  tracked in #387). Artifact-format-01 Section 4.6.4 lets a reader accept the
  time-only and now/subsec forms (a reader MAY accept forms 2 through 5), so
  `deck.Parse` keeps rejecting them while fossil(1) and the date-tag path
  (SQLite julianday) accept them.
- The conformance suite covers eleven more sync-protocol-01 rows and part of
  a twelfth: the peer-lacks gimme filter, the twentieth redirect, VT, FF and
  server-side token runs, the SHA3 refusal left out of `errors N`, the Clone,
  Private and Admin capability checks, the fossilized `uvfile` NAME, the
  deferred clone configuration, the well-formed `uvigot` ban, the private
  inventory under private sync, the private-igot continuation, the written
  and kept check-in lock, and a discarded `pragma link` ARG. Where fossil(1)
  2.28 differs (it repeats a gimme for a hash the peer lacks, refuses an
  escaped `uvfile` NAME, and sends a well-formed `uvigot` past `max-upload`),
  the cases follow the draft and record fossil's behavior. SP-9.5-4 is
  completed by `SyncOpts.ShareLinks` (see Added), which stores a link.
- Repo-state conformance cases now cover a misnamed artifact (RS-4.4-3), the
  default branch and the `main-branch` setting (RS-6.1-2, RS-6.1-4), the
  active-tag rule for leaves and the checkout's branch (RS-6.4-1), a parent
  tag that arrives before its target (RS-4.2-8), and the leaf recheck of
  untagged check-ins (RS-7.3-3; a check-in with a parent but no branch tag is
  rechecked only as the parent of a branch-tagged one, as in fossil, which
  the 2026-10-13 erratum made the draft's text).
  Where fossil 2.28 differs (its rebuild links a misnamed artifact, and its
  `branch current` and `leaf_check` read cancelled branch rows), the cases
  follow the draft and record the difference. Checkout-relative names and
  branch-type requests (RS-8.3-4, RS-8.3-12) have cases tracked in #386.
- A repo-state conformance case, `repostate.6.1.main-branch-leaf`, pins the
  leaf table's reading of an untagged check-in as the `main-branch` setting's
  branch (RS-6.1-4). fossil 2.28's `leaf.c` reads it as the literal `trunk`,
  so the case records that difference. No library behavior changes.

### Fixed

- The leaf table is maintained incrementally, as repo-state-01 §7.3
  (RS-7.3-3) and fossil(1) do, instead of being recomputed at the end of
  every crosslink batch. A check-in and its parents, merge parents
  included, are rechecked only when a branch tag of any type is written to
  it or tag propagation updates or deletes its branch row; crosslinking a
  check-in, adding a plink edge or reparenting schedules nothing. The
  rechecks run at the end of the batch: the closing repair of a sweep,
  clone or sync session, the end of each phantom-fill cascade transaction,
  and the end of a commit's or a tag's own transaction. A check-in
  therefore keeps its verdict when an untagged child arrives in a later
  batch (`repostate.7.3.recheck-untagged-child` passes and is no longer a
  gap, tracked in #426), a commit onto a parent whose branch row is a
  singleton leaves both verdicts as they were, and a branch tag added by a
  tag control artifact now rechecks its target and parents, which it did
  not before. A cancel or singleton that replaces a propagating branch row
  rechecks the descendants that row reached, even in a sweep that has not
  yet handed them the row, so `verify.Rebuild` and a sweep give the table
  `fossil rebuild` gives (`repostate.7.3.cancel-propagation-recheck`).
  `verify.Rebuild` applies the batch's own schedule, the single-batch result
  §7.3 defines, and the corpus parity tests still match `fossil rebuild` row
  for row. One departure from fossil, which follows the draft: a recheck also
  covers the parents a check-in has when the schedule runs, not only those it
  had when scheduled, so a tag applied before its target is linked, or a
  reparent inside the batch, gives the same table in any link order. The
  reparent case is split:
  `repostate.4.2.reparent` loads in one batch, and the new
  `repostate.7.3.reparent-no-recheck` loads per artifact, where the old
  parent keeps its verdict, as in fossil's own crosslink.
- A check-in that a sync linked through the phantom-fill cascade now gets its
  leaf row and propagated tags (#414). A pulled check-in arrives as an
  announced phantom filled the next round, so the cascade links it, and the
  session's closing sweep then found nothing left to link and skipped the
  leaf and tag-propagation repair: the check-in had an event row and no leaf
  row, and its parent stayed a leaf. The cascade now reports what it linked,
  and the sync client and the server handler each run the repair once at the
  end of the session when the cascades or the sweep linked anything, as
  repo-state-01 §7.3 (RS-7.3-4) requires before the leaf table is read; not
  once per file. New cases `repostate.7.3.pulled-child` and
  `repostate.7.6.pulled-retag` each also run as a `.pull` case, which pulls
  the script's last artifacts from a go-libfossil server into a repository
  already holding the first. A session that fails after a round whose cascade
  linked check-ins runs the same repair before returning its error, as
  fossil's per-round manifest_crosslink_end does.
- A received delta stored before its basis is now checked against its HASH
  when the basis arrives later in the same pass: the same reply on the sync
  client, the same request on the server, the same round of a clone
  (sync-protocol-01 Section 7.7, SP-7.7-4). It was kept unverified, and the
  mismatch surfaced only as a logged warning from the after-dephantomize
  cascade and an expansion error wherever the artifact was later read. Each
  receiver now remembers the deltas its pass stored dangling, and the store
  that makes one's bytes available checks it in that store's transaction,
  including a delta whose chain to that basis runs through a row an earlier
  pass stored. The check applies each delta of a chain to its parent's
  bytes, so a clone's chain of n deltas costs n applies, not n squared. A
  mismatch aborts the whole pass, as fossil's commit hook does (see the
  #418 entry below): the client's sync and a clone fail, and the server
  answers `wrong hash on received artifact: HASH` naming the delta. The
  cases `sync.7.7.late-basis-wrong-hash`,
  `sync.7.7.clone-late-basis-wrong-hash` and
  `sync.7.7.server-late-basis-wrong-hash` now pass. Tracked in #412.
- Each server request, each client reply and each clone round now runs in
  one write transaction, as fossil's `page_xfer` runs a request after
  `db_begin_write` and `client_sync` runs a round between
  `db_begin_transaction` and `db_end_transaction` (sync-protocol-01 Sections
  7.7 and 10.1). A received delta found wrong only when its basis arrives
  later in the pass fails Section 7.7's commit check, and that now rolls
  back everything the pass stored, the basis, the phantom-fill crosslink
  cascades and the server's closing leaf and tag repair included, where it
  used to undo only the delta and keep the rest. The server still answers
  with the wrong-hash error and its timestamp comment, as
  `sync.7.7.server-late-basis-wrong-hash` requires; fossil 2.28 and 2.29 differ,
  since their commit hook fails inside `page_xfer` and the request ends with
  an HTTP error page instead of that card. A client's sync and a clone fail
  with the error. The server's `invalid clone sequence number` fatal
  rolls the request back too, so the side-effect-free walk of the request
  that stood in for that rollback is gone; a request that pushes a file with
  a wrong hash ahead of such a clone card now gets the file's error, where
  page_xfer's card loop breaks, not the clone's. Every other card error still keeps the work
  before it, as Section 10.1 requires: on the client that prefix now commits
  whole, where a card error used to return before the reply's buffered igot
  phantoms and visibility changes were written. The deviation on
  `sync.7.7.late-basis-aborts-pass` and
  `sync.7.7.server-late-basis-aborts-request` is gone, so SP-7.7-4 is
  covered, and the new requirement SP-10.1-6 has the case
  `sync.10.1.integrity-error-prefix`. fossil 2.28 stores a file whose hash
  is wrong and then aborts at commit, which rolls that prefix back; the case
  follows the draft. After such an error the client ends the session
  instead of evaluating continuation (Section 3.7); that is SP-10.1-7, a
  marked gap tracked in #421. Server requests, pulls and clones included, now
  hold the repository's write lock for the whole request, as fossil's
  `db_begin_write` does, so concurrent requests to one repository wait on
  each other up to the busy timeout. A `ContentCache` must not outlive a
  rolled-back pass: a rolled-back request frees row ids that later inserts
  reuse. Tracked in #418.
- A received delta against a held basis whose bytes are not its name was
  stored unchecked: the basis failed its own hash check, so the delta was
  treated as having no basis. The basis is now reconstructed without that
  check, as fossil's `content_get` does, and the delta's result checked
  against its HASH (Section 7.7 hashes the reconstructed artifact, not its
  basis), so a mismatch is refused and a match kept. A held basis whose
  bytes cannot be rebuilt at all (bad zlib, or a delta in its chain that does
  not apply) is a mismatch too: fossil's `content_get` keeps the bytes it
  has, so the delta is applied to those and the result refused. New cases:
  `sync.7.7.misnamed-basis-wrong-hash`, `sync.7.7.misnamed-basis-verified`
  and `sync.7.7.unreadable-basis-wrong-hash`.
- The crosslink sweep no longer holds back a check-in for absent file
  content (#406). A root check-in, or one on a present parent, whose files
  never arrived got no event, plink, mlink, leaf or tag rows, so it never
  appeared. The sweep -- `Crosslink`, the step that ends every sync and
  clone, and `verify.Rebuild` -- now links it against phantoms for the
  absent files, as fossil's `add_one_mlink` does and as repo-state-01 §4.2
  (RS-4.2-2) requires of an unresolved reference; the content fills the
  phantoms in place when it arrives. The receive-time paths, the
  phantom-fill cascade and a clone's receive linker, still wait for files
  while a session is delivering them, so `Checkout.Update` does not meet a
  check-in whose files are in flight; the closing sweep bounds that wait.
  New case: `repostate.4.2.present-parent-absent-file`; the gap case
  `repostate.4.2.root-absent-file` now passes, and both also run through
  `verify.Rebuild`.
- `verify.Rebuild` no longer links a delta manifest whose B-card baseline is
  absent (#406). It wrote event and plink rows, with no baseline id, where
  `fossil rebuild` refuses it (`fetch_baseline`). Every crosslink path now
  refuses it the same way, as artifact-format-01 §8.3's soft failure: the
  baseline gets a phantom, so a sync requests it, and the manifest is filed
  in `orphan` under it, where the phantom-fill cascade links it when the
  baseline arrives. Before, a sweep deferred it without either, so nothing
  asked for the baseline. A baseline that is stored but is not a check-in
  manifest is refused the same way, as `fetch_baseline`'s `manifest_get` does.
  The `orphan` row is dropped whenever the manifest links, by any path.
  `verify.Rebuild` clears and rederives `orphan`. New requirement RS-3.1-3
  and cases `repostate.3.1.absent-baseline` (with `.rebuild`),
  `repostate.3.1.non-manifest-baseline` (with `.rebuild`) and
  `repostate.3.1.late-baseline`.
- A check-in whose primary parent is absent is no longer held back for
  absent file content (#398). The crosslink sweep, the receive linker and the
  phantom-fill cascade deferred it until its files arrived, but with no parent
  manifest there is no file transition to record, so the wait could never end
  for a check-in whose files the repository will not receive. Its event and
  plink rows and its parent phantom were missing where `fossil rebuild`
  writes them, as RS-4.2-2 requires: the libfossil repository holds one such
  fossil-scm check-in, imported without its history. Its file transitions are
  derived when the parent arrives, against phantoms for files still absent,
  as fossil's do. New case: `repostate.4.2.phantom-parent-absent-file`. A
  root check-in, or one on a present parent, with absent files is covered by
  the #406 entry above.
- A received `file` or `cfile` delta whose basis is held is now checked
  against its HASH before it is stored, on the sync client, the server and a
  clone alike (sync-protocol-01 Section 7.7, SP-7.7-4). The server had
  checked only a `file` delta, since #402, and the client and clone stored
  every delta as given. A delta whose result is not HASH, or that does not
  apply, is not stored: the client's sync fails and a clone fails with the
  wrong-hash error a full file already gets, and the server answers with its
  wrong-hash error. A basis not held still leaves the delta stored dangling,
  as fossil does. fossil checks a `file` delta on receipt and a `cfile` or
  clone delta in its commit hook; this checks them all on receipt. Tracked
  in #405. A delta whose basis arrives later in the same reply or request
  is covered by the #412 entry above.
- `Checkout.Update` with no target, and `CalcUpdateVersion`, now pick the
  target as fossil's `update` does when no version is named: the newest open
  leaf descended from the checkout (every descendant leaf when all are
  closed), kept to the checkout's branch when there are several. If several
  remain on that branch the update fails ("multiple descendants") instead of
  guessing. It previously moved to the newest leaf on any branch, including
  leaves that do not descend from the checkout. repo-state-01 does not
  define update with no version named. Where fossil 2.28's
  `compute_leaves` reads branch rows without tagtype > 0 and an untagged
  check-in as `trunk`, this reads active rows only and the `main-branch`
  setting, as Sections 6.4 and 6.1 require of branch membership; an untagged
  checkout is kept to the default branch, where fossil keeps none of the
  leaves and stays put, and untagged leaves count as the default branch's.
  Section 10.3 says closed-ness and leaf-ness are not inputs to update's
  target computation; that is read as covering a named update, while
  fossil's no-version update drops closed leaves and works from the leaf
  set (an erratum candidate).
- The initial check-in starts the branch named by the `main-branch` setting
  (branch tag and `sym-` tag), not the literal `trunk`, as repo-state-01
  Section 6.1 and fossil's `commit` require.
- `LeafAmbiguous` is false for a check-in with no active branch tag: Section
  6.1 attributes it to the default branch but says that does not make it an
  open leaf of that branch, and fossil's `leaf_ambiguity` compares raw
  values, where a missing row matches nothing. It used to compare against
  the default branch's open leaves.
- A check-in now verifies its own R card before it is linked: the stored
  manifest is read back and the digest recomputed over the tree it resolves
  to (a delta manifest's baseline merged with its own cards) must equal the R
  card byte for byte, as fossil's checkin.c self-test does and
  artifact-format-01 6.3.4 requires. A mismatch, such as an uppercase R card,
  fails the commit. Tracked in #387.
- A `hash-policy` setting that is not one of the ordinals 0 through 4 is now an
  error when content is named or stored, not a silent fall back to the
  default (artifact-format-01 7.1: an unrecognized policy value is fatal),
  and `Repo.SetConfig` refuses to write such a value, as fossil's
  `hash-policy` command does. fossil 2.28 reads a non-integer value as 0
  (sha1), treats a negative stored value as unset (db.c) and leaves a value
  above 4 undefined; -01 is followed here, so every stored value outside 0
  through 4 is an error.
- The sync client treats a reply `gimme` or `igot` whose HASH is not an
  artifact name as fossil's client does: it reports `unknown command: [LINE]`
  and stops reading the reply, as sync-protocol-01 Section 5.2 (SP-5.2-8)
  requires (#397). It acted on the card and read on. The server already
  answers such a `gimme` and `igot` with bad command; a new case pins the
  `gimme` half.
- The same holds for a reply `file` or `cfile` whose HASH or DELTASRC is not an
  artifact name: the client reports `malformed file line` (`cfile`) and stops
  reading the reply, keeping the prefix already processed, as fossil's
  `xfer_accept_file` and client loop do (#397). It aborted the whole sync with
  an invalid-UUID error before.

- An `igot` under push reclassifies an artifact or phantom the server
  already holds, as sync-protocol-01 Section 7.5 (SP-7.5-5) and fossil's
  `content_make_private` and `content_make_public` in `page_xfer` do (#391).
  A public `igot HASH` makes it public; `igot HASH 1` makes it private,
  from any pushing requester, though one without the private capability
  still gets no new phantom. Several igots for one name in a request apply
  in card order. This follows fossil: a requester who may push can make a
  private artifact public by announcing it, and a client that pushes its
  private igots makes the server's copy private again.

- A pushed `file` or `cfile` card that fails the server's handler now resets
  the reply, sends the one error and ends card processing, as
  sync-protocol-01 Section 5.2 (SP-5.2-7) and fossil's `xfer_accept_file`
  path in `page_xfer` require (#390). Content that is not its HASH is
  answered `wrong hash on received artifact: HASH`, a HASH or DELTASRC that
  is no artifact name `malformed file line` (or `cfile`), and a storage
  failure its error; each was appended to the reply and the next card
  handled. A `file` delta whose basis the server holds is now applied and
  checked against HASH before it is stored, the pre-storage verification
  Section 7.5 gives a reconstructable non-clone file. A `cfile` is checked
  too, where fossil(1) 2.28 stores it unchecked and aborts at commit; the
  wrong-hash cases are not sent to fossil(1) for that reason.

- The server sends a clone's reply (the unversioned catalog under uv-sync,
  the snapshot cookie, the cfile batch, `clone_seqno` and the push card) at
  the clone card, and the cluster igots of `pragma req-clusters` at the
  pragma, as fossil's `page_xfer` does. A later reset, from a bad command or
  a refused push, now discards them, so the reply is the error alone
  (sync-protocol-01 Section 10.1, #389). They were sent after the card loop
  and survived the reset. The clone snapshot cookie is read before the card
  loop, since fossil's client sends it after the clone card; a
  `pragma send-private` must now precede the clone card to include private
  content, as in fossil.

- `pragma ci-lock` and `pragma ci-unlock` are applied for any requester, with
  no Write-capability test, as sync-protocol-01 Section 9.4 and fossil's
  `page_xfer` do (#392). The lock table stays bounded without that gate: each
  `ci-lock` first deletes every lock whose check-in is not a leaf, so at most
  one lock per leaf and one other survive. nobody's lock row stores a null
  login, as fossil's does, and `ci-lock-fail` names such a holder `nobody`,
  since -01's grammar requires a LOGIN token where fossil sends an empty one.
  The LOGIN in `ci-lock-fail` is fossilized on the server and defossilized on
  the client. The `lock-timeout` setting is read as fossil's `db_get_int`
  reads it: a value of 0 or less, or one that is not a number (read as 0),
  makes every lock stale, where it previously fell back to 60 seconds.

- The sync client answers a server's `gimme` for content it holds as
  private, outside a private sync, with `igot HASH 1` when the server's
  `pragma server-version` dates it 2020-04-13 or later, and with nothing for
  an older or undated server, as sync-protocol-01 Section 8.6 and fossil's
  `send_file` do. Such an answer to the first reply continues the session
  once (Section 3.7 limb c). It previously sent nothing.
- The `uvfile`, `uvigot` and `uvgimme` NAME is fossilized on the wire, as
  sync-protocol-01 Section 7.3 and Appendix B require: a name with a space
  goes out as `a\sb.txt`, and a received NAME is defossilized before it is
  checked as a pathname and stored. fossil(1) 2.28 sends the name raw and
  checks the token as sent, refusing any escape; names that need no escaping
  are unchanged on the wire, and the code follows -01. A raw backslash escape
  in a received NAME is now undone, where it was refused. The `uv-hash`
  catalog hashes and sorts each name in its fossilized form (Section 8.5.1),
  which is what fossil(1) hashes for a name it holds as sent.
- The server's check-in locks share fossil's lock table: a lock is the config
  row `ci-lock-HASH` holding JSON `login` and `clientid`, timed by the row's
  `mtime`, where go-libfossil wrote `libfossil-ci-lock-HASH`, so a repository
  served by both saw two tables. A live lock is now kept as it is, holder and
  time, when its own client asks again, where it was refreshed (Section 9.4),
  and a lock is stale once it is the lock timeout old, not only after. The
  timeout is the repository's `lock-timeout` setting, 60 seconds when unset,
  as in fossil. Rows left under the old `libfossil-ci-lock-` key are deleted
  the next time a lock is requested. `pragma ci-lock` is taken only with
  exactly a hash and a client id, as in fossil, where it wrote a lock for any
  two tokens. The server still ignores `ci-lock` and `ci-unlock` from a
  requester without Write capability, where Section 9.4 applies them to any
  (#392).
- The sync client's unversioned-file session now stops after request 3 when
  that request's `uvgimme` brings back no `uvfile`, as Section 3.7(d) and
  fossil's `nCycle<3` do; it ran one request more.
- Looking up one file in a delta manifest follows artifact-format-01 Section
  8.4: the delta's own F cards are searched first, and a delete marker answers
  not-found without reading the baseline, so a deleted name no longer fails
  when the baseline is missing. A name the delta does not mention still
  needs the baseline and fails without it. The new `manifest.LookupFile`
  serves `Repo.ReadFile`, `manifest.FileAt`, annotate, the merge resolver and
  conflict-file reads, and no longer lists the whole file set for one name.
- `deck.Deck.Marshal` writes the K card's ticket ID (Section 4.7.9) and the R
  card's checksum (Section 6.3.4) in lowercase, as those sections require of a
  writer, even when the deck came from an artifact whose parser-accepted card
  was uppercase.
- The sync server handles request cards in request order, as fossil's
  `page_xfer` does (#385). Before this, the login card was verified first and
  every control card was handled ahead of the data cards. So a file card
  sent before its push card was stored, and cards sent before a successful
  login got the login's capabilities. Now that file card is refused with
  `not authorized to write`, and cards before the login get nobody's
  capabilities. fossil rolls back the whole request when it reaches an
  invalid clone sequence (sync-protocol-01 Section 8.1). Here a request that
  holds such a clone card is first walked without side effects, so no
  earlier card writes before the fatal.
- A push refused without an earlier pull now resets the reply and is
  answered `error not authorized to write`, and card processing continues.
  A refused pull resets, is answered `error not authorized to read` and ends
  card processing. These are the sync-protocol-01 Section 6.6 texts. They
  replace `push denied: insufficient capabilities` and `pull denied:
  insufficient capabilities`, which neither reset nor stopped (#385).
- After an accepted push the server now asks for its own phantoms, as
  fossil's `request_phantoms(&xfer, 500)` does. It sends at most 500 gimme
  cards, ahead of a pull's igot inventory, and also after a broken card
  loop (Section 6.4). Phantoms the client asked for this round, shunned names
  and private ones outside a private sync are left out. A pushed igot now
  makes a phantom, as Section 8.5 says, instead of drawing a gimme on the
  spot. So a file card later in the same request fills the phantom without
  being asked for. An igot or gimme whose name is not an artifact name
  is a bad command, as in fossil (#385). Unlike fossil's validate16, an
  uppercase hex name is refused too, since the blob table only holds
  lowercase names. A run of igot cards stores its phantoms in one
  transaction. Still open: #389, #390, #391.
- A clone no longer aborts on a reply card it cannot read (Sections 5.2 and
  10.1). An unknown or malformed card ends the reply-card loop, and the
  cards before it are kept. The session continues, including the mandatory
  second clone round, and then fails naming the card, as the pull path
  already does after #373 (#385).
- The leaf table reads a check-in's branch through its active branch row
  only (tagtype > 0), on both the crosslink sweep and the commit path, as
  repo-state-01 Section 6.4 requires of every branch-membership test. A
  check-in whose branch tag was cancelled with a value no longer stays a leaf
  on that value's account. fossil 2.28's `leaf_check` reads the row unfiltered,
  so this departs from fossil; its `is_a_leaf` and `leaf_rebuild` filter.
- A check-in with no active branch tag is attributed to the `main-branch`
  setting, `trunk` only when it is unset, as repo-state-01 Section 6.1
  requires and fossil's `branch_of_rid` does. This affects `root:`, `start:`
  and `merge-in:` resolution, `checkout.LeafAmbiguous`, and
  `checkout.BranchLeaves("")`, which now lists the default branch.
  `repo.MainBranch` reads the setting; an empty value counts as unset, which
  departs from the draft and from fossil's `db_get`. The leaf table and
  `checkout.WouldFork` apply the same default to a check-in with no active
  branch row, where fossil's `leaf.c` uses the literal `trunk`.
- `verify.Verify` and `verify.Rebuild` report a blob whose bytes do not hash
  to its stored name as `IssueHashMismatch`, not `IssueBlobCorrupt`.
  `content.Expand` checks the name itself and its error now wraps
  `content.ErrHashMismatch`, so the report can keep the two apart, as
  repo-state-01 Section 4.4 asks.
- `deck.Parse` accepts every Julian-day spelling artifact-format-01 Section
  4.6.4 form 5 allows: a sign, an exponent, a leading point and a trailing
  point. Hex, `inf`, `nan` and `_` stay rejected, and the bound is now
  checked after rounding to the millisecond, so `5373484.4999999999` and
  `1e-10` are rejected as in fossil (#370).
- `manifest.ListFiles` and crosslink accept a delta manifest whose B card
  names another delta manifest, as artifact-format-01 Section 4.7.3 requires
  and fossil's manifest_get does. The file set is the named delta's own F
  cards overlaid by this manifest's, one level, without following the named
  delta's B card. This reverses the refusal added for #275; -00 forbade it
  and -01 reverses that, so the code follows -01. The named delta's delete
  markers are dropped from the set, and the commit writer never emits a B
  card naming a delta manifest (#371).
- A check-in's event time now follows repo-state-01 §4.2 step 6 whatever order
  the check-in and its `date` tags arrive in (#372). Crosslinking a check-in
  read its `date` tagxref row whatever its source, so re-crosslinking after a
  propagating `date` tag retimed every descendant that had inherited it; only
  a tag applied directly to the check-in now sets its time. A cancelled `date`
  tag now restores the D-card time, and a value that does not parse, or parses
  (by SQLite julianday) to a time that is not positive, is ignored and keeps the D-card time,
  where go-libfossil, like fossil 2.28, stored no time or a time of 0. The
  rule lives in one place, `tag.RefreshEventTime`, which both tag application
  and crosslinking call; `event.omtime` still keeps the D-card time. This
  departs from fossil 2.28, whose result for these cases depends on arrival
  order. The `date-tag-cancel`, `-unparsable` and `-zero` conformance cases
  are no longer marked as deviations, and new `-before-checkin` cases check
  the same three with the tags arriving while the check-in is a phantom.
- The sync client no longer sends an unversioned file named by SHA3 to a
  server whose `pragma server-version` predates SHA3 support (#303). It sends
  `error Fossil\sversion\s2.0\sor\slater\srequired.` in its place, as
  sync-protocol-01 Section 7.6 and fossil's `send_unversioned_file` do,
  whether the card would carry content or not; a deletion tombstone names no
  hash and still goes. Artifacts were already gated this way.
- Outside a private sync, the sync server answers a gimme for private content
  with `igot HASH 1` when the peer's `pragma client-version` dates it
  2020-04-13 or later, and with nothing for an earlier or missing date, as
  sync-protocol-01 Section 8.6 and fossil's `send_file` do (#375). It used to
  send nothing whatever the date. The server now records the peer's check-in
  date from the pragma.
- The sync server's refusals now follow sync-protocol-01 Sections 5.3, 6.4 and
  6.6 (#374). A file or cfile card sent without an accepted push resets the
  reply, is refused with `error not\sauthorized\sto\swrite` and ends card
  processing, where it used to be refused alone (`file HASH rejected: no push
  card`) and the cards after it still ran. A refused login (a wrong password,
  or a second login card) ends card processing at its own position instead of
  ending the request: the cards before it keep their effect, the igot
  inventory of an accepted pull still follows the error, and the reply ends
  with the timestamp comment. `errors N` in that comment now counts the errors
  raised, including those a later reset discarded, and leaves out the
  bad-command error, the private-content refusal and the SHA3 refusal, as
  fossil's `nErr` does.
- A file, cfile or uvfile payload cut short by the end of the body is now the
  card's handler failure, as sync-protocol-01 Section 5.1 requires (#373). The
  decoder used to fail the whole message, so a server answered HTTP 400 and a
  client dropped the reply. It now yields the card as a malformed line and
  stops: a server answers 200 with `error malformed\sfile\sline` (or the
  cfile or uvfile text) and keeps the effect of the cards before it, and a
  client keeps the prefix it has processed and records the error. The short
  payload is never stored. This follows -01, not fossil 2.28, which stores the
  bytes present.
- Tests that run `fossil server` no longer flake with "directory not empty"
  during temp-dir cleanup (#352). Fossil forks a child per connection, and
  killing only the parent let a child write into the directory while it was
  being removed. They now share `testutil.StartServer`, which puts the server
  in its own process group and kills the whole group before reaping it.
- A check-in's `event.bgcolor`, `euser` and `ecomment` now follow its
  `bgcolor`, `user` and `comment` tags as fossil's `tag_insert` and
  `manifest_crosslink` do (#343). `tag.ApplyTagWithTx` set `event.bgcolor` only
  for a propagating `bgcolor` tag, so a singleton one did nothing and a cancel
  left the colour in place, and it never touched `euser` or `ecomment`. It now
  sets all three for every tag type, writing NULL for a cancel, and crosslinking
  a check-in reads the three values back from `tagxref`, so a tag that reached
  the check-in while it was a phantom takes effect.
- The sync server's `xtable-hash` now needs a direction card (#351), not
  Read alone. Read is derived for a Clone-only requester (#312), which then
  received an inventory of table keys and mtimes it can neither pull (xgimme
  needs a pull card) nor push against. The gate is Read plus an accepted pull
  or push card: a push-only table-sync client still gets the inventory it
  uses to queue newer local rows. Table sync is a go-libfossil extension
  with no fossil counterpart. The private-clone card order the issue also
  raised needs no change: the clone client has no private mode and never
  sent `pragma send-private`; a comment records fossil's order for when one
  is added.

- The sync client's per-request `gimme` limit now follows fossil's
  `mxPhantomReq` exactly (#350). The cap itself (200, or twice the files the
  last round received) was already in place, but it was recomputed every
  round; fossil assigns it only when a round continues for its phantoms and
  otherwise keeps the earlier value, so a round that continues to send files
  no longer drops a raised limit back to 200. Tests pin the 200 start and the
  growth rule.

- The sync server no longer asks for a shunned artifact (#349). `handleIGot`
  answered an `igot` for a shunned hash with `gimme`; fossil's `content_new`
  returns 0 for a shunned name, so it makes no phantom and requests nothing.
  Private igot emission is deliberately unchanged: fossil's `send_private`
  has no shun filter, so a shunned private artifact is still announced
  (contrary to what #349 proposed); a conformance case pins this.

- Tests that run the fossil binary no longer write into the developer's
  global fossil config (`~/.fossil` or `~/.config/fossil.db`) (#284). Each
  `fossil new`, `open`, `clone` and `serve` on a temporary repository left a
  `repo:` or `ckout:` row there that was never removed, and parallel package
  runs contended for that file's lock. `testutil.RequireFossilBin` now points
  `HOME`, `FOSSIL_HOME` and `XDG_CONFIG_HOME` at a per-test directory through
  the new `testutil.IsolateFossilHome`, and the tests that resolved fossil
  with `exec.LookPath` use it. `TestFossilInvocationsIsolateHome` fails when a
  Go file runs `exec.Command` without one of these.

- `Checkout.WouldFork` again reports a fork when the checkout's version
  already has a child on its own branch (#347), as after a pull brings in a
  child of the checked-out version. Since #291 it was fossil's leaf-ambiguity
  test, which answers no for a version that is not a leaf, so a commit there
  created a fork. It is now fossil's commit-time "Would fork" check
  (`!is_a_leaf`); another open leaf elsewhere on the branch no longer counts,
  since a commit extends the checkout's own leaf. The repo-state §7.5
  ambiguity test is `checkout.LeafAmbiguous`.

- `branch.Create` refuses an open branch name inside the check-in's own
  transaction (#316). The check ran before `manifest.Checkin` opened its
  transaction, and the branch tags were applied in further transactions after
  it committed, so concurrent creates of one name could all pass the check and
  all commit. `manifest.Checkin` now applies its inline tags, parent tag
  propagation and leaf state inside its transaction, and takes a `Precheck`
  that runs first in it; `branch.Create` checks the name there, so exactly one
  of several concurrent creates commits.

- `verify.Rebuild` is atomic again (#335). Since #285 it committed the emptied
  derived tables before the crosslink sweep ran, so a sweep that failed partway
  left event, plink, mlink, tagxref, leaf and the rest empty until a later
  rebuild succeeded. It now empties, re-derives and refills the bookkeeping
  tables in one transaction, as fossil's `rebuild_db` does, and a failed
  rebuild rolls back to the state it found. `manifest.CrosslinkRebuild` now
  takes the caller's transaction and runs candidate discovery, linking and the
  leaf and tag repair inside it; the incremental `Crosslink` path is unchanged.

- A check-in's `event.mtime` now follows its newest `date` tag, the one-shot
  tag `fossil amend --date` writes, as fossil's `manifest_crosslink` and
  `tag_insert` do (#334). Crosslink used the manifest's D-card time and never
  applied the override, so every date-amended check-in sorted at its original
  time in the timeline, `tip` and branch listings. Crosslinking a check-in now
  reads a date tag already recorded for it, and applying a date tag later moves
  the check-in's `event.mtime`, keeping the original in `omtime`. `tag.AddTag`
  now derives its tag through the same path crosslink uses, so it moves
  `event.mtime` and `event.bgcolor` too.

- `verify.Rebuild` now derives repository state through the crosslink sweep
  rather than its own partial derivation (#285). It kept a second derivation
  that wrote rows only for check-ins and control artifacts: no cluster, wiki,
  ticket, technote or forum events, applied tags from artifacts the crosslink
  would refuse, skipped absent tag targets and parents instead of reserving
  phantoms, wrote no plink `baseid`, and computed leaves by "no child at all"
  regardless of branch. Rebuild now empties the derived tables and runs the
  same sweep a clone runs, so derived state has one owner and matches
  `fossil rebuild` in event, plink, mlink, tagxref, leaf and phantom. A new
  `manifest.CrosslinkRebuild` runs that sweep in a terminal mode that links a
  check-in whose content is absent with phantoms rather than deferring it for a
  blob that will never arrive, as `fossil rebuild` does. Its branch-aware leaf
  rule now follows fossil, so an untagged parentless check-in is no longer a
  leaf (the §7.3 erratum the sweep already observed).

- The sync server lets Clone imply Read at its read gates, as fossil's
  trunk `page_xfer` and the §7.3 erratum do (#312). This is an access
  change: a requester whose anonymous (nobody) capabilities include Clone
  (`g`) now receives unversioned files (the catalog, `uvgimme` answers, and
  the catalog sent with a clone), the `xtable-hash` inventory and read-gated
  configuration from `reqconfig`. As in fossil, a Clone granted by a login
  card does not imply Read. Pull still needs Read itself (`o`): fossil
  trunk's derived Read also lets a Clone-only nobody pull, and this server
  deliberately does not. One handler check, `canRead`, now decides every
  read gate; `auth.CanRead` is unchanged.
- Sync login follows §6.4 and §6.5 (#304). Once a server's `server-version`
  names a release that reads it (2.27, built 2025-07-27 11:05 or later), the
  client's HTTP transports send the login card in the `x-f-l-c` cookie,
  urlencoded, instead of the body, as fossil's client does; other transports
  keep it in the body. The server, when a user's stored password is not a
  40-character shared secret, checks the signature again against the secret
  that password derives, as fossil's `check_login` does for a password stored
  in clear. A new `HandleOpts.LoginCookie` field carries the cookie to
  `HandleSyncWithOpts` for an embedder that serves sync over its own HTTP
  handler; without it, a client's cookie login reaches such a server as
  nobody. `XferHandler` and `ServeHTTP` read the cookie themselves.
- Unversioned sync applies a received `uvfile` as fossil's
  `xfer_accept_unversioned_file` does, on both sides (§7.3, #308). The row
  keeps the HASH the card gave rather than one the repository's hash policy
  computes, so both catalogs agree; the same content at a later mtime
  (status 2) moves only the mtime, where it was dropped; and a card that
  omits its content stores the row with NULL content and size 0, where it
  changed nothing. The server answers a `uvgimme` for a name it does not
  hold with no card, where it sent a tombstone. The client no longer seeds
  its send table with tombstones (§8.5.1). A received deletion of a name the
  repository does not hold changes nothing, as fossil's update does. A
  `uvfile` line whose HASH is neither an artifact name nor `-`, or whose
  NAME is not a strict simple pathname, is malformed and refused, where its
  HASH was stored as given; and only a card that carries content has its
  HASH checked, so a deletion or a row stored without content relays.
- Clone replies follow fossil's `page_xfer` (#306). Each `cfile` carries
  the artifact as the blob table stores it, verbatim, followed by an LF when
  its payload does not end in one (§5.1, §7.2), where the server
  recompressed it and never padded. Over HTTP a version 3 clone's reply is
  `application/x-fossil-uncompressed` (§4.1). With the `uv-sync` setting on,
  a clone reply carries the unversioned catalog under `pragma uv-pull-only`
  for a requester with Read (§8.4). A clone refused for want of Clone resets
  the reply and sends the repository's `push` card ahead of `error not
  authorized to clone` (§8.2, §10.1), where the error was appended to what
  the reply held. A clone whose SEQNO is negative gets no content and
  `clone_seqno 0`, where it restarted at the first artifact.
- The sync server answers three more cards as fossil does (#305). A pushed
  `config` card from a requester without Admin is refused with `error not
  authorized to push configuration`, reset and break (§9.3); one from an
  Admin is refused as unsupported, since this server does not apply pushed
  configuration, where both were silently ignored. `pragma ci-unlock`
  releases the client's check-in locks (§9.4), for writers, as `ci-lock` is
  taken. `pragma req-clusters` is answered with the cluster `igot` cards
  between `# sending-clusters` and `# end-of-clusters` (§5.3); it was
  recorded and never answered. This is an access change: as in fossil, any
  requester, one without Read included, is sent the names of the public
  cluster artifacts. Their content still needs Read to fetch.
- Sync exchanges releases and withholds SHA3 content from older peers, as
  fossil does (§7.6, §9.1, #303). The server answers a `pragma
  client-version` that carries DATE and TIME with its own `pragma
  server-version`, ahead of any content. Both sides now record the peer's
  release, and where they would send content named by SHA3 to a peer older
  than fossil 2.0 (a client that announces no release counts as one), they
  send `error Fossil version 2.0 or later required.` in its place: the
  server for `gimme`, clone and `uvgimme`, the client for `gimme`. Because a
  server-version from a current release has fossil's client log in through
  the `x-f-l-c` cookie, the HTTP server now reads that cookie's login card,
  its nonce covering the whole body (§6.5).
- The sync client ends a session by §3.7's rules, as fossil's `client_sync`
  does (#301). Stored content or a new phantom continues it only while the
  phantom table is not empty, so neither received content alone nor
  phantoms the server cannot supply keep it going; a phantom used to be asked
  for until it had aged three rounds. A `gimme` for content the client does
  not hold, or may not send, is dropped where it stayed queued and held the
  session open until `MaxRounds`. An `igot` for a missing artifact now
  records a phantom in the phantom table, so a later session still asks for
  it. After a clone's first round, a server `error` ends that reply and the
  clone continues as the draft says, with a cleanup round, and `Clone` then
  fails naming the error. Phantoms a pull fills are crosslinked once per
  reply instead of in one transaction each.
- The sync client places `pragma send-private` where fossil's `client_sync`
  does (§3.2, #307): first in its first request and right after
  `client-version` in later ones, where it trailed the content cards. A
  delta's missing basis received under a `private` card becomes a private
  phantom, as fossil's `content_new` makes it, where it became public.
- Sync honors the shun list as fossil does (§3.3, §7.7, #302). A received
  `file` or `cfile` whose hash is shunned is dropped without an error and
  not counted as received, by the server and the sync client, where each
  stored it; a clone drops a shunned `cfile` but keeps a shunned `file`, as
  fossil's `!cloneFlag` exemption does. The client no longer asks for a
  shunned phantom: the phantom scan, an `igot` and a clone's delta basis all
  leave shunned names out, so a delta whose basis is shunned is kept and the
  basis is never requested. `shun.IsShunned` now reports a failed lookup as
  an error instead of "not shunned".
- The sync server stores a `uvfile` sent without a `push` card, as
  `fossil uv sync` sends it (§7.3, #296). It answered each one with
  `rejected: no push card`, so pushing unversioned files from fossil failed.
  This is an access change: a user with only the unversioned-write capability `y`
  can now push unversioned files, as in fossil; Write is no longer also
  required. A refused `uvfile` (no `y`, or a HASH that does not match its
  content) now resets the reply and ends the card loop, as fossil does.
- The sync server refuses a `push` or `pull` card naming another project
  with `error wrong project`, as fossil's `page_xfer` does (§8.5, #309). It
  accepted either whatever its project code, so a client of an unrelated
  repository could pull its artifacts or push into it. The refusal comes
  before any capability check, resets the reply and ends card processing.
- Sync HTTP framing now follows §4 (#310). The server dispatches a request
  typed `application/x-fossil-debug`, which it refused with HTTP 415. The
  client takes only HTTP 200 as a sync reply, so a 201 or other 2xx is an
  error, and reads a reply's framing as fossil's `http.c` does: no
  Content-Type is the compressed container, an `x-fossil-debug` or
  `x-fossil-uncompressed` prefix is plain cards, any other `x-fossil` prefix
  is compressed, and a foreign type such as a proxy's HTML page is refused
  rather than read as cards. A server that writes `HandleSync`'s reply over
  HTTP must type it `application/x-fossil`.
- Unknown, wrong-direction and error cards now follow §5.2 and §10 on both
  sides of a sync (#300). The server answers a client-sent `error` or
  `message` card — reply-only cards fossil's `page_xfer` has no branch for —
  with `bad command`, resetting the reply, rather than ignoring it. The client
  ends its reply-card loop on an `error` card, keeping the cards before it, as
  fossil's `client_sync` does. A `message pull only ...` from the server
  (§10.2) now stops the client pushing: it keeps pulling but answers no
  further gimme and drops its push card one request later, matching fossil's
  clearing of `SYNC_PUSH`. To make that reachable, the client now sends its
  `pull` card before its `push` card (fossil's order), and the server answers
  a push from a requester who may read but not write with `message pull only -
  not authorized to push` instead of an error, so the pull still proceeds.

- The xfer card decoder now follows the draft's card grammar instead of
  failing a whole message on one bad card (#299). A line no card rule accepts
  (an unknown operator, a bare `pragma`, a `gimme` with a surplus token, a
  `config` with a signed size) decodes to a bad-command card: the server
  resets its reply, answers `error bad command: <LINE>` and parses on, and the
  client ends the reply-card loop keeping the cards before it. A `file`,
  `cfile` or `uvfile` line that cannot be framed decodes to a bad-file card:
  the receiver emits the handler's message (`malformed file line` and the
  like) and stops, as fossil's `page_xfer` breaks. Tokens are now split on
  ASCII whitespace only, within fossil's six-token window, so a no-break space
  no longer separates tokens and a seventh token is ignored rather than
  rejected; `igot` and `private` tolerate a surplus token. A full `cfile`'s
  USIZE is treated as metadata, not checked against the inflated size, and the
  byte after a `config` payload is consumed whatever it is, both matching
  fossil.

- `Repo.ResolveVersion` now follows fossil's `symbolic_name_to_rid` name
  resolution (src/name.c): it chooses a tag by the carrying check-in's
  `event.mtime` rather than `tagxref.mtime`, no longer falls back to the tip
  when no check-in carries `sym-trunk`, tries an artifact-hash prefix before a
  plain tag, and accepts the `rid:`, `tag:`, `root:`, `start:`, `merge-in:` and
  `NAME:TIMESTAMP` forms plus a trailing-punctuation retry. A hex prefix now
  must be 4–64 characters, as fossil requires (#290).
- `branch.Create` now refuses a name an open branch already carries, as
  `fossil branch new` does, and stores no artifact when it refuses. `branch.List`
  now reports a branch closed only when its newest check-in carries a `closed`
  tag (fossil's branch-list rule), not when any check-in on the branch does
  (#292).
- `checkout.BranchLeaves` now returns only the *open* leaves of a branch,
  skipping any leaf that carries a `closed` tag, as fossil's `leaves` lists
  them (#291).

- A repository with no `hash-policy` row names new content as fossil's
  `hname_default_policy` does (#274): SHA3 when it is empty or holds a SHA3
  artifact, SHA1 (policy `auto`) otherwise. It used to name an empty
  repository's first artifact with SHA1. And when an `auto` repository takes
  in its first SHA3-named artifact, the promotion is written to the
  `hash-policy` setting, as fossil's `content_put` does, so fossil opening
  the repository agrees. It used to be derived from the stored names only.

- `ListFiles` and crosslink reject a B card whose target is not a baseline
  manifest (#275): a delta manifest or a control artifact used to be accepted
  as the baseline, giving a partial or empty file set instead of an error.

- `Checkin` refuses a filename the artifact format's writer profile forbids
  (#270): one containing a backslash, or that is not valid UTF-8
  (surrogates and overlong forms included). It used to write a manifest
  naming it. fossil's own `add` refuses such names, so fossil never creates
  that manifest, but its reader still accepts one that arrives from
  elsewhere. The draft's writer profile also bars characters above U+FFFF,
  private-use characters and noncharacters; those are not enforced here.

- `delta.Create` refuses a source or target of 2^32 bytes or more with
  `delta.ErrTooLarge` (#279), as the delta format's producer bound requires;
  it used to write header and copy integers of 2^32 or more. `Create` now
  returns an error, and `Deltify` stores such content whole, as fossil does
  when a delta is not worthwhile. `Apply` now rejects a header naming exactly
  2^32 as well (the draft leaves that limit implementation-defined), so what
  `Create` writes and `Apply` reads agree.

- Storing content whose name is a phantom fills the phantom in place, as
  fossil's `content_put` does (#278). `blob.Store` used to return the
  phantom's rid and leave it empty, so a check-in of a file matching a known
  phantom recorded a manifest pointing at content that was never stored.

- `Repo.Tag` now places a tag name in the symbolic namespace with a `sym-`
  prefix and refuses the reserved `wiki-`, `tkt-` and `event-` prefixes, as
  `fossil tag add` does for a check-in. It used to store the name verbatim, so
  the tag did not resolve as a branch/tag name. Callers pass the bare name.


- Cancelling a propagated tag no longer deletes an inherited row that is newer
  than the cancellation. The cancel walk carried no mtime guard
  (`COALESCE(srcid=0, 0)`); it now matches fossil's `tag_propagate`
  (`srcid=0 AND tagxref.mtime < ?`), removing an inherited row only when it is
  older and halting the descent at a newer one (#288).


- The singleton `parent` (reparent) tag is now applied: the tagged check-in's
  plink and mlink rows are rewritten to the parents the tag names, and the
  affected leaves are recomputed, as fossil's `manifest_crosslink_end` does.
  An old parent left without a child becomes a leaf, matching `fossil rebuild`.
  Both the crosslink sweep and `verify.Rebuild` apply it (#286).


- A technote whose T-card names another artifact, or propagates or cancels,
  is now refused at crosslink time and applies none of its tags, as fossil
  refuses it at parse time. A technote may only carry `+` (singleton) tags on
  itself (#287).


- Crosslinking a root check-in a second time no longer duplicates its mlink
  rows. The root-file path inserted each row with no "already derived" guard,
  so a re-crosslink (as the relink path does after clearing event/tagxref)
  doubled them; it now skips the block when the rows are already present, as
  `manifest_crosslink` is idempotent (#294).


- The first check-in's `sym-trunk` tag is now propagating (`*sym-trunk`),
  matching `fossil new` and repo-state §6.1, so the trunk alias descends to
  every check-in on the branch. It used to be written as a singleton
  (`+sym-trunk`), which stopped at the root (#289).

- `deck.Parse` applies the §5 structural-type rules once a type is determined:
  the §5.2 card-legality table (each type's allowed and required cards, from
  fossil's `manifestCardTypes`), so a card the type disallows or a required
  card left out is rejected, and the §5.4 semantic checks -- a control
  artifact's tags may not target itself, a technote's tags are all
  self-referential singletons, a forum post carries at most one of an H or I
  card and at most one parent. Type determination now falls back to a control
  artifact (not an empty check-in) when no card sets a type and there is no
  comment, file or parent, matching fossil. This also makes a missing Z card a
  required-card rejection rather than a special case (#273).


- `deck.Marshal` re-encodes what `deck.Parse` read, so `Marshal(Parse(x)) == x`
  for a canonical artifact. A present-but-empty C comment, N mimetype or T
  value, and a bare P card, are kept distinct from an absent one and re-emitted
  (an empty W body already was, now via nil versus a non-nil empty slice); the
  distinction is carried in unexported Deck/TagCard fields, so a caller that
  builds a deck and sets a non-empty value still has it marshalled and the
  public API is unchanged. The D and E timestamps are written at their writer
  precision -- millisecond for D, second for E -- extended to keep any finer
  fraction the parser read, so a sub-second timestamp round-trips instead of
  being truncated (#272).


- `deck.Parse` parses D and E timestamps with the §4.6.4 grammar (as amended
  by the §4.6.4 erratum) instead of two fixed `time.Parse` layouts. It accepts
  the date-time form with `HH:MM`, `HH:MM:SS` or a fractional second, a `Z`,
  `z`, `+HH:MM` or `-HH:MM` timezone, one or more whitespace-or-`T` separators,
  the hour 24 and an unchecked day (`2024-02-31` rolls forward, as SQLite's
  `julianday()` does), and a date at or after `-4713-11-25`; it applies the
  exact-digit field ranges (year 4 digits, month 01-12, day 01-31, hour 00-24,
  minute and second 00-59) and rejects a lowercase `t` separator rather than
  silently rewriting it to `T` (#271).


- `deck.Parse` applies fossil's §4.6 name predicates. An F-card filename and
  old-name, and an A-card filename, must be simple pathnames (§4.6.1): no
  leading or doubled or trailing slash and no "." or ".." path element, the
  non-strict form fossil uses, which still admits a backslash or non-UTF-8
  byte. An L-card title and an A-card target (when not a hash) must be
  well-formed wiki names (§4.6.2): 1 to 100 bytes, no leading, trailing or
  doubled space and no control byte. A reserved repository filename
  (`_FOSSIL_`, `.fslckout`, in either case, with an optional -wal/-shm/-journal
  suffix, at the root or as a path's last component) is silently dropped from
  an F-card list after the sort-order check, exactly as fossil does (#269).


- `deck.Parse` validates every artifact-hash position against the §4.7.1 map:
  the A source, B, the E identifier, each F hash, G, I, M, every P token, both
  Q tokens, and the T target must be 40 or 64 case-insensitive hexadecimal
  characters; the K ticket id is pinned to 40; the R checksum to 32. A hash is
  required on every F card of a baseline manifest (only a delta manifest may
  carry a hashless deletion card). The A target and the N mimetype are now
  escape-decoded on parse and re-encoded on marshal, matching §4.3.1, while
  the other hash tokens stay raw; the A filename keeps only its final path
  component. The Q sign must be `+` or `-`, a tag name that is entirely
  hexadecimal (either case) is rejected, and these checks reject malformed
  tokens fossil also rejects, such as a hash carrying a stray carriage return
  (#268).


- `deck.Parse` tokenizes artifact cards the way fossil does (its
  `next_token`/`next_card`): tokens are separated by a single space, a card
  ends at LF, and tab and CR are ordinary token bytes rather than
  whitespace to split or trim on. A blank line, an empty token between two
  others (two spaces in a row), a bare card where a value is required, and
  trailing residue after the last card are each rejected; a line that ends
  in a space still carries a present-but-empty last value. The W card's size
  is read as fossil reads it -- leading decimal digits, with an overflow
  guard -- and the body must be followed by a newline. The clearsign armor
  trailer is matched on 29 bytes, as fossil matches it, so a mangled final
  dash no longer hides the signature. Each card is also held to its exact
  §4.2.1 arity: a card with more tokens than Table 2 allows is rejected
  (#266, #267).


- `deck.Parse` and `deck.VerifyZ` judge an artifact's framing by the rule it
  breaks. The Z-card check now mirrors fossil's `verify_z_card`: it locates
  the Z card as the last 35 bytes beginning `Z `, never reads the final byte,
  treats a body too short or without a trailing Z card as "Z card absent", and
  reports only a present-but-wrong checksum. `Parse` gained the §4.1 framing
  gates fossil applies up front -- a non-empty blob ending in LF, checked on
  the raw bytes before the signature wrapper is stripped, and a first card
  that is an upper-case letter followed by a space -- and a missing Z card is
  now rejected by the required-card rule rather than by the checksum. Artifacts
  rejected for a missing final LF, residue after the Z card, or a bad first
  line are no longer blamed on the checksum (#265).


## [0.11.1] - 2026-10-11

### Security

- The sync server sends repository content only to a requester allowed to
  read it (GHSA-mvx3-6793-8x4r). It used to answer `gimme HASH` with the
  artifact whether or not a pull was accepted, and `pragma uv-hash`,
  `uvgimme` and `uvigot` with the unversioned catalog and files, to any
  requester, anonymous ones included. As in fossil's `page_xfer`, `gimme` is
  now answered only in a session whose `pull` card was accepted, or in a
  clone, and the unversioned catalog and files go only to a requester with
  Read (o, i, or Admin). `uv-push-ok` is sent only to a requester who may
  write unversioned files (y); a reader without it gets `uv-pull-only`, as
  fossil sends. The same rules apply to table sync: `xgimme` needs an accepted
  pull and `pragma xtable-hash` needs Read. A `uvigot` no longer updates an
  unversioned file's mtime for a requester without y, and `pragma ci-lock`
  takes or reports a check-in lock only for a requester with Write (i), so an
  anonymous requester can no longer learn who holds a lock. This is a
  deliberate departure from fossil, which takes a lock for any requester;
  fossil's own client asks for one only before a commit, which needs Write.
- The HTTP sync transports follow redirects as fossil does rather than as
  Go's `net/http` does (GHSA-3f8x-8fr5-mr98). They used to follow a redirect
  from HTTPS to plain HTTP, which resent the request, login card included, in
  cleartext. They now refuse it, and follow a 301, 302, 307 or 308 by
  re-POSTing the same body and Content-Type, with one trailing `/xfer`
  stripped from the new location, failing on the twentieth redirect. A 301 or
  302 used to become a GET without the body, and a chain stopped after 10.
  This applies to `NewHTTPTransport`, whatever `http.Client` it is given.

## [0.11.0] - 2026-10-10

### Added

- `Checkout.Merge` merges another checkin into the checkout, as fossil's
  `merge` does, and `Checkout.Conflicts` lists the files left in conflict
  (#244). The merge is recorded the way fossil records one, so fossil reads
  it and commits it, and the next `Checkin` records the merged-in checkin
  as a merge parent. A file edited here but deleted in the merged-in
  version is kept rather than deleted, and a merge that would overwrite an
  untracked file is refused: go-libfossil has no undo copy to recover them
  from. A change the merged-in version made to a file's executable bit is
  taken. Renames are not followed yet.
- `CheckoutCommitOpts.Files` commits only the named files, and
  `CheckoutCommitOpts.AllowConflict` commits files that still hold conflict
  markers (#244).

### Changed

- Merge conflicts use fossil's conflict markers, with the common ancestor
  shown between the two sides, so fossil sees a conflict go-libfossil wrote,
  and the other way round (#244). This applies to `Update` as well as
  `Merge`. A file is in conflict while it holds markers; there is no
  separate conflict state to clear.
- The CLI's `merge` command calls `Checkout.Merge` (#244). It merges into
  the checked-out version; it used to merge against the repository's newest
  checkin, which made merging a branch that is the newest checkin a no-op.
  It no longer writes `.LOCAL`, `.BASELINE` and `.MERGE` files beside a
  conflicted file, since the markers now include the common ancestor.
  `mark-resolved` refuses a file that still holds markers, and `conflicts`
  lists the files that hold them.
- **Breaking (CLI):** `repo ci` commits the checkout, as `fossil commit`
  does (#244). It used to commit the named files straight to the repository
  on top of its newest checkin, ignoring the checkout, so it could not
  record a merge and left the checkout behind. Run it in a checkout (or pass
  `-d`); with no files named it commits every change. The `--parent` flag
  is gone, since a checkout always commits onto its own version.
  `--allow-conflict` commits files that still hold conflict markers, and
  `--allow-empty` commits when nothing has changed.
- `Checkout.Checkin` refuses files that still hold merge conflict markers,
  as fossil's commit does, unless `AllowConflict` is set (#244).
- **Breaking:** `Checkout.Checkin` refuses a commit with nothing changed and
  no merge pending ("nothing has changed"), as fossil's commit does, unless
  `CheckoutCommitOpts.AllowEmpty` is set (#244). It used to record a checkin
  identical to its parent. A named file the checkout does not track is
  refused too; a named directory selects the tracked files under it.
- `stash.Save` refuses while a merge is pending (#244). A cleanly merged
  file looks like a plain edit; stashing it left the merge record naming
  content that was no longer in the checkout.

### Fixed

- A pull-only `Sync` converges when the client holds artifacts the server
  lacks (#277). The server used to answer the client's `igot` cards with
  `gimme` for them even though the client was not pushing, so the client
  never satisfied the request and the pull ran to `exceeded 100 rounds`. The
  server now asks only a pushing client, as fossil does, and a client that is
  not pushing ignores any `gimme` it receives.

- `Repo.Annotate` credits each line to the right check-in when edits shift
  line positions between versions (#253). Past the first parent it used to
  write each line's attribution by its position in an older version, so lines
  were credited to the wrong check-in, and an older version with more lines
  than the starting one panicked with an index out of range.
- Check-in, parent-link and tag rows store the values fossil stores (#258),
  whether written by a commit, a crosslink or `verify.Rebuild`. Times match
  bit for bit: tag times are SQLite's `julianday()`, and event, plink,
  attachment and forumpost times are that value as fossil writes it into
  SQL text, which keeps 16 digits; they used to differ in the last digits.
  Fossil's own text conversion depends on the platform it was built for,
  and this follows the macOS and Windows builds. A check-in's `omtime` is
  recorded (it was NULL), and a tag without a value is stored as NULL
  rather than an empty string. The CLI's `repo tag ls` reads such tags,
  which it used to misprint.
- Clone and sync exchanges follow draft-fossil-sync-protocol-00 (#255).
  A clone reply sends an artifact stored as a delta as a `cfile` naming its
  source, as fossil does in a version-3 clone; it used to send a `file`
  card. A clone session stops asking once the server sends
  `clone_seqno 0`, sending at most one cleanup request with no operation;
  it used to switch to `pull` to fetch missing artifacts, which the draft
  forbids. `Clone` now fetches any artifact still missing with a separate
  pull session afterwards, and still fails if one cannot be had: unlike
  the draft, which ends a clone with phantoms left, `Clone` returns only a
  complete repository. A clone the server stops before `clone_seqno 0`
  fails rather than returning part of a repository. A sync now asks once,
  in its first round, for the phantoms a repository already holds, as
  fossil's does, without letting one the server lacks hold it open. Every
  request ends with a random comment, not only those with a login, and
  every completed reply ends with fossil's timestamp comment.
- `Repo.Annotate` credits lines the way `fossil annotate` does (#256, #257).
  It now uses a port of fossil's own line diff and, like fossil, diffs each
  older version against the starting one. Where a line repeats (blank lines,
  braces), which copy counts as new depends on the diff's tie-breaking, and
  go-libfossil's diff used to pick differently, crediting such lines to other
  check-ins. Trailing blank lines are no longer dropped from the result. On
  the RFC, althttpd, pikchr and wapp repositories every annotated file now
  matches `fossil annotate` line for line. A line's text no longer includes a
  trailing carriage return, a leading byte-order mark is ignored, and a file
  fossil will not diff (one with a NUL byte, or a line over 32,767 bytes)
  returns an error. Annotate no longer allocates a table the size of both
  versions at every step.
- `Checkout.Checkin` records pending merges as merge parents, including
  merges made by the fossil binary (#244). It used to drop them, so a
  merge committed through go-libfossil looked like an ordinary commit.
  Committing only some files of a merge is refused, as fossil refuses it.
- `Checkout.Checkin` of a checkout with no version and nothing added
  returns an error instead of panicking (#244).
- The CLI's `merge` recorded a conflict as `chnged=5`, which fossil reads as
  "added by integrate", and recorded no merge parent (#244).

### Security

- A sync card whose size is not a plain decimal is a decoding error. A
  negative size, as in `file HASH -1`, used to panic the client or server
  reading the message, and a signed size such as `+5` was read as a size.
  Sizes follow draft-fossil-sync-protocol-00's `decimal` grammar and fossil's
  32-bit limit, and a `uvfile`'s FLAGS take the same rule, so negative FLAGS
  are rejected as the draft requires. A declared size no longer sets how much
  memory is allocated before the payload arrives: the buffer grows with the
  bytes read, so a large size over a short message fails without allocating
  it. A `cfile` payload may inflate only to its USIZE, or to a margin above
  USIZE for a delta, and not past its 4-byte size prefix, and all `cfile`
  payloads in one message may inflate to at most `MaxCFileInflateBytes`
  (1 GiB) together; a few megabytes of zlib could otherwise inflate to tens
  of gigabytes. A single artifact larger than that cannot be received as a
  `cfile` (GHSA-j99p-mw22-69c8).

- An `xrow` payload or `xdelete` PKData must be one JSON object whose values
  are scalars and whose primary-key values have the column's type; anything
  else is refused with an error, on the client and the server. JSON `null`
  used to panic the process, as did a primary-key value of the wrong type or
  a nested value. A client refuses an `xrow` for a table it has not
  registered (GHSA-j99p-mw22-69c8).

- A synced table's row can no longer rewrite the SQL that stores it. Keys of
  an `xrow` payload or `xdelete` PKData were written into `UpsertXRow`'s
  `INSERT` as column names unchecked, so a peer could choose the statement's
  columns and values, or select from other tables into a synced one. A
  peer's row must now use exactly the table's declared columns, and
  `UpsertXRow` refuses any key the table's registered schema does not
  declare. Every other function that writes a synced table or column name
  into SQL validates it first (GHSA-9fq6-h6xw-274g).

- The HTTP transports read at most `xfer.MaxBodyBytes` (65 MiB) of a reply
  and return `xfer.ErrBodyTooLarge` past it; a server streaming an endless
  reply used to be read into memory whole (GHSA-j99p-mw22-69c8).

- The sync server answered `reqconfig NAME` with any row of the
  `config` table, to any requester, including one with no capabilities, so
  it disclosed `project-code`, `server-code` and every local setting. It now
  answers as fossil does: only a `/GROUP` request, only for a requester with
  Read, and only with the names fossil transfers in that group. The user and
  subscriber tables also need Admin, which counts only for a verified login,
  and concealed addresses need RdAddr (`e`). A refused request gets no cards
  and no error, so the rest of the exchange goes on. Groups are sent in
  fossil's encoding, so `fossil clone` and `fossil config pull` against this
  server now receive the skin, project, ticket, shun and alias settings (GHSA-p6w4-4xqc-jhq2).

- The sync server checks a login card's nonce against the request body, as
  fossil does (GHSA-2g5p-q5f3-477p). The nonce must be the SHA-1 of the
  decompressed bytes after the login line; it was never checked, so a login
  card captured from one request authenticated any other request body. A
  refused login now ends the exchange with fossil's `error login failed`
  instead of carrying on as nobody, and a second login card is refused. A
  successful login adds the user's capabilities to nobody's instead of
  replacing them. As in fossil, `nobody` and `anonymous` log in without a
  password, and accounts with no stored password, `developer` and `reader`
  cannot be logged into.

- **Breaking:** A new repository gives its built-in users the capabilities
  `fossil new` gives them. `nobody`, whose capabilities every request
  without credentials gets, may now clone, read and download (`gjorz`); it
  used to be granted `cghijknorswy`, which let anyone push check-ins and
  unversioned files, write wiki pages and tickets, and act with Setup. The
  repository also gets fossil's `anonymous`, `developer` and `reader` users.
  The creating user, who holds Setup, can now push and pull once given a
  password, as Setup includes those rights in fossil. A login is refused
  for any account with no password stored, as fossil refuses it (GHSA-44p2-whvx-9gwq).

  Migration: new repositories no longer let anonymous users push. To allow
  it, grant it explicitly, for example `r.SetCaps("nobody", "gijoryz")`, or
  give clients an account with a password. Existing repositories keep the
  capabilities already stored in them.

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
