# Erratum — draft-fossil-artifact-format-00 §4.6.4

| | |
|---|---|
| **Document** | `draft-fossil-artifact-format-00` |
| **Section** | 4.6.4 — Timestamps (accepted parser grammar) |
| **Type** | Editorial (clarification — the field constraints are correct; their placement scopes them to the wrong form) |
| **Status** | Proposed — operator decision pending |
| **Reported** | 2026-10-08 |
| **Submitter** | Dan Mestas |
| **Found via** | libfossil conformance triage (`artifact.4.6.4.invalid-month`) + independent verification against the Fossil reference implementation |

## 1. Summary

§4.6.4 lists five token forms a parser accepts. The paragraph giving the
field constraints (digit counts and ranges for year, month, day, hour,
minute, second and fraction) is printed inside item 5, the bare-decimal
Julian day number. Item 5 has none of those fields. Read literally, the
constraints apply to nothing, and items 1 to 3 have no range checks at all.
The constraints belong to the forms that carry those fields. This erratum
moves the paragraph out of the list.

## 2. Location

§4.6.4 (Timestamps), list item 5 (~lines 1013-1019):

```
   5.  A bare decimal number, treated as a Julian day number when its
       value r satisfies 0 < r < 5373484.5.  Field constraints, with
       exact digit counts: the year is exactly 4 digits, the month
       exactly 2 digits in 01-12, the day exactly 2 digits in 01-31 with
       no calendar-aware check (2024-02-31 is accepted), the hour 2
       digits in 00-24, the minute and second 2 digits each in 00-59,
       and the fraction one or more digits.
```

## 3. The reading hazard

The fields the constraints name exist only in items 1 to 3: year, month and
day in the date-time and date-only forms, hour, minute, second and fraction
in the date-time and time-only forms. Item 4 is a literal and item 5 is a
single number. Nesting the paragraph under item 5 gives a reader two wrong
choices: apply the ranges to the Julian-day form, where they cannot apply,
or treat items 1 to 3 as unconstrained and accept `2026-13-02T03:04:05`.

The reference implementation applies the ranges to the date and time
fields. Verified with `fossil test-parse-manifest` (fossil 2.28):

- `D 2026-13-02T03:04:05` is rejected ("cannot parse date on D-card").
- `D 2026-01-02` (date only) is accepted.

## 4. Impact

No wire-byte change and no change to which tokens are valid. The hazard is
implementer-facing: an implementation that follows the list structure can
skip the month and day range checks. libfossil's parser does, and it accepts
the month-13 token above, which the reference implementation rejects.

## 5. Correction (editorial move)

End item 5 after its Julian-day sentence, and move the field constraints
into their own paragraph after the list, naming the forms they govern:

```diff
    5.  A bare decimal number, treated as a Julian day number when its
-       value r satisfies 0 < r < 5373484.5.  Field constraints, with
-       exact digit counts: the year is exactly 4 digits, the month
-       exactly 2 digits in 01-12, the day exactly 2 digits in 01-31 with
-       no calendar-aware check (2024-02-31 is accepted), the hour 2
-       digits in 00-24, the minute and second 2 digits each in 00-59,
-       and the fraction one or more digits.
+       value r satisfies 0 < r < 5373484.5.
+
+   In forms 1 through 3, each field present has exact digit counts and
+   ranges: the year is exactly 4 digits, the month exactly 2 digits in
+   01-12, the day exactly 2 digits in 01-31 with no calendar-aware
+   check (2024-02-31 is accepted), the hour 2 digits in 00-24, the
+   minute and second 2 digits each in 00-59, and the fraction one or
+   more digits.
```

## 6. Rationale

- **Scopes the constraints to the forms that have the fields.** "In forms 1
  through 3, each field present" says where they apply, so the Julian-day
  form keeps only its own `0 < r < 5373484.5` bound.
- **Leaves the constraint text unchanged.** Only the placement and the
  lead-in change; every digit count and range is the same.
- **Matches the reference implementation**, which rejects an out-of-range
  month in a date-time token and accepts the date-only form.

## 7. Application and verification

- The libfossil conformance case `artifact.4.6.4.invalid-month` already
  assumes the corrected reading: it expects `D 2026-13-02T03:04:05` to be
  rejected.
- Verified against the reference implementation (fossil 2.28,
  `test-parse-manifest`): `2026-13-02`, `2026-01-32`,
  `2026-01-32T03:04:05`, `2026-01-02T25:04:05` and `2026-01-02T03:60:05`
  are rejected ("cannot parse date on D-card"); `2026-01-02`,
  `2026-01-02T03:04:05` and the Julian-day number `2461000.5` are
  accepted.

---

*Prepared against `draft-fossil-artifact-format-00`. The section is
substantively faithful; the correction is editorial. Surfaced when libfossil
conformance triage found its parser following the list structure literally.
The RFC remains the oracle; the reference implementation is used only to
confirm the correct observable behavior, per the interop-verification
policy.*
