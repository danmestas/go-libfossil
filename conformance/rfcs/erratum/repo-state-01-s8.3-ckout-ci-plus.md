# Erratum — draft-fossil-repo-state-01 §8.3

| | |
|---|---|
| **Document** | `draft-fossil-repo-state-01` |
| **Section** | 8.3 — Procedure, step 3 (the *ckout* keyword) |
| **Type** | Technical (the keyword is recognized for a narrower request type than the draft says) |
| **Status** | Applied in `draft-fossil-repo-state-01` (2026-10-13) |
| **Reported** | 2026-10-13 |
| **Submitter** | Dan Mestas |
| **Found via** | Reading Fossil 2.29's `symbolic_name_to_rid()` against §8.3 |

## 1. Summary

§8.3 says *ckout* resolves to the checkout sentinel "only when the
requested type is "ci" or a checkout-inclusive type". Fossil recognizes it
only for the request type `ci+`. For `ci`, for an unconstrained request,
and for any other type, `ckout` is not a keyword and falls through to the
remaining forms, where it resolves like any other name (for example to a
check-in tagged `ckout`), or not at all. The draft also leaves "checkout-
inclusive type" undefined, and does not say that the keyword needs an
open checkout. The "ci+" semantics also apply only to the name as given:
Fossil rewrites the type to "ci" before the forms that resolve a
contained name and before the trailing-punctuation retry, so `ckout.`
under "ci+" names a tag called ckout, not the checkout.

## 2. Location

§8.3, step 3, the *ckout* bullet:

```
       *  *ckout* (only when the requested type is "ci" or a checkout-
          inclusive type) resolves to the sentinel checkout RID rather
          than an ordinary artifact RID, and marks that a local checkout
          is in use.
```

## 3. The reference behavior

Verified against Fossil 2.29 source.

- `symbolic_name_to_rid()` (`src/name.c:482-536`) sets
  `isCheckin = 1` for type `ci` and `isCheckin = 2` for type `ci+`,
  rewriting `ci+` to `ci` for the rest of the lookup (`:493-499`).
- The keyword block runs only when the type is unconstrained or a
  check-in type and a checkout is open (`0<ridCkout`, `:519`). Within it,
  `current`, `prev`, `previous` and `next` need no more, but `ckout` is
  tested as `isCheckin>1 && fossil_strcmp(zTag, "ckout")==0` (`:529`),
  so only `ci+` matches. It returns `RID_CKOUT` (`RID_MAX+1`, `:30-34`)
  and sets `g.localOpen` to the checkout's RID (`:530-532`).
- The function's header comment (`:462-468`) documents the same: "A
  value of "ci+" works like "ci" but adds these semantics: if zTag is
  "ckout" and a checkout is open, "ci+" causes RID_CKOUT to be returned".
- `name_to_uuid()` (`:834-836`) maps `RID_CKOUT` back to the checkout's
  RID before reading its hash.
- The rewrite of `ci+` to `ci` (`:497-498`) happens before the forms
  that resolve a contained name, `root:`, `start:` and `merge-in:`
  (`:585`, `:591`, `:597`), and before the trailing-punctuation retry
  (`:730`, `:739`), all of which pass the rewritten type on. The keyword
  therefore applies only when `ckout` is the whole name as given.
- No caller in Fossil 2.29's `src/` passes `ci+`; the only occurrences
  of the string are in `name.c`. Commands that accept `ckout` (for
  example the `/doc` page, `src/doc.c:973`, `:1033`) test for it
  themselves before resolving a name.

## 4. Impact

Observable only in corner cases. A resolver that follows the draft
returns the sentinel for `ckout` with type `ci`, where Fossil resolves
`ckout` as an ordinary name; a repository with a `ckout` tag then gives a
different check-in, and one without such a tag gives no check-in in
Fossil and the sentinel in the reader's resolver.

## 5. Correction

§8.3, step 3:

```diff
-       *  *ckout* (only when the requested type is "ci" or a checkout-
-          inclusive type) resolves to the sentinel checkout RID rather
-          than an ordinary artifact RID, and marks that a local checkout
-          is in use.
+       *  *ckout* is a keyword only when the requested type is "ci+"
+          and a local checkout is open.  It then resolves to the
+          sentinel checkout RID rather than an ordinary artifact RID,
+          and marks that a local checkout is in use; a caller that
+          needs the check-in itself maps the sentinel to the checkout's
+          RID.  The type "ci+" otherwise behaves as "ci".  For any other
+          requested type, including "ci" and an unconstrained request,
+          or when no checkout is open, "ckout" is not a keyword and is
+          resolved by the remaining forms of this procedure like any
+          other name.  The "ci+" semantics apply only to the name as
+          given: the trailing-punctuation retry below and every form
+          that resolves a contained name (root:, start:, merge-in:)
+          resolve that name under "ci", where "ckout" is an ordinary
+          name.
```

## 6. Rationale

Names the one type that enables the keyword, replacing the undefined
"checkout-inclusive type", states the open-checkout condition the other
keywords in the list already carry, and says what happens to the name
when the keyword does not apply, so a resolver gives Fossil's answer for
`ckout` under `ci`.

## 7. Verification

- Source: Fossil 2.29 `src/name.c:30-34`, `:462-468`, `:482-536`,
  `:834-836`; `src/doc.c:973`, `:1033`.
- Run: Fossil 2.28 in an open checkout. `fossil info current` resolves;
  `fossil info ckout` fails with `cannot resolve name: ckout`; after
  `fossil tag add ckout` on another check-in, `fossil info ckout` shows
  that check-in, not the checkout.
- Not run: no Fossil 2.29 command passes `ci+`, so the sentinel path is
  read from the source only.
- Run (go-libfossil, Fossil 2.28): `fossil test-name-to-id --type ci+
  ckout.` with the checkout on c2 and a tag named ckout on c1 answers
  c1, the tag.
