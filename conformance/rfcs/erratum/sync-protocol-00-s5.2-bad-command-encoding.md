# Erratum — draft-fossil-sync-protocol-00 §5.2 / Appendix C

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-00` |
| **Section** | 5.2 — Card Syntax (unknown-operator handling); also §7.x client-sent `error`, §9.3, Appendix C |
| **Type** | Editorial (clarification — the ABNF and §10.1 are correct; the prose spells a TEXT as bare words) |
| **Status** | Proposed — operator decision pending |
| **Reported** | 2026-09-17 |
| **Submitter** | Dan Mestas |
| **Found via** | zit 0.3.18 in production behind a forge: a stock `fossil push` refused by policy reported `Artifacts sent: N`, exit 0, for fifteen hours before anyone noticed; verified against the Fossil reference implementation |

## 1. Summary

The unknown-operator rule is stated six times as "emits error bad command:
LINE", and Appendix C repeats it. Read as a wire line, that is four
space-separated tokens. The card grammar (§14 ABNF) defines the card as
`"error" SP fossilized LF`, and §10.1 says TEXT is fossilized, so the
intended wire form is `error bad\scommand:\sLINE` with LINE's own bytes
escaped. The prose describes the *text* and never says so. An implementer
who takes the prose as the framing emits a card that the reference client
cannot read, and the client's failure mode is silent: it discards the card
and reports success.

The same section already says "emits the handler's fossilized error" for the
`file`/`cfile`/`uvfile` validation path (~line 1015), which is correct and is
what this erratum aligns the unknown-operator sentences with.

## 2. Location

- §5.2, unknown-operator paragraph (~lines 969-970): "server resets partial
  output and emits error bad command: LINE, then continues parsing later
  request cards."
- The same phrase at ~790 (pragma), ~992, ~1422 (client-sent `error`),
  ~1743, ~1812, ~1926, and §10.1 ~2110 ("including error bad command: LINE").
- Appendix C, first paragraph (~2979).

## 3. The reading hazard

Every other TEXT in the document is either given as a metavariable (`error
TEXT`) or explicitly marked fossilized. `bad command: LINE` is the one error
text the draft writes out as words, and because those words contain spaces
it is the one place a reader can mistake the text for the wire framing.
The reference implementation builds this card as

```
error bad\scommand:\s%F(line)
```

that is, the literal text with its spaces escaped and LINE passed through
the fossilize encoder (`xfer.c`, `@ error bad\scommand:\s%F(...)`). The
reference client tokenizes an `error` card on SP and acts on it only when
exactly two tokens are present (§10.2 states this rule for `message`; the
client applies the same test to `error`). Four tokens are `error malformed`
plus two cards it does not know: it logs them as unknown commands, discards
the refusal, and prints its ordinary success summary.

## 4. Impact

Wire-visible. A server following the prose literally emits a refusal the
reference client cannot read; a client that pushes on a timer (autosync, a
forge, CI) treats every refused push as landed and re-sends the same
artifacts each time. zit did exactly this through 0.3.19 for the
`bad command` and `malformed CARD line` cards (zit's own client tolerates
the malformed card, which is why zit-to-zit tests never saw it). Fixed in
zit 0.3.20 by encoding at the single point every error card is assembled.

## 5. Correction (additive clarification)

In §5.2, amend the unknown-operator sentence and add one sentence:

```diff
-   Unknown first token has precedence as unknown operator: server resets
-   partial output and emits error bad command: LINE, then continues
-   parsing later request cards.
+   Unknown first token has precedence as unknown operator: server resets
+   partial output and emits an error card whose TEXT is "bad command: "
+   followed by the offending line, then continues parsing later request
+   cards.  As for every error card (Section 10.1), TEXT is fossilized on
+   the wire: the card is "error bad\scommand:\sLINE" with LINE's own
+   bytes escaped, never four SP-separated tokens.  A receiver that finds
+   more than two tokens on an error card does not recognize it as one.
```

In Appendix C, after the first sentence, add: "The text `bad command:
LINE` is a fossilized TEXT (Section 10.1); it is rendered here as words
for readability."

The remaining occurrences of "emits error bad command: LINE" need no change
once §5.2 defines the phrase; a cross-reference "(Section 5.2)" at each is
optional.

## 6. Rationale

- **Aligns prose with the grammar it already has.** The ABNF and §10.1 are
  correct; the fix names the encoding where the text is first spelled out.
- **Names the failure mode.** "A receiver that finds more than two tokens
  does not recognize it as one" tells an implementer what a wrong framing
  costs, which is the sentence that would have caught this in review.
- **Matches the file-handler sentence** in the same section, which already
  says "fossilized error".

## 7. Application and verification

- zit 0.3.20 assembles every server error card at one point with TEXT
  encoded; the texts §5.2 and Appendix C pin are unchanged.
- A unit test drives every server fault variant through the emitter and
  fails on any raw SP after the operator. The §5.2 and §7.1 tests assert
  the encoded wire bytes `error bad\scommand:\sbogus` and
  `error malformed\sfile\sline`.
- Verified against the reference implementation's emitter (`xfer.c`, the
  `bad\scommand:\s%F(...)` line) and its two-token client rule.

---

*Prepared against `draft-fossil-sync-protocol-00`. The section is
substantively faithful; the correction is editorial. Surfaced when a stock
fossil client silently dropped a zit server's refusal. The RFC remains the
oracle; the reference implementation is used only to confirm the observable
behavior, per the interop-verification policy.*
