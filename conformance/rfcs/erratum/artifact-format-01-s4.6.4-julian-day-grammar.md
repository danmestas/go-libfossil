# Erratum — draft-fossil-artifact-format-01 §4.6.4

| | |
|---|---|
| **Document** | `draft-fossil-artifact-format-01` |
| **Section** | 4.6.4 — Timestamps (form 5, the bare Julian day number) |
| **Type** | Technical (the lexical form and the out-of-range outcome are unstated) |
| **Status** | Applied in `draft-fossil-artifact-format-01` (2026-10-12) |
| **Reported** | 2026-10-12 |
| **Submitter** | Dan Mestas |
| **Found via** | A reader implementing §4.6.4 from the text alone |

## 1. Summary

Form 5 says a timestamp token may be "a bare decimal number, treated as a
Julian day number when its value r satisfies 0 < r < 5373484.5". It does not
say what spellings count as a bare decimal number, and it gives no outcome for
a number outside that range: "treated as ... when" states a condition without
saying what happens when the condition fails.

## 2. Location

§4.6.4, item 5, and the "Finally:" paragraph after the field-constraint
paragraph.

## 3. The reference behavior

Verified against Fossil 2.28 by reading `src/manifest.c` and the bundled
SQLite.

The D and E card tokens are handed unchanged to the storage engine's
`julianday()` function (`manifest.c:644`, `:660`), and the card is rejected
when the result is not positive. That function recognizes a numeric token by
its text-to-float conversion (`sqlite3AtoF`, `parseDateOrTime`): an optional
sign, digits with an optional fraction, and an optional decimal exponent, in
the forms `[+-]digits[E[+-]digits]`, `[+-]digits.[digits][E[+-]digits]` and
`[+-].digits[E[+-]digits]`. Hexadecimal, infinity and NaN spellings are not
numbers. The value is converted to whole milliseconds, `(i64)(r*86400000 + 0.5)`
(`setRawDateNumber`), and kept only when that lies in
`0 .. 464269060799999` (`validJulianDay`); otherwise the function yields no
value and Fossil rejects the card. A value that rounds to 0 milliseconds
passes the engine's check but fails Fossil's positivity test. The accepted
range is therefore `0 < r < 5373484.5` measured after rounding to the
millisecond: a positive value below about 5.8e-9 is rejected, and so is a
value within about 6e-9 below 5373484.5.

## 4. Impact

Observable. Two readers can disagree on whether `2.45e6`, `+2451545.5` or
`.5` is a valid D card, and on whether `5373484.5` is rejected or read some
other way. Fossil accepts the first three and rejects the fourth.

## 5. Correction

```diff
 5.  A bare decimal number, treated as a Julian day number when its
-    value r satisfies 0 < r < 5373484.5.
+    value r satisfies 0 < r < 5373484.5.  The number is an optional
+    sign, then digits with an optional fraction (at least one digit
+    in all), then an optional decimal exponent introduced by E or e
+    with an optional sign, such as 2451545.5, +2451545, .5 or 2.45e6.
+    No other numeric spelling is recognized.  A number whose value
+    is zero, negative, or 5373484.5 or more MUST be rejected.  The
+    check is made on the value rounded to the nearest millisecond,
+    so a positive value that rounds to zero is rejected, as is a
+    value just below 5373484.5 that rounds up to it.
```

## 6. Rationale

The grammar is the engine's own, stated so a reader need not consult it. The
rejection outcome is what Fossil does; without it the text leaves the
over-limit case open.

## 7. Verification

- Source: Fossil 2.28 `src/manifest.c:642-646` and `:656-662`; bundled SQLite
  `parseDateOrTime`, `setRawDateNumber`, `validJulianDay`, and the
  `sqlite3AtoF` contract.
