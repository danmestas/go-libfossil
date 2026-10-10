# Technical erratum — draft-fossil-artifact-format-00 §4.2

| | |
|---|---|
| **Document** | `draft-fossil-artifact-format-00` |
| **Section** | 4.2 — Line and Token Grammar (consequences in 4.7.4 C Card, 4.7.8 J Card, 4.7.16 T Card) |
| **Type** | Technical (the token grammar is stricter than the reference implementation) |
| **Status** | Reported; zit's reader corrected |
| **Reported** | 2026-08-16 |
| **Submitter** | Dan Mestas |
| **Found via** | Indexing `--compatible` fossil→zit imports of sqlite.org/src and core.tcl-lang.org/tcl; every ticket-change artifact carrying an empty field was rejected |

## 1. Summary

§4.2 gives `token = 1*tchar`, making an empty token invalid everywhere.
§4.7.17 then carves out a single exception for the U card, noting that its
grammar "takes precedence over the general requirement that a card argument
be a non-empty token."

Fossil's parser applies no such general requirement. `next_token()` returns a
non-NULL **empty string** for a trailing space on any card, and the C, J and T
cards accept it as a present, empty value. Real repositories rely on this:
Fossil writes an empty value as a bare trailing space. A reader that
implements §4.2 literally rejects those artifacts.

The U card exception is therefore not an exception — it is one instance of a
general rule the document does not state. The rule is POSITIONAL, not
per-card: it is not confined to grammars written `[ SP value ]`. The C card's
value is mandatory, yet `C LF`-with-trailing-space is a present, empty
comment that Fossil accepts, and it is the most common instance in practice.

## 2. The text in error

Page 11 (§4.2):

> ```
> card-args   = token *( SP token )
> token       = 1*tchar
> ```

and, page 22 (§4.7.8) and page 25 (§4.7.16), the `[ SP value ]` production in
`j-card` and `t-card`, which inherits `token`'s non-empty requirement and so
cannot express a present-but-empty value.

and, page 20 (§4.7.4):

> Exactly one required token, escape-decoded; single-occurrence; a C card
> with no token MUST be rejected.

This last conflates two distinct inputs. `C LF` (no space) genuinely has no
token and Fossil rejects it. `C SP LF` has one token, which is empty, and
Fossil accepts it as an empty comment.

## 3. What Fossil implements

`manifest.c`:

```
manifest.c:383-395   next_token(): returns 0 only when already at EOL;
                     otherwise n = strcspn(z, " \n") — which may be 0 —
                     and the token is returned as a NUL-terminated string.
manifest.c:965-966   T card: zValue = next_token(&x,0); if( zValue ) defossilize(zValue);
manifest.c:784-786   J card: zValue = next_token(&x,0); if( zValue==0 ) zValue = "";
manifest.c:629-630   C card: p->zComment = next_token(&x,0);
                             if( p->zComment==0 ) SYNTAX("missing comment text on C-card");
manifest.c:399-412   next_card(): only z[1]==' ' or z[1]=='\n' continues; the
                     '\n' branch sets atEol, which is what makes a bare "C"
                     yield NULL from the following next_token().
```

For the line `T +date <hash> LF`, the third `next_token` call sees `strcspn`
return 0 at the LF, sets `atEol`, and returns a pointer to an empty string —
non-NULL. The T card stores that as the tag's value. Only a card line with no
trailing space at all yields NULL and hence an absent value.

This distinction is exactly the one §4.7.16 already draws ("An absent value
yields null (unlike the J card's empty-string default)"): under Fossil,

* `T +date <hash>` — value absent, null.
* `T +date <hash> ` — value present, empty string.

Both forms occur in the wild. For the J card the two collapse, since an absent
J value already defaults to the empty string.

## 4. Corrected text

In §4.2, after the grammar block, add:

> The final token of a card MAY be empty, on any card letter. When a card
> line's last octet before LF is SP, the final token is PRESENT and its
> decoded form is the empty string — distinct from a card that omits the
> token entirely, which is the argument-less form `card-letter LF`. Whether
> an empty final token is legal for a given card is then decided by that
> card's own arity and validation in Section 4.7, not by this grammar.
> Empty tokens remain invalid in every other position: a card line
> containing two consecutive SP octets is malformed.

and amend the grammar so only the non-final tokens are bound by `token`:

> ```
> card-args   = *( token SP ) value
> token       = 1*tchar
> value       = *tchar
> ```

Note this formulation covers the C card, whose value is mandatory rather than
optional, without a second rule: `C SP LF` yields `card-args` of one empty
`value`, which §4.7.4 then accepts as an empty comment.

In §4.7.4, replace the rejection clause so it distinguishes an absent token
from an empty one:

> Exactly one required token, escape-decoded; single-occurrence. A C card
> with no token — a bare `C` immediately followed by LF — MUST be rejected.
> A C card whose single token is empty (`C`, SP, LF) is a present comment
> whose decoded value is the empty string, and MUST be accepted.

In §4.7.17, the sentence beginning "This card's grammar, with its optional and
possibly-empty login token, governs the U card and takes precedence over the
general requirement…" should be reduced to a cross-reference, since after the
§4.2 correction the U card no longer takes precedence over anything:

> The U card's possibly-empty login token is the general trailing-value rule of
> Section 4.2; the substitution of `anonymous` is what is specific to this card.

## 5. Observable consequence of the uncorrected text

A conforming reader rejects the whole artifact on `J name LF`-with-trailing-space
and on `T +tag <hash> LF`-with-trailing-space. Because these are ticket-change
and tag artifacts, the loss is not partial degradation of one record — the
artifact fails to parse at all and, in any implementation that falls back to
treating unparsable artifacts as opaque content, disappears from derived
state without an error surfacing to the user.

Measured against sqlite.org/src (131,558 artifacts) and core.tcl-lang.org/tcl
(254,469 artifacts), with a reader implementing §4.2 literally:

* sqlite carries 2,100 ticket-change artifacts and **zero** were indexed;
  after the correction, all 794 distinct tickets appear.
* 1,009 artifacts across the two corpora carry an empty C card. These are
  the largest single class, and rejecting them drops whole check-ins — not
  merely their comment text.

The C-card case is the one most likely to be missed by an implementer
reading §4.7.4 alone, because that section's rejection clause reads as
though it already settles the question.

## 6. Scope note

This erratum concerns only the trailing position. §4.2.1 (Exact Arity) is
unaffected and its maximum-token counts continue to apply: a trailing empty
token occupies the card's final optional slot and does not add one. An
interior empty token — two consecutive spaces — remains a rejection, which
matches `next_token`, where an interior `strcspn` of 0 produces an empty
token in a non-final position that each card's own validation then refuses.

Implementations should check whether their tokenizer rejects empty tokens
before the per-card grammar is consulted; that ordering is what turns a
legal artifact into a parse failure.

## 7. Conformance of this implementation

zit was **not** exempt, contrary to the first reading of the report. Its
tokenizer distinguishes absent from empty correctly, but two card parsers then
rejected the empty form before their own card grammar was consulted — the exact
ordering flagged at the end of Section 6:

| card | reference implementation | zit before the fix | after |
|---|---|---|---|
| `T +x <hash>` SP LF | value present, empty | present, empty | unchanged |
| `U` SP LF / bare `U` | `anonymous` | `anonymous` | unchanged |
| `C` SP LF | present, empty comment | **rejected** | present, empty |
| `J field` SP LF | present, empty value | **rejected** | present, empty |

Both rejections were load-bearing in tests that asserted them, so the defect was
pinned rather than latent: `§4.7.4-r1` asserted `C SP LF` was rejected under a
title that said "a C card with no token", conflating the two inputs in exactly
the way Section 2 describes. Those tests now assert the corrected behaviour, and
a third covers the writer: an empty comment survives `create` as `C SP LF`, which
matters because `import-fossil --submanifest` re-mints every manifest it carries.

The consequence for zit would have been the same class of loss measured against
quarry, and for the same reason: a check-in whose manifest fails to parse leaves
derived state without surfacing an error.
