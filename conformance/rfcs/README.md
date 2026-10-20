# Pinned drafts

The conformance suite reads these documents, never edits them. Each is a
byte-for-byte copy of a file under `docs/rfcs/` in the zit repository, taken
from the check-in below. The requirements tables in `../requirements/` pin
each copy by its SHA-256, so a refreshed copy fails the coverage test until
its table has been re-read against it.

The draft text is the authority. Since zit check-in `62a7d1bb3f`
(2026-10-20) every erratum is applied in its draft and zit keeps no
erratum files: each draft records its corrections in its own Changes
appendix ("Changes from draft-fossil-X-00" for artifact-format, repo-state
and sync-protocol; "Changes in This Revision" for submanifests and
sparse-cone), and notes beside the amended text record where Fossil
differs.

Since zit check-in `5a1b741b25d8bb65fe8b2e71a0e099b1ec13b418` (2026-10-13)
the -01 files are gone from zit: each draft is
`docs/rfcs/draft-fossil-X-00.txt`, whose content is the -01 text (the
document name inside still reads `draft-fossil-X-01`). The copies here keep
the -01 file names; the zit path is the second column.

| File | zit path | zit check-in | zit artifact | SHA-256 |
|---|---|---|---|---|
| `draft-fossil-artifact-format-01.txt` | `draft-fossil-artifact-format-00.txt` | `62a7d1bb3f` | `b92b19b71f` | `3217c01c2d5a01a4…` |
| `draft-fossil-repo-state-01.txt` | `draft-fossil-repo-state-00.txt` | `62a7d1bb3f` | `9980e22265` | `59dd17b9588497c6…` |
| `draft-fossil-sync-protocol-01.txt` | `draft-fossil-sync-protocol-00.txt` | `62a7d1bb3f` | `bafc21adc5` | `c738d32f854495e9…` |
| `draft-fossil-delta-format-00.txt` | same | `62a7d1bb3f` | `d8025d6d75` | `6a28ee7e2e146f80…` |
| `draft-fossil-submanifests-00.txt` | same | `62a7d1bb3f` | `5379bc614e` | `9326a7f95bc33875…` |
| `draft-fossil-sparse-cone-00.txt` | same | `0acb5e185c` (branch `horizon-rfc`) | `05f8b59810` | `2494753225ec3b88…` |
| `draft-fossil-horizon-00.txt` | same | `86d4b67372` (branch `horizon-rfc`) | `56bf5c0553` | `e1e92af6fa1c2acf…` |

The artifact-format, repo-state, sync-protocol and delta-format files are
unchanged since `3f86ff4261` (2026-10-17). There is no delta-format -01, so
the delta suite stays on -00.

The sub-manifest draft (`draft-fossil-submanifests-00`) has no -01. Its
copy is the revision dated 19 October 2026, whose Appendix C lists the
corrections it applies to the 27 July 2026 text. Its Appendix A vectors are
byte vectors: `../testdata/submanifest/` holds zit's copies of them, as
authored for zit, taken at check-in `3f86ff4261` and unchanged at
`62a7d1bb3f` (`src/rfc_vectors/`), each with two leading `#` lines that
keep it from reading as a live artifact on disk; the suite drops those
lines.

The sparse-cone and horizon drafts follow zit's `horizon-rfc` branch, where
the horizon draft lives; that branch merges trunk. The sparse-cone copy is
the 19 October 2026 revision at `0acb5e185c`, the branch's merge of
`62a7d1bb3f`. Trunk's copy is the same text less the two corrections the
horizon work added (§3.1 members of other kinds, §6.2 delta bases outside
the keep set), which the branch's Appendix B lists; the copy here
keeps them because the cone serialization and the horizon table depend on
them. The horizon copy stays at `86d4b67372` (2026-10-18); the branch has
since added §6.2.6 (the horizon cursor) and the classification-memoization
SHOULDs of §6.2, which are not yet read into the horizon table. The same
branch adds the pragma names these two drafts define (`cone`, `cone-not`,
and the `horizon` family) to the sync draft's §9 list of extension pragmas;
the sync draft here follows trunk, so that addition is pending.
