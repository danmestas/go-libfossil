# Erratum — draft-fossil-repo-state-01 §6.2

| | |
|---|---|
| **Document** | `draft-fossil-repo-state-01` |
| **Section** | 6.2 — Branch creation (the cancel cards) |
| **Type** | Technical (the normative sentence names the wrong check-in) |
| **Status** | Applied in `draft-fossil-repo-state-01` (2026-10-12) |
| **Reported** | 2026-10-12 |
| **Submitter** | Dan Mestas |
| **Found via** | A reader implementing §6.2 from the text alone |

## 1. Summary

The normative paragraph says branch creation "MUST also cancel every
pre-existing active sym-* tag on the branch-root check-in". The branch-root
check-in is the one being written, so it has no pre-existing tags; the tags
to cancel are the ones it would inherit from its parent. The informative
paragraph added in -01 says Fossil computes the cards from the parent, but
the normative sentence still points at the root, and a reader cannot tell
whether the new branch's own sym-* tag is among the tags to cancel.

## 2. Location

§6.2, second paragraph.

## 3. The reference behavior

Verified against Fossil 2.28. `commit --branch` (`src/checkin.c:2100-2112`)
emits `T -sym-NAME *` for each propagating `sym-*` tag of the parent
check-in other than the new branch's own. `branch new` (`src/branch.c:
170-183`) emits one for every active `sym-*` tag of the parent, singletons
included. In both, the source is the parent, never the root.

## 4. Impact

Observable in corner cases only: an implementation that reads "on the
branch-root check-in" literally emits no cancel cards, and the root then
inherits the parent's symbolic names, which §6.2 says it must not.

## 5. Correction

```diff
-   Branch creation MUST also cancel every pre-existing active sym-* tag
-   on the branch-root check-in by emitting corresponding - (cancel)
-   cards, because symbolic tags are otherwise inherited from a check-
-   in's parent.  This ensures a newly created branch does not carry
-   forward the parent branch's symbolic names alongside its own.
+   Branch creation MUST also cancel every sym-* tag the branch-root
+   check-in would otherwise inherit from its parent, that is, every
+   propagating sym-* tag active on the parent other than the new
+   branch's own, by emitting a corresponding - (cancel) card on the
+   root.  This ensures a newly created branch does not carry forward
+   the parent branch's symbolic names alongside its own.  Cancel cards
+   for tags the root would not inherit are permitted and have no
+   effect.
```

Also, in the informative paragraph after "the extra cards cancel nothing on
the root.":

```
   If the parent itself carries a propagating sym-* tag of the new
   branch's own name, branch new emits both the propagating card and a
   cancel card for that name; this document does not define their
   combined effect.
```

## 6. Rationale

Names the check-in whose tags are read, excludes the new branch's own tag
(cancelling it would undo the creation), and keeps `branch new`'s extra
cards conformant, as the informative paragraph already describes them.

## 7. Verification

- Source: Fossil 2.28 `src/checkin.c:2100-2112`, `src/branch.c:170-183`.
