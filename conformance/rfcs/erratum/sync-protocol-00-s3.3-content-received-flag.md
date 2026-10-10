# Erratum — draft-fossil-sync-protocol-00 §3.3

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-00` |
| **Section** | 3.3 — Phantoms and Deferred Reconstruction (dangling-delta receipt) |
| **Type** | Editorial (clarification — the normative actions are correct; one phrase invites a wrong reading) |
| **Status** | Proposed — operator decision pending |
| **Reported** | 2026-07-10 |
| **Submitter** | Dan Mestas |
| **Found via** | zit conformance implementation (sync §9.3-r5) + independent verification against the Fossil reference implementation |

## 1. Summary

§3.3 correctly specifies the actions a receiver takes when a `file`/`cfile`
card delivers HASH's content as a delta against a DELTASRC it does not hold. But
its closing sentence — "the content-received flag is set" — reads, in isolation,
as if a per-artifact tri-state ("received but unreconstructed") must be recorded
against HASH. No such per-artifact flag exists in the reference implementation:
the "received-but-unreconstructed" condition is represented *implicitly*, and the
"content-received" flag §3.3 refers to is the *per-round* termination flag defined
elsewhere. This erratum adds a cross-reference and one clarifying sentence.

## 2. Location

§3.3 (Phantoms and Deferred Reconstruction), the dangling-delta paragraph
(~lines 599-604), specifically the sentence: "…the delta payload for HASH is
stored as received, the phantom record for HASH is deleted, and the
content-received flag is set."

## 3. The reading hazard

The three states in play are: (1) phantom / never-received; (2) content received
but not yet reconstructable (its delta basis is still a phantom); (3) fully
resolved. A reader implementing §3.3 in isolation naturally asks "where do I
record state 2 per artifact?" — because "the content-received flag is set" sounds
like a per-HASH flag. It is not.

The reference implementation records no such per-artifact flag. A phantom is a
stored object with a negative size and a phantom-table row. On dangling-delta
receipt, the delta bytes are stored as HASH's content (its size becomes
non-negative) and HASH's phantom row is deleted, while a phantom is created for
the missing DELTASRC. State 2 is therefore *implicit*: HASH is a stored object
whose delta-basis chain still terminates at a phantom. "Reconstructable" is
*derived* on demand by walking that chain, not stored. Re-request suppression is
automatic: `gimme` eligibility is driven solely by the phantom set, and HASH is
no longer in it.

The "content-received flag" the sentence names is the **per-round** flag defined
by the protocol state table ("set when any file or cfile card delivers stored
content"), which feeds the §3.7 termination decision — not a per-artifact state.
The single word "flag," unqualified, is what conflates the two.

## 4. Impact

No wire-byte change and no defect in the specified actions — they match the
reference implementation action-for-action. The hazard is implementer-facing: the
phrasing invites a superfluous per-artifact tri-state (a store-model change) that
the protocol neither needs nor uses, and can lead to divergent `gimme` re-request
and termination timing if an implementer gates on a per-artifact flag rather than
the phantom set.

## 5. Correction (additive clarification)

Amend the closing sentence and append a clarifying sentence:

```diff
-   the delta payload for HASH is stored as received, the phantom record
-   for HASH is deleted, and the content-received flag is set.
+   the delta payload for HASH is stored as received, the phantom record
+   for HASH is deleted, and the per-round content-received flag of
+   Section 3.7 is set.  No per-artifact "received" state is recorded: the
+   received-but-unreconstructed condition is represented implicitly by the
+   stored delta payload whose basis remains a phantom, and a receiver
+   suppresses re-requests for HASH solely by removing it from the phantom
+   set.  Reconstructability is derived when the basis arrives, not stored.
```

## 6. Rationale

- **Names the flag's scope.** "content-received flag" becomes "the per-round
  content-received flag of §3.7," removing the per-artifact reading.
- **States the representation the reference implementation actually uses** —
  implicit via a stored delta over a phantom basis — so implementers do not add a
  store-model tri-state the protocol never requires.
- **Ties re-request suppression to the phantom set**, which is where the
  reference implementation drives `gimme` eligibility.

## 7. Application and verification

- The zit reference implementation keeps its artifact store binary
  (phantom / present) and derives the content-received condition at the sync
  layer from the stored/pending delta state, driving `gimme` eligibility from the
  phantom set — mirroring the reference implementation's design.
- Verified by the §9.3 tests: a dangling-delta receipt removes HASH from the
  re-request set without introducing a per-artifact state, and `gimme` for HASH
  is not re-emitted while its DELTASRC is pending.

---

*Prepared against `draft-fossil-sync-protocol-00`. Substantively faithful section;
the correction is editorial. Surfaced when the zit port's §9.3 implementation and
independent verification against the reference implementation showed the
"content-received flag" phrasing invited a per-artifact tri-state the protocol
does not use. The RFC remains the oracle; the reference implementation is used
only to confirm the correct observable behavior, per the interop-verification
policy.*
