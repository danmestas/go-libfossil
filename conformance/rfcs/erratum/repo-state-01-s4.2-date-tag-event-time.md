# Erratum — draft-fossil-repo-state-01 §4.2 / §1

| | |
|---|---|
| **Document** | `draft-fossil-repo-state-01` |
| **Section** | 4.2 — Crosslink procedure (the check-in's stored event time); 1 — Introduction |
| **Type** | Technical (a `date` tag overrides the D card's time, and the draft does not say so) |
| **Status** | Applied in `draft-fossil-repo-state-01` (2026-10-12) |
| **Reported** | 2026-10-12 |
| **Submitter** | Dan Mestas |
| **Found via** | go-libfossil's conformance follow-ups against Fossil 2.28 on a copy of `libfossil.fossil`; confirmed by reading Fossil's source |

## 1. Summary

§1 says "the check-in modification time (mtime) that orders check-ins is
carried by the manifest date", and every ordering rule in the draft reads
that stored time. Fossil lets a singleton tag named `date`, written by
`fossil amend --date`, replace it: the stored event time is the newest
active `date` tag's value when one exists, and the D card's time otherwise.
The draft never mentions the tag, so a reader orders amended check-ins by
their original D cards and disagrees with Fossil on leaves, tips and
symbolic-name resolution for them.

## 2. Location

§1, the sentence quoted above; §4.2, which has no step for the event time.

## 3. The reference behavior

Verified against Fossil 2.28.

- `manifest_crosslink()` (`src/manifest.c:2394-2409`) writes the check-in's
  event row with
  `mtime = coalesce((SELECT julianday(value) FROM tagxref WHERE tagid=TAG_DATE AND rid=...), D)`
  and keeps the D card's time as `omtime`.
- `tag_insert()` (`src/tag.c:229-235`) runs, for a `date` tag applied to an
  already crosslinked check-in,
  `UPDATE event SET mtime=julianday(value), omtime=coalesce(omtime,mtime)`.
  A `date` tag is applied under the ordinary tag_insert rule, so a newer
  tag replaces an older one and the newest value wins.
- Neither site filters on tag type: a propagating `date` tag sets its
  target's time (descendants are not retimed, since the update names one
  rid), and a cancel (`tagtype` 0) reaches the update with a NULL value
  (`src/tag.c:197-199`), so a cancel or an unparsable value applied after
  crosslink stores no time at all, while the same tag applied before
  crosslink falls back to the D card. A value of `0` stores a time of 0,
  since this path has no positivity check.

On a copy of `libfossil.fossil`, four check-ins carry `date` tags; an
implementation that read only the D card computed event times different
from Fossil's for all four.

## 4. Impact

Observable wherever an amended date exists: branch tips, leaves, `tip`,
and plain-name resolution all order on the stored event time.

## 5. Correction

§1, the sentence on the check-in modification time:

```diff
-   names; the check-in modification time (mtime) that orders check-ins
-   is carried by the manifest date defined there.  It relies on
+   names; the check-in modification time (mtime) that orders check-ins
+   is carried by the manifest date defined there, unless a date tag
+   overrides it (Section 4.2).  It relies on
```

§4.2, a new step after step 5:

```
   6.  *Event time.*  The check-in's stored event time is its D-card
       timestamp, unless a tag named "date" is applied to the
       check-in, in which case it is that tag's value parsed under the
       timestamp grammar of [FossilArtifact] Section 4.6.4.  A date
       tag of any type sets the time of its target only; a
       propagating date tag does not retime descendants.  When a date
       tag is applied to a check-in that has already been crosslinked,
       the stored event time MUST be updated to the tag's value; when
       the tag arrives first, the time MUST be taken from it when the
       check-in is crosslinked.  A newer date tag replaces an older
       one under the rule of Section 5.3, so the newest date tag's
       value is the one that orders the check-in.  Cancelling the date
       tag MUST restore the D-card time, and a date tag whose value
       does not parse, or parses to a time that is not positive, MUST
       be ignored for this purpose, so that the time is the D-card
       time.  The D-card timestamp is retained
       separately as the original time.  (Fossil instead stores no
       time at all when a date tag is cancelled or unparsable after
       the check-in was crosslinked, and stores a time of zero for the
       value 0; its result then depends on arrival order.)
```

## 6. Rationale

States the rule the ordering sections silently depend on, in both orders
of arrival, and ties the replacement rule to §5.3 rather than restating it.
The cancel and unparsable cases are defined to keep a time on every
check-in, because a check-in with no time cannot be ordered, and to give
the same answer whether the tags arrive before or after the check-in or
the repository is rebuilt; Fossil's order-dependent result for them is
recorded as a divergence.

## 7. Verification

- Source: Fossil 2.28 `src/manifest.c:2394-2409`, `src/tag.c:171-184`,
  `:197-199`, `:229-235`.
- Run: go-libfossil's conformance tests for a date tag arriving after the
  check-in, before it while the check-in is a phantom, and newest-wins,
  all agreeing with Fossil 2.28.
