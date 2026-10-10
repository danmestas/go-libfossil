# Technical erratum — draft-fossil-repo-state-00 §5.4 step 3

| | |
|---|---|
| **Document** | `draft-fossil-repo-state-00` |
| **Section** | 5.4 — Tag propagation, step 3 (qualification) |
| **Type** | Technical (the text diverges from Fossil's implemented behavior) |
| **Status** | Reported |
| **Reported** | 2026-08-13 |
| **Submitter** | Dan Mestas |
| **Found via** | Editorial cross-check of two divergent draft copies against Fossil C (`tag.c`) |

## 1. Summary

§5.4 step 3 says a child with no same-tag entry "always qualifies" for
propagation. That is true only for PROPAGATING tags. For a cancel-type
propagation, Fossil's walk halts at a child with no same-tag row: there is
nothing to cancel there, and the walk does not continue past it.

## 2. The text in error

Page 15 (§5.4 step 3):

> A child that currently has no same-tag entry always qualifies.

## 3. What Fossil implements

`tag.c` (the propagation query and its queue gate):

```
tag.c:55-61   doit = coalesce(srcid=0 AND tagxref.mtime<:mtime, tagType==2)
tag.c:88-93   pqueuex_insert(...) runs only if( doit )
```

The qualification is computed over a LEFT JOIN. When the child has no
`tagxref` row, the first `coalesce` argument is NULL and `doit` falls back
to `tagType==2` — true for a propagating tag (tagtype 2), FALSE for a
cancel (tagtype 0). Because `pqueuex_insert` is gated on `doit`, a
cancel-type walk genuinely stops at a childless-of-that-tag node.

## 4. Corrected text

> A child that currently has no same-tag entry qualifies only when the
> propagating tag is of propagate type; a cancel-type propagation does not
> qualify at such a child — it has no entry to cancel — and the walk does
> not continue past it.

## 5. Observable consequence of the uncorrected text

On a chain carrying a gap — a descendant that never had the tag — an
implementation of the literal text walks a cancel through the gap and can
cancel same-tag entries further down than Fossil would, so the derived
tagxref-equivalent state diverges from what Fossil computes for the same
artifact sequence.

## 6. Provenance note

The wording traces to one of two divergent working copies of the draft;
the other copy did not generalize "always qualifies" beyond propagating
tags. The zit reference implementation had followed the wrong copy
(fixed, with the halt pinned by a step-count test).
