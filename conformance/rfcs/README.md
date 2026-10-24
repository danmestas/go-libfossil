# Pinned drafts

The conformance suite reads these documents, never edits them. Each is a
byte-for-byte copy of a file under `docs/rfcs/` in the zit repository, taken
from the check-in below. The requirements tables in `../requirements/` pin
each copy by its SHA-256, so a refreshed copy fails the coverage test until
its table has been re-read against it.

The draft text is the authority. Since zit check-in `62a7d1bb3f`
(2026-10-20) every erratum is applied in its draft and zit keeps no
erratum files: each draft records its corrections in its own Changes
appendix ("Changes in This Revision").

Since zit check-in `68a3f76cb7` (2026-10-20) the artifact-format,
repo-state and sync-protocol drafts name themselves `-00` throughout, in
their headers, their cross-references and their reference entries, and the
copies here carry zit's file names. Against the `62a7d1bb3f` text, which
these copies replace, nothing but those names and the wording of the
Changes appendix's heading and introduction differs: no normative text
changed and no section number moved.

| File | zit check-in | zit artifact | SHA-256 |
|---|---|---|---|
| `draft-fossil-artifact-format-00.txt` | `68a3f76cb7` | `aa13f2e42c` | `0887d83d3c57c8b1…` |
| `draft-fossil-repo-state-00.txt` | `68a3f76cb7` | `140fdf2d3b` | `137f0a573498bf27…` |
| `draft-fossil-sync-protocol-00.txt` | `68a3f76cb7` | `62e9495f35` | `2195d9a18fe6bfaa…` |
| `draft-fossil-delta-format-00.txt` | `62a7d1bb3f` | `d8025d6d75` | `6a28ee7e2e146f80…` |
| `draft-fossil-submanifests-00.txt` | `e03d91943e57afa20c3fdeab159a23a8a5eab66c` (branch `horizon-rfc`) | `b9b19fa418` | `99ea094690fe1386…` |
| `draft-fossil-sparse-cone-00.txt` | `e6ee276c89` (branch `horizon-rfc`) | `c7ef80e7e1` | `2ecfc54f4b830938…` |
| `draft-fossil-horizon-00.txt` | `caeef421ee` (zit 0.7.2) | `ab18fa7f89` | `bbcc73a010a6d7b3…` |

The delta-format and sub-manifest files are byte-identical at `68a3f76cb7`
and `62a7d1bb3f`. The delta-format draft is unchanged since `3f86ff4261`
(2026-10-17).

The sub-manifest draft (`draft-fossil-submanifests-00`) includes the sparse
canonical-rewrite correction merged into `horizon-rfc` at the exact check-in
above (2026-10-20). Sections 6.2 and 6.4 require canonical construction under
the requested k, including MAX-cut shifts, final-small-group merges and
propagation at every height. Opaque reuse requires verified compatible
partition evidence, not the mutable setting; missing necessary nodes must
be fetched or cause explicit refusal without a partial check-in. Appendix
A.6 supplies the required MAX, final-merge and different/unknown-k cases.
The earlier Appendix A byte vectors in `../testdata/submanifest/` remain
unchanged: they are zit's `src/rfc_vectors/` copies from `3f86ff4261`, each
with two leading `#` lines that the suite drops.

The sparse-cone and horizon drafts follow zit's `horizon-rfc` branch, where
the horizon draft lives; that branch merges trunk. The sparse-cone copy at
`e6ee276c89` carries the corrections of zit #257 to #260 and #262 (the
cone-served acknowledgement, the empty-cone refusal, structural retention
on compaction and local filtering, and the pull-inventory clarifications)
and retains the branch's §3.1
members-of-other-kinds and §6.2 outside-delta-base corrections. Its §4.2
now requires the complete canonical structural affected range and verified
partition basis, with fetch or explicit refusal when necessary nodes are
absent. Section 5.1 permits outside payloads to be absent but does not
require discarding already-held or demand-fetched payloads; §5.3 follows
the corrected canonical-writing contract. The horizon copy is pinned at
`caeef421ee`; the `version-0.7.2` tag (`1dcb2d4a29`) carries the same
bytes, and the interop tests are pinned to zit 0.7.2. At that check-in the sparse-cone,
sub-manifest, repo-state and delta-format drafts are byte-identical to the
copies here; the artifact-format and sync-protocol drafts differ and stay
pinned at their own check-ins. Against the `236b7b4285` copy (zit 0.7.1)
it replaces, the horizon draft adds two Appendix C bullets, each a
normative change. The seed's date (§5.2.2, §6.2.3): a seed's D card is the
latest mtime of any tag event in the repository, one that applied,
propagated or cancelled a tag, rounded up as before, no longer the latest
of the events the seed summarizes; dated from the events in force, a seed
moved earlier when the newest of them was cancelled or a closed branch was
reopened, and the clone's D floor refused the re-seed as a replay. The
receive rule (§5.2.3): a store never keeps a seed it did not receive
through a horizon-seed announcement; on every receive path it drops an
artifact that is a seed by its own label or a binding card, judged when it
first becomes storable, a delta completed in a later round included; a
dropped artifact is not counted as received and is not requested again in
the session, and a store should keep a never-synchronized record of the
hashes it dropped; a store that keeps a pushed or received delta whose
base it lacks does not announce or serve it while it cannot be expanded.
The earlier text had a clone keep a seed-shaped artifact outside an
announcement as an ordinary control artifact.

The `236b7b4285` copy was the zit 0.7.1 text; the `version-0.7.1` tag
(`1c4cd7a165`) carries the same bytes. The other six drafts are
byte-identical at `3a2df62515`, `24457c7dd6` and `5d618aaeb1`. Against the
`5d618aaeb1` copy it replaced, the `236b7b4285` horizon draft added three
Appendix C bullets, each a normative change.
Named horizons (§6.2.7, new, with §6.1 and §3.4): a cloning client that
cannot name C by hash sends, in its first request only and in place of
pragma horizon, `pragma horizon-name <name> <rule> <depth>`, where name is
`tip`, a symbolic name or a full hash, and depth plain base-10 digits that
fit 32 bits; the origin looks a full hash up as a hash first, resolves any
other name to the most recent check-in carrying an active `sym-<name>`,
ties going to the lowest full hash, steps back depth primary parents, and
emits `pragma horizon-resolved <hash> <steps>` before any file card and
before horizon-served; a name that resolves to nothing, a malformed depth,
or both pragma forms is one error card, after any login or authorization
error; the client fails the clone, leaving nothing on disk, on a malformed
or late echo, too many steps or a resolved check-in that never arrives, and
against a peer that ignores the name resolves it locally at the content
horizon; the store records the hash, never the name. The seed's label
(§5.2.2, §5.2.3): the U card is the origin's own seed label (zit writes
`zit-seed`, no longer `zit-serve`), which appears on nothing else it writes
for sharing; an origin refuses a pushed control artifact bearing it, judged
on expanded content, a delta whose base arrives later in the request
included, storing nothing and replying with the one error card `seed
refused: a seed is private to the clone that holds it; it is never pushed`;
a seed delta that becomes expandable only later, by a push, pull, clone or
local check-in, is never crosslinked, applied or served, and is recorded
refused in local state that is never synchronized, never in the shun list,
the transfer that filled it being stored normally; §5.7's stale-seed check
is judged on expanded content the same way. The seed's binding (§5.2,
§5.2.3, §5.2.4): every seed carries `T +horizon-seed <C> <rule>`, which the
clone strips before derivation and the origin never counts toward the one
T card nor derives into a seed; an origin refuses any pushed control
artifact carrying that card, whatever its label, and only a foreign
artifact carrying neither its label nor the card is ordinary; a clone holds
a seed only with exactly one binding card naming its own C and rule, and
refuses one dated earlier than the seed it holds, the floor restarting at
a deepen; seed signing is configured apart from check-in signing and, for
SSH, signs under the namespace `fossil-horizon-seed`. Against the
`24457c7dd6` copy that the `5d618aaeb1` copy replaced, the horizon draft
added two Appendix C bullets: §5.2 says that when
no tag is in force at any check-in a seed would cover, the origin mints no
seed and serves the content level, since a control artifact needs a T card
(a normative change); and §3.4 makes the cost report guidance for the tool
that suggests a horizon, not a wire obligation, which an origin may expose
outside the protocol (editorial). Against the `3a2df62515` copy that the
`24457c7dd6` copy replaced, the horizon draft's Appendix C adds one bullet
per change: §3.4 notes that the leaves rule can hold
effectively the whole history when childless check-ins fork near the root,
measured on the Fossil repository (no rule change); a seed's D card is the
latest tag event it summarizes rounded up to a whole second with no
fraction (C's event time when it summarizes none), so two origins mint the
same seed up to the U card, which is the origin's own label for the
minting implementation and not a user's login (§5.2, §5.2.2, §6.2.3); the
requirement that a seed's D be strictly later than the seed it replaces is
dropped, since a clone replaces its seed whole (§5.2.2); a store at the
structural horizon whose pull or sync replies lack horizon-served
structural falls back to the content horizon, records it, discards its
seed, fetches the manifests and nodes below C and reports it, as a clone
does (§6.2.5, §6.3); a client asks the origin for the merge base whenever
either side's ancestry crosses the horizon, not only when it finds none
(§5.5); a deepen moves C to a common ancestor of C and the target, and
when none is known refuses naming the remedy, never moving C to a
non-ancestor (§5.8); the merge-base query takes a primary and one or two
secondaries, the answer repeats them and appends Fossil's pivot over the
set, and in a merge the secondaries are the checkout and its pending plain
merges (§6.2.4); and a boundary hash is a P-card parent, not in K, of any
held check-in manifest, a baseline below C included, labelled in the reply
that introduces the manifest (§5.3, §6.2.2). The `3a2df62515` copy it
replaced made a path cone elide sub-manifest nodes under either horizon,
an origin withholding the rest, its walk bounded and kept, and a cursor
voided when an appended tree makes an older withheld node kept (§3.2,
§4.1, §4.3, §5.1, §6.2, §6.2.6); with the revisions before it, the leaves
rule restated over P cards alone, the seed carrying T cards for every
entry of K, the cursor's announcements made only in reply to a request
that pulls, the cursor itself and the classification-memoization SHOULDs.
The same branch adds the pragma names these two drafts define (`cone`, `cone-not`,
and the `horizon` family) to the sync draft's §9 list of extension pragmas;
the sync draft here follows trunk, so that addition is pending.
