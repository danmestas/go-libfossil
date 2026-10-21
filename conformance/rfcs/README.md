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
| `draft-fossil-horizon-00.txt` | `e6ee276c89` (branch `horizon-rfc`) | `de33f5b033` | `f808dbd0db372f37…` |

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
the corrected canonical-writing contract. The horizon copy moves from
`86d4b67372` (2026-10-18) to `e6ee276c89` with it; the normative changes
between them are all in §6: §6.1 lets any request carry the cursor, §6.2
adds the classification-memoization SHOULDs, and §6.2.6 (the horizon
cursor) is new. Every other section is word-for-word unchanged. The same
branch adds the pragma names these two drafts define (`cone`, `cone-not`,
and the `horizon` family) to the sync draft's §9 list of extension pragmas;
the sync draft here follows trunk, so that addition is pending.
