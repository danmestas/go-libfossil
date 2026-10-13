# Pinned drafts

The conformance suite reads these documents, never edits them. Each is a
byte-for-byte copy of the file of the same name under `docs/rfcs/` in the zit
repository, taken from the check-in below. The requirements tables in
`../requirements/` pin each copy by its SHA-256, so a refreshed copy fails the
coverage test until its table has been re-read against it.

| File | zit check-in | zit artifact | SHA-256 |
|---|---|---|---|
| `draft-fossil-artifact-format-01.txt` | `100351e26ce269a2` | `dd0e1a9322` | `e91b720ba70a74b5…` |
| `draft-fossil-repo-state-01.txt` | `100351e26ce269a2` | `03b0c3049d` | `028d6bd0c5a9842…` |
| `draft-fossil-sync-protocol-01.txt` | `100351e26ce269a2` | `4d873f280c` | `b19c7cfc8f69bb4…` |
| `draft-fossil-delta-format-00.txt` | `0ae7fb2622` | `d8025d6d75` | `6a28ee7e2e146f8…` |
| `erratum/artifact-format-01-s4.6.4-julian-day-grammar.md` | `100351e26ce269a2` | `5a1adf7f9c` | `c50e6b5aef35b65…` |
| `erratum/repo-state-01-s4.2-date-tag-event-time.md` | `100351e26ce269a2` | `b5e6383412` | `e55108ed39857f4…` |
| `erratum/repo-state-01-s6.2-cancel-cards-from-parent.md` | `100351e26ce269a2` | `43f021a207` | `3d24910396abea9…` |
| `erratum/sync-protocol-01-s5.1-truncated-payload-receiver.md` | `100351e26ce269a2` | `8157e3faaf` | `0e08208def3084a…` |
| `erratum/sync-protocol-01-s5.3-errors-count.md` | `100351e26ce269a2` | `ca1539a1e9` | `752da0b6944f341…` |
| `erratum/sync-protocol-01-s6.6-file-card-after-refused-push.md` | `100351e26ce269a2` | `1c50fe8add` | `5857e99624ece02…` |

zit check-in `100351e26ce269a29e0567dfd81010729be80f3d` (2026-10-12) applies
the six -01 errata to the -01 drafts, so the drafts already carry their text;
the errata are kept for the Fossil 2.28 behavior they record, which the cases
use as the reference where the drafts and fossil(1) disagree. The -00 errata
are folded into the -01 drafts (zit check-in `44685f33b1`) and are no longer
pinned. There is no delta-format -01, so the delta suite stays on -00.
