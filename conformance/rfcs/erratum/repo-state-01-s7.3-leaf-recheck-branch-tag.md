# Erratum — draft-fossil-repo-state-01 §7.3 / §7.6

| | |
|---|---|
| **Document** | `draft-fossil-repo-state-01` |
| **Section** | 7.3 — Maintenance algorithm (full rebuild and deferred scheduling); 7.6 — Triggers for re-evaluation |
| **Type** | Technical (the draft's leaf table differs from Fossil's on real repositories) |
| **Status** | Applied in `draft-fossil-repo-state-01` (2026-10-13) |
| **Reported** | 2026-10-13 |
| **Submitter** | Dan Mestas |
| **Found via** | go-libfossil's corpus comparison against `fossil rebuild` (Fossil 2.28) on a copy of `libfossil.fossil`; confirmed by reading Fossil 2.29's source and by crafted artifacts crosslinked with Fossil 2.28 |

## 1. Summary

§7.3 makes every check-in a leaf candidate and schedules a recheck for
every crosslinked check-in and its parents. Fossil does neither. It
rechecks a check-in only when a branch tag is applied to it or propagated
to it, and then also rechecks that check-in's parents. A check-in that
never receives a branch tag, and is not the parent of one that does, is
never rechecked, so it is never in Fossil's leaf table. The informative
paragraph in §7.3 records this for parentless check-ins only, and says
"Every check-in Fossil writes carries a branch tag, so the two agree on
Fossil-authored history". That is not so: an implementation that followed
the normative text produced a leaf table that differed from
`fossil rebuild`'s on a real Fossil-authored repository.

## 2. Location

§7.3, the "Full rebuild" bullet (RS-7.3-1), the "Deferred scheduling"
bullet (RS-7.3-3) and the informative paragraph after the bullets; §7.6,
the third bullet.

## 3. The reference behavior

Verified against Fossil 2.29 source; behavior run on Fossil 2.28.

- `leaf_eventually_check()` (`src/leaf.c:178-191`) adds the check-in and
  every parent named in `plink` (merge parents included) to the pending
  set. `leaf_do_pending_checks()` (`:195-200`) runs `leaf_check()` on each
  at transaction commit (`src/db.c:308`).
- `leaf_eventually_check()` has two callers: `tag_insert()` for a branch
  tag of any type (`src/tag.c:195`), and `tag_propagate()` for each child
  the branch tag's walk updates or deletes (`src/tag.c:100-102`).
  `manifest_crosslink()` and `rebuild_db()` never call it, and neither
  calls `leaf_check()` or `leaf_rebuild()`.
- `rebuild_db()` drops the `leaf` table with the other derived tables
  (`src/rebuild.c:399-418`) and refills it only through the crosslink
  of every artifact, so `fossil rebuild` produces exactly the table the
  scheduling rule produces. `leaf_rebuild()` (`src/leaf.c:89-104`) is
  reached only from `fossil leaves --recompute`
  (`src/descendants.c:458`).
- `leaf_check()` (`src/leaf.c:115-146`) applies the §7.2 test, treating a
  check-in with no branch row as `trunk`.

Crafted artifacts crosslinked with Fossil 2.28, one transaction each:

- An untagged check-in with no parent: not a leaf.
- An untagged check-in whose parent is that untagged root: not a leaf,
  although it has no child at all.
- An untagged root whose only child carries `*branch * feat`: a leaf. It
  is rechecked as the parent of a tagged check-in, and its child is on
  another branch. A parentless untagged check-in is therefore not always
  absent from the table.
- A check-in with `+branch * trunk` (a singleton, so the child inherits
  nothing), then its untagged child in a later transaction: the parent
  stays in the leaf table although the child is on the same branch
  (`trunk` by default), because nothing rechecks the parent. Crosslinked
  child first, or both in one transaction (as `fossil rebuild` does), the
  parent is not a leaf. The child is not a leaf in either order.

## 4. Impact

Observable. The leaf table feeds open-leaf enumeration, leaf ambiguity
and branch-tip resolution (§7.5, §7.7, §8). On repositories that hold
check-ins with no branch tag, such as a history whose root check-in has
none, a reader that follows §7.3 lists leaves that
Fossil does not, and cannot rebuild a repository to Fossil's leaf table.

## 5. Correction

§7.3, the "Full rebuild" bullet:

```diff
-   *  Full rebuild: implementations MUST support recomputing the entire
-      leaf table by selecting every check-in and excluding those that
-      have a same-branch child, with no primary/non-primary filter
-      applied.  The candidate set is every check-in, not only those that
-      appear as a child of some parent-link.  A check-in with no parent
-      at all (for example the sole check-in of a new repository, or any
-      root check-in) is a leaf when it has no same-branch child, and
-      MUST be included.  This is a whole-table recomputation, so its
-      cost scales with repository size.  Implementations SHOULD reserve
-      it for explicit rebuild operations rather than routine updates.
+   *  Full rebuild: implementations MUST support recomputing the entire
+      leaf table.  The result MUST be the table that crosslinking every
+      check-in in a single batch produces under the scheduling rule
+      below: the candidates are the check-ins to which a branch tag of
+      any type is applied or propagated, and the parents of those check-
+      ins; each candidate is a member if it has no same-branch child,
+      with no primary/non-primary filter applied.  A check-in that is
+      not a candidate is not a member, even when it has no same-branch
+      child: the leaf table holds the candidates of this section that
+      pass the test of Section 7.2, and that test alone does not decide
+      membership.  This is a whole-table recomputation, so its cost scales
+      with repository size.  Implementations SHOULD reserve it for
+      explicit rebuild operations rather than routine updates.
```

§7.3, the "Deferred scheduling" bullet:

```diff
-   *  Deferred scheduling: implementations MUST schedule an incremental
-      leaf recheck for the affected check-in and for each of its direct
-      parents when a check-in is crosslinked, when a parent-link is
-      created, or when a branch tag is applied or propagated.
-      Establishing a new child can retroactively remove a parent's leaf
-      status.
+   *  Deferred scheduling: implementations MUST schedule an incremental
+      leaf recheck for a check-in and for each of its direct parents,
+      merge parents included, when a branch tag of any type is applied
+      to that check-in, and when the propagation walk of a branch tag
+      (Section 5.4) updates or deletes that check-in's branch entry.
+      Crosslinking a check-in or creating a parent-link MUST NOT by
+      itself schedule a recheck.  A check-in that never receives a
+      branch tag is therefore rechecked only as the parent of a check-
+      in that does.
```

§7.3, the informative paragraph:

```diff
    This paragraph is informative.  Fossil's full leaf recompute (fossil
    leaves --recompute) draws its candidates from the child column of the
    parent-link table, so it omits every check-in with no parent, check-
-   ins this section requires to be included.  Fossil's incremental
-   maintenance schedules a recheck only when a branch tag is set on a
-   check-in or propagated to it, so a check-in with no parent and no
-   branch tag never enters its leaf table.  Every check-in Fossil writes
-   carries a branch tag, so the two agree on Fossil-authored history.
+   ins the full rebuild above can include; fossil rebuild does not use
+   it.  Under the scheduling rule above, a check-in with no branch tag
+   is never a leaf unless it is the parent of a check-in that receives
+   one, and a check-in keeps its verdict when a child that inherits no
+   branch row is linked to it in a later batch.  (An ordinary child with
+   no T card does inherit its parent's propagating branch row when it is
+   crosslinked, which schedules the child and the parent; the verdict is
+   kept only when the parent's branch row is a singleton or absent.)
+   The leaf table can therefore omit check-ins that the test of
+   Section 7.2 would accept, and can keep a check-in whose
+   same-branch child arrived later.  An ordinary commit carries no
+   branch card and inherits its branch tag along the primary parent,
+   so a history whose root check-in has no branch tag, as some older
+   or imported histories do, consists of untagged check-ins.
```

§7.6, the third bullet:

```diff
-   *  Crosslinking a check-in, and creating a parent-link, MUST trigger
-      a re-evaluation of the check-in and of each of its direct parents,
-      since a new child can remove a parent's leaf status and a check-in
-      with no parent and no branch tag receives no other trigger.
+   *  Crosslinking a check-in, and creating a parent-link, do not by
+      themselves trigger a re-evaluation (Section 7.3).  A check-in
+      that inherits a branch tag from its parent when it is crosslinked
+      is re-evaluated, with its parents, under the second bullet.
```

## 6. Rationale

The leaf table exists so that leaf, tip and ambiguity answers agree with
Fossil's; a definition that cannot reproduce `fossil rebuild` on a real
repository defeats that. The correction states Fossil's rule normatively,
as the corrections to §6.2 and §6.6 did, and keeps the §7.2 test as the
test applied to each candidate. The informative paragraph now records the
two consequences a reader would otherwise rediscover: untagged check-ins
are absent, and a parent's verdict is not revisited when an untagged child
arrives later.

This correction reverses option A of the -00 §7.3 erratum
(`repo-state-00-s7.3-root-leaf.md`), which kept every check-in as a
candidate and recorded Fossil's divergence in an informative note. That
erratum rejected option B for two reasons. It would have written the
`fossil leaves --recompute` defect into the draft; this correction avoids
that by defining the full rebuild as the result of crosslinking every
check-in in a single batch, which is what `fossil rebuild` produces and
which does include parentless check-ins that receive a branch tag. And it
rested on the premise that every check-in Fossil writes carries a branch
tag; `libfossil.fossil` disproves that, holding one check-in with no
branch row (rid 1496, 4b05c2c59f, 2013-08-14, one parent, no children)
that is absent from Fossil's 114-row leaf table and would be a leaf under
-01 §7.2.

## 7. Verification

- Source: Fossil 2.29 `src/leaf.c:89-104`, `:115-146`, `:178-200`;
  `src/tag.c:100-102`, `:195`; `src/db.c:308`; `src/rebuild.c:399-418`;
  `src/descendants.c:458`.
- Run: hand-built check-ins stored with `fossil test-content-put` and
  crosslinked with `fossil test-crosslink` (Fossil 2.28), before and
  after `fossil rebuild`, giving the results in Section 3.
- Run: go-libfossil cases `repostate.7.3.parentless-untagged`,
  `repostate.7.3.recheck-lone-untagged` and
  `repostate.7.3.recheck-untagged-child`, with their `.rebuild`
  variants, which follow Fossil and are marked as deviations from the
  -01 text.
- Run: `libfossil.fossil`, read with Fossil. Its one check-in with no
  branch row (rid 1496, 4b05c2c59f) is absent from the 114-row leaf
  table, and would be a leaf under the -01 text.
