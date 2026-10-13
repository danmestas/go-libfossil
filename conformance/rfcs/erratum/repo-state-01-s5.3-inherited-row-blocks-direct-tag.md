# Erratum — draft-fossil-repo-state-01 §5.3

| | |
|---|---|
| **Document** | `draft-fossil-repo-state-01` |
| **Section** | 5.3 — Direct Application Procedure, step 1 |
| **Type** | Technical (the skip test also reads inherited rows, which makes the result depend on arrival order; the draft does not say so) |
| **Status** | Applied in `draft-fossil-repo-state-01` (2026-10-13) |
| **Reported** | 2026-10-13 |
| **Submitter** | Dan Mestas |
| **Found via** | Reading Fossil 2.29's `tag_insert()` against §5.3 and §5.5; confirmed with Fossil 2.28 |

## 1. Summary

Step 1 skips a direct application when "an existing tagxref row for that
(tag, artifact) pair already carries an mtime greater than or equal to the
incoming tag's mtime". In Fossil that row can be an inherited one (srcid
0) written by propagation from an ancestor. §5.5 says the reverse
comparison never happens: a direct row blocks propagation "regardless of
that entry's mtime". The two rules together make the outcome depend on
which tag arrives first, and the draft states order dependence only for
the equal-mtime case. For a `date` tag the stored event time of the
check-in then depends on arrival order, which §4.2 step 6 does not
anticipate.

## 2. Location

§5.3, step 1.

## 3. The reference behavior

Verified against Fossil 2.29 source; behavior run on Fossil 2.28.

- `tag_insert()` (`src/tag.c:173-186`) returns without writing when
  `SELECT 1 FROM tagxref WHERE tagid=? AND rid=? AND mtime>=:mtime`
  finds a row. The query has no `srcid` term, so a propagated row counts.
- `tag_propagate()` (`src/tag.c:54-59`) updates a child only when
  `srcid=0 AND tagxref.mtime<:mtime`, so a direct row always blocks it.
- A propagated row carries the origin tag's mtime (`src/tag.c:66-69`),
  and propagation updates no event column except `bgcolor`
  (`src/tag.c:80-82`, `:95-98`). The `date` event update happens only in
  `tag_insert()` (`src/tag.c:229-235`).

Take an ancestor A, its primary descendant C, a propagating tag `*T` on A
with tag time m1, and a direct tag `+T` (or `*T`) on C with tag time
m2 <= m1, both crosslinked after C:

- `+T` on C first: C gets a direct row. `*T` then reaches C, finds a
  direct row, and stops there. C holds the direct value.
- `*T` on A first: C gets an inherited row with mtime m1. `+T` then finds
  `mtime >= m2` and is skipped. C holds A's value, and C's descendants
  keep it too.

The two orders leave different `tagxref` rows on the same set of
artifacts. If C itself arrives after both tags, the direct tag is stored
on C's phantom, and the propagation from A, which runs when C is
crosslinked, stops at C, so C holds the direct value.

For `date` the difference reaches the event table. Fossil 2.28, two
repositories with the same check-ins c1 and c2 (c2 a child of c1), with
`*date` on c1 (value 2001-01-01, tag time 2026-02-01) and `+date` on c2
(value 2002-02-02, tag time 2026-01-01):

| Order of arrival | c2's `date` row | c2's event time |
|---|---|---|
| `+date` on c2, then `*date` on c1 | direct, 2002-02-02 | 2002-02-02 |
| `*date` on c1, then `+date` on c2 | inherited, 2001-01-01 | c2's D card |

`fossil rebuild` keeps each repository's result, since it crosslinks in
the order the artifacts were stored. In the second order c2's time is
neither tag's value: the inherited row does not retime c2 (§4.2 step 6),
and the direct tag that would have was skipped.

## 4. Impact

Observable whenever a check-in's ancestor has a propagating tag newer than
a direct tag of the same name on the check-in. Two repositories that hold
the same artifacts, received in different orders by sync, can disagree on
the check-in's tags (and so its branch and symbolic names) and, for
`date`, on its event time and therefore on leaves, tips and name
resolution ordering.

## 5. Correction

§5.3, step 1:

```diff
    1.  If an existing tagxref row for that (tag, artifact) pair already
        carries an mtime greater than or equal to the incoming tag's
-       mtime, the write MUST be skipped.  Resolution between competing
-       tag applications on the same artifact is by timestamp: a strictly
-       later mtime supersedes an earlier one.  If two competing
-       applications for the same (tag, artifact) pair carry exactly
-       equal mtime, the >= skip rule makes the winner depend on write
-       order, so that equal-mtime case is implementation-defined rather
-       than crosslink-order-independent.
+       mtime, the write MUST be skipped.  The existing row may be a
+       directly applied row or an inherited row written by propagation
+       (srcid zero, carrying the originating tag's mtime); both are
+       compared.  Resolution between competing tag applications on the
+       same artifact is by timestamp: a strictly later mtime supersedes
+       an earlier one.  If two competing applications for the same (tag,
+       artifact) pair carry exactly equal mtime, the >= skip rule makes
+       the winner depend on write order, so that equal-mtime case is
+       implementation-defined rather than crosslink-order-independent.
+
+       Because an inherited row blocks an older direct application here,
+       while a direct row blocks propagation regardless of mtime
+       (Section 5.5), the result also depends on arrival order when an
+       artifact's ancestor carries a propagating tag newer than a direct
+       tag of the same name on the artifact.  If the direct tag is
+       applied first, the artifact keeps the direct value and the
+       propagation stops there.  If the propagation reaches the artifact
+       first, the direct tag is skipped and the artifact and its
+       descendants keep the inherited value.  For a date tag this
+       changes the stored event time (Section 4.2, step 6): the direct
+       value in the first order, the D-card time in the second.  This
+       document does not define an order-independent result for this
+       case; an implementation that must reproduce Fossil applies the
+       rule above as written.
```

## 6. Rationale

States what Fossil compares, so a reader does not limit the skip to direct
rows, and records the order dependence alongside the equal-mtime case the
step already admits. Fossil's rule is kept normative because the
conformance cases, and any rebuild meant to match `fossil rebuild`, depend
on it. Defining an order-independent result (for example, a direct row
always beating an inherited one) would change every tag name, not only
`date`, and is left for a later revision.

## 7. Verification

- Source: Fossil 2.29 `src/tag.c:54-59`, `:66-69`, `:80-82`, `:95-98`,
  `:173-186`, `:229-235`.
- Run: Fossil 2.28, `fossil tag add --raw [--propagate] --date-override`
  applied in both orders to two fresh repositories, read before and after
  `fossil rebuild`, giving the table in Section 3.
- Run: go-libfossil case `repostate.5.3.cancel-older-than-inherited`
  (an older direct cancel skipped against a newer inherited row), which
  agrees with Fossil.
