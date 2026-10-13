# Pinned drafts

The conformance suite reads these documents, never edits them. Each is a
byte-for-byte copy of a file under `docs/rfcs/` in the zit repository, taken
from the check-in below. The requirements tables in `../requirements/` pin
each copy by its SHA-256, so a refreshed copy fails the coverage test until
its table has been re-read against it.

Since zit check-in `5a1b741b25d8bb65fe8b2e71a0e099b1ec13b418` (2026-10-13)
the -01 files are gone from zit: each draft is
`docs/rfcs/draft-fossil-X-00.txt`, whose content is the -01 text (the
document name inside still reads `draft-fossil-X-01`) with the -01 errata
applied. The copies here keep the -01 file names; the zit path is the
second column.

| File | zit path | zit check-in | zit artifact | SHA-256 |
|---|---|---|---|---|
| `draft-fossil-artifact-format-01.txt` | `draft-fossil-artifact-format-00.txt` | `5a1b741b25d8` | `dd0e1a9322` | `e91b720ba70a74b5…` |
| `draft-fossil-repo-state-01.txt` | `draft-fossil-repo-state-00.txt` | `5a1b741b25d8` | `e005e776d7` | `d94ba052317b718…` |
| `draft-fossil-sync-protocol-01.txt` | `draft-fossil-sync-protocol-00.txt` | `5a1b741b25d8` | `bacc557b5a` | `383bd08c8923dca…` |
| `draft-fossil-delta-format-00.txt` | `draft-fossil-delta-format-00.txt` | `5a1b741b25d8` | `d8025d6d75` | `6a28ee7e2e146f8…` |
| `erratum/artifact-format-01-s4.6.4-julian-day-grammar.md` | same | `5a1b741b25d8` | `5a1adf7f9c` | `c50e6b5aef35b65…` |
| `erratum/repo-state-01-s4.2-date-tag-event-time.md` | same | `5a1b741b25d8` | `b5e6383412` | `e55108ed39857f4…` |
| `erratum/repo-state-01-s5.3-inherited-row-blocks-direct-tag.md` | same | `5a1b741b25d8` | `1ee5e540ce` | `2bfd0d2e76efe0b…` |
| `erratum/repo-state-01-s6.2-cancel-cards-from-parent.md` | same | `5a1b741b25d8` | `43f021a207` | `3d24910396abea9…` |
| `erratum/repo-state-01-s7.3-leaf-recheck-branch-tag.md` | same | `5a1b741b25d8` | `275509b8fc` | `25a27e819bae83c…` |
| `erratum/repo-state-01-s8.3-ckout-ci-plus.md` | same | `5a1b741b25d8` | `7e8ecce19e` | `a955cf371e420eb…` |
| `erratum/sync-protocol-01-s5.1-truncated-payload-receiver.md` | same | `5a1b741b25d8` | `8157e3faaf` | `0e08208def3084a…` |
| `erratum/sync-protocol-01-s5.3-errors-count.md` | same | `5a1b741b25d8` | `ca1539a1e9` | `752da0b6944f341…` |
| `erratum/sync-protocol-01-s6.6-file-card-after-refused-push.md` | same | `5a1b741b25d8` | `1c50fe8add` | `5857e99624ece02…` |
| `erratum/sync-protocol-01-s9.4-avoid-delta-manifests.md` | same | `00e1711e802e` | `e29beb0afc` | `caad0402360fdcc…` |
| `erratum/sync-protocol-01-s10.1-wrong-hash-file-fails-the-round.md` | same | `5a1b741b25d8` | `e5dd8a4c9b` | `8cb2b8cbbdfe808…` |

The drafts carry every -01 erratum: the six of zit check-in
`100351e26ce269a29e0567dfd81010729be80f3d` (2026-10-12) and the five of
2026-10-13 (repo-state §5.3, §7.3, §8.3; sync-protocol §9.4, §10.1), applied
by check-in `5a1b741b25d8`. The §9.4 erratum was corrected afterwards, at
check-in `00e1711e802e`: it now says the `forbid-delta-manifests` default
is on since Fossil 2.29, while 2.28 defaults it off. The draft text is
unchanged. The errata are pinned for the Fossil 2.28 and
2.29 behavior they record, which the cases use as the reference where the
drafts and fossil(1) disagree. The -00 errata are folded into the -01 drafts
(zit check-in `44685f33b1`) and are no longer pinned. There is no
delta-format -01, so the delta suite stays on -00; its file is unchanged at
`5a1b741b25d8`.
