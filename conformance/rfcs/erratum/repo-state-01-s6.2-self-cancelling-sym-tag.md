# Erratum: draft-fossil-repo-state-01 §6.2 and §5.3

| | |
|---|---|
| **Document** | `draft-fossil-repo-state-01` |
| **Section** | 6.2, Branch creation (a sym tag both applied and cancelled on the root); 5.3, Direct Application Procedure, step 1 |
| **Type** | Technical (a combined effect left undefined that Fossil determines) |
| **Status** | Applied in `draft-fossil-repo-state-01` (2026-10-14) |
| **Reported** | 2026-10-14 |
| **Submitter** | Dan Mestas |
| **Found via** | A review of the drafts from libfossil; confirmed by running Fossil 2.28 and reading its source |

## 1. Summary

§6.2 records that `fossil branch new` can write both `T *sym-NAME *` and
`T -sym-NAME *` on a branch root, when the parent already carries a
propagating `sym-NAME`, and then says "this document does not define their
combined effect". Fossil determines it. Crosslink applies an artifact's tag
cards one at a time in card order, all with the artifact's own mtime, so the
§5.3 skip rule decides: the `*` card sorts first and writes the row, and the
`-` card meets a row with an equal mtime and is skipped. The root keeps the
propagating tag.

§5.3 calls the equal-mtime case implementation-defined because the winner
depends on write order. For two cards of one artifact the order is fixed by
the artifact, so the case is defined once the draft says the cards are
applied in card order.

## 2. The text in error

Page 18 (§6.2, informative paragraph):

> If the parent itself carries a propagating sym-* tag of the new branch's
> own name, branch new emits both the propagating card and a cancel card for
> that name; this document does not define their combined effect.

Page 15 (§5.3 step 1):

> If two competing applications for the same (tag, artifact) pair carry
> exactly equal mtime, the >= skip rule makes the winner depend on write
> order, so that equal-mtime case is implementation-defined rather than
> crosslink-order-independent.

## 3. What Fossil implements

`manifest_crosslink()` in `src/manifest.c`:

```
for(i=0; i<p->nTag; i++){
  ...
  switch( p->aTag[i].zName[0] ){
    case '-':  type = 0;  break;
    case '+':  type = 1;  break;
    case '*':  type = 2;  break;
  }
  tag_insert(&p->aTag[i].zName[1], type, p->aTag[i].zValue,
             rid, p->rDate, tid);
}
```

Every card of the artifact goes to `tag_insert()` with the same mtime,
`p->rDate`, in the order the cards appear. The artifact format sorts `T`
cards by their full name token, sign included ([FossilArtifact] §4.7.16),
and `*` (0x2A) and `+` (0x2B) sort before `-` (0x2D). `tag_insert()` skips a
write when the existing row's mtime is greater than or equal to the
incoming one (§5.3 step 1).

Checked with Fossil 2.28 [52445a27f1]: commit c1 on trunk; commit c2 with
`--branch feat`; add a `closed` tag to c2; `fossil branch new feat c2`. The
new root's manifest carries

```
T *branch * feat
T *sym-feat *
T -sym-feat *
```

and its tagxref rows after crosslink, and again after `fossil rebuild`, are

| tag | tagtype | srcid |
|---|---|---|
| `branch` | 2 (`feat`) | the root |
| `sym-feat` | 2 | the root |

The cancel left nothing behind. zit trunk, given the same repository
through `import-fossil`, lists `branch feat` and `sym-feat` as active on the
root.

## 4. Corrected text

§5.3 step 1, replace the sentence quoted in Section 2 with:

> If two competing applications for the same (tag, artifact) pair carry
> exactly equal mtime, the >= skip rule makes the winner depend on write
> order. The tag cards of one artifact MUST be applied in the order they
> appear in the artifact, all with that artifact's mtime, so between two
> cards of one artifact the first in card order wins. Between applications
> from different artifacts with equal mtime the case is
> implementation-defined rather than crosslink-order-independent.

§6.2, replace the sentence quoted in Section 2 with:

> If the parent itself carries a propagating sym-* tag of the new branch's
> own name, branch new emits both the propagating card and a cancel card for
> that name. The card order decides their combined effect (Section 5.3,
> step 1): the sign is part of the ordering key ([FossilArtifact],
> Section 4.7.16), so the * card precedes the - card, and the cancel meets
> the row the * card wrote with an equal mtime and is skipped. The root
> carries the propagating sym-* tag as if the cancel card were absent.

## 5. Observable consequence of the uncorrected text

An implementation that applies a cancel last regardless of card order, or
that treats the case as undefined and picks the cancel, leaves the root
without its own `sym-NAME`. Branch listings and name resolution for that
branch then differ from Fossil's for the same artifacts.

## 6. Implementations

zit already applies an artifact's tag cards in card order and matches
Fossil. No change is needed.
