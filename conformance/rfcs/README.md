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
| `draft-fossil-artifact-format-01.txt` | `draft-fossil-artifact-format-00.txt` | `3f86ff4261` | `b92b19b71f` | `3217c01c2d5a01a4…` |
| `draft-fossil-repo-state-01.txt` | `draft-fossil-repo-state-00.txt` | `3f86ff4261` | `9980e22265` | `59dd17b9588497c6…` |
| `draft-fossil-sync-protocol-01.txt` | `draft-fossil-sync-protocol-00.txt` | `3f86ff4261` | `bafc21adc5` | `c738d32f854495e9…` |
| `draft-fossil-delta-format-00.txt` | `draft-fossil-delta-format-00.txt` | `3f86ff4261` | `d8025d6d75` | `6a28ee7e2e146f80…` |
| `draft-fossil-submanifests-00.txt` | same | `bbc6794bf0` | `22c3592cdd` | `a7e51f7bb964ebc7…` |
| `draft-fossil-sparse-cone-00.txt` | same | `86d4b67372` (branch `horizon-rfc`) | `cc2c259c19` | `22f7c2d847a31bc8…` |
| `draft-fossil-horizon-00.txt` | same | `86d4b67372` (branch `horizon-rfc`) | `56bf5c0553` | `e1e92af6fa1c2acf…` |
| `erratum/artifact-format-01-s4.4-z-card-last-by-ordering.md` | same | `3f86ff4261` | `dc53e482e1` | `3049c7ebc2aadea1…` |
| `erratum/artifact-format-01-s4.6.4-julian-day-grammar.md` | same | `3f86ff4261` | `5a1adf7f9c` | `c50e6b5aef35b654…` |
| `erratum/artifact-format-01-s4.7.3-delta-of-delta-delete-markers.md` | same | `3f86ff4261` | `fbc5c735d8` | `55195d37f9e74737…` |
| `erratum/artifact-format-01-s4.7.8-j-card-empty-value.md` | same | `3f86ff4261` | `befe7488ef` | `d0880d6735b7890d…` |
| `erratum/repo-state-01-s4.2-date-tag-event-time.md` | same | `3f86ff4261` | `b5e6383412` | `e55108ed39857f42…` |
| `erratum/repo-state-01-s5.3-inherited-row-blocks-direct-tag.md` | same | `3f86ff4261` | `1ee5e540ce` | `2bfd0d2e76efe0bf…` |
| `erratum/repo-state-01-s6.2-cancel-cards-from-parent.md` | same | `3f86ff4261` | `43f021a207` | `3d24910396abea9e…` |
| `erratum/repo-state-01-s6.2-self-cancelling-sym-tag.md` | same | `3f86ff4261` | `821dd715d9` | `85d4f040bbede7ee…` |
| `erratum/repo-state-01-s7.3-leaf-recheck-branch-tag.md` | same | `3f86ff4261` | `275509b8fc` | `25a27e819bae83cf…` |
| `erratum/repo-state-01-s8.3-ckout-ci-plus.md` | same | `3f86ff4261` | `7e8ecce19e` | `a955cf371e420ebd…` |
| `erratum/sync-protocol-01-s5.1-truncated-payload-receiver.md` | same | `3f86ff4261` | `8157e3faaf` | `0e08208def3084ac…` |
| `erratum/sync-protocol-01-s5.3-errors-count.md` | same | `3f86ff4261` | `ca1539a1e9` | `752da0b6944f341a…` |
| `erratum/sync-protocol-01-s6.6-file-card-after-refused-push.md` | same | `3f86ff4261` | `1c50fe8add` | `5857e99624ece02c…` |
| `erratum/sync-protocol-01-s6.6-refusal-text-suffix.md` | same | `3f86ff4261` | `80923cf46f` | `19ff7626abc62a50…` |
| `erratum/sync-protocol-01-s9.4-avoid-delta-manifests.md` | same | `3f86ff4261` | `e29beb0afc` | `caad0402360fdcca…` |
| `erratum/sync-protocol-01-s10.1-wrong-hash-file-fails-the-round.md` | same | `3f86ff4261` | `e5dd8a4c9b` | `8cb2b8cbbdfe8089…` |

The drafts carry every -01 erratum, taken from zit check-in
`3f86ff4261484cc3b57aa6bbeb1cede7515d76ad` (2026-10-17): the six of check-in
`100351e26ce269a29e0567dfd81010729be80f3d` (2026-10-12); the five of
2026-10-13 (repo-state §5.3, §7.3, §8.3; sync-protocol §9.4, §10.1), applied
by check-in `5a1b741b25d8`, with the §9.4 erratum corrected at `00e1711e802e`
to say the `forbid-delta-manifests` default is on since Fossil 2.29 and off
in 2.28; and six more of 2026-10-14 and 2026-10-16: artifact-format §4.4/§6.2
(the Z card is last by ordering and the required-card rule; verification
reads bytes by position), §4.7.3 (a named delta's delete markers are
filtered), §4.7.8 (an absent and an empty `J` value decode alike),
repo-state §5.3/§6.2 (an artifact's tag cards apply in card order, so a
self-cancelling sym tag stays), and sync-protocol §6.6 (a client matches a
refusal by its leading text). The sixth, repo-state §5.3 step 3 (a cancel's
value stays on the stored row), is filed in zit as
`erratum/repo-state-00-s5.3-cancel-value.md`, an erratum to -00 applied in the
-01 text; like the other -00 errata it is not pinned here, and the -01 draft
carries its text. The horizon draft merged into zit trunk on 2026-10-17 was
backed out at `3f86ff4261`, so nothing of it is pinned.

The errata are pinned for the Fossil 2.28 and 2.29 behavior they record,
which the cases use as the reference where the drafts and fossil(1)
disagree. The -00 errata are folded into the -01 drafts (zit check-in
`44685f33b1`) and are no longer pinned. There is no delta-format -01, so the
delta suite stays on -00; its file is unchanged at `3f86ff4261`.

The sub-manifest draft (`draft-fossil-submanifests-00`) has no -01. It is
taken from zit check-in `bbc6794bf0f75e1cb6bb70f0e40444bed68678ca`
(2026-10-19), the revision dated 19 October 2026, whose Appendix C lists
the nine errata of 2026-10-19 it applies (filed in zit as
`erratum/submanifests-00-*.md`): §7.2 gates on the `send-submanifests`
pragma, not the announced version (erratum check-in `6588d1bb3b`); §7.2
places `avoid-submanifests` in every reply's leading pragma run, and defines
the sync session it binds (§7.2, §8.1); §6.3 adopts the settings once, at
clone, through `reqconfig /config`, specifies the history replay, and names
the newest main-branch leaf as the current check-in without a checkout;
§8.1 refuses an enable over an out-of-range stored k; §1.3 records that
zit and go-libfossil implement it; and the provisional phase retires, §8.2
is removed and its flag-day rule moves to §8.1. Like the other -00 errata they are folded
into the draft and not pinned on their own. Its Appendix A vectors are byte
vectors: `../testdata/submanifest/` holds zit's copies of them, as authored
for zit, taken at check-in `3f86ff4261` and unchanged at `bbc6794bf0`
(`src/rfc_vectors/`), each with two leading `#` lines that keep it from
reading as a live artifact on disk; the suite drops those lines.

The sparse-cone and horizon drafts are not on zit's trunk yet. They are
taken from check-in `86d4b6737219c3d403868015d6d396367e89c11c`
(2026-10-18) on zit's `horizon-rfc` branch, and are re-pinned when zit
lands them on trunk. The sparse-cone draft's §3.6 still says exclusion
members are out of scope; that paragraph is stale (tracked in
danmestas/zit#242), and the suite follows §3.3 and zit, which implement
them. The same check-in adds the pragma names these two drafts define
(`cone`, `cone-not`, and the `horizon` family) to the sync draft's §9
list of extension pragmas; the sync draft here is not re-pinned to it, so
that addition is pending.
