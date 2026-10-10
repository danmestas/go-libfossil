# Erratum — draft-fossil-artifact-format-00 §4.1.2

| | |
|---|---|
| **Document** | `draft-fossil-artifact-format-00` |
| **Section** | 4.1.2 — Sterilization |
| **Type** | Technical (three factual divergences from the reference implementation) |
| **Status** | Draft — not yet applied |
| **Reported** | 2026-08-08 |
| **Submitter** | Dan Mestas |
| **Found via** | zit `import-git` mirror classifier, against a 36,383-commit SQLite Git mirror |

## 1. Summary

§4.1.2 describes Fossil's sterilization marker — the comment line a writer adds
to make content deliberately unparseable as a structural artifact. Three of its
statements are wrong against the reference implementation: the marker's
**position**, the **gate that rejects** the sterilized bytes, and the marker's
**literal text**. The section is additionally self-contradictory: it says the
marker is the first line, then says the signed form places it before the
trailing signature block, which is after the cards.

The direction of the error is the dangerous one. An implementer following
§4.1.2 as written builds a sterilizer that does not match Fossil's, and the
mismatch is invisible until a sterilized artifact reaches another repository —
where it surfaces as a foreign check-in rather than as a parse failure.

## 2. Location

§4.1.2 (Sterilization), the paragraph beginning *"This subsection is
informative."* Current text:

> To render content deliberately unparseable as a structural artifact, a writer
> inserts a comment line beginning with # (of the form `# Remove this line to
> create a well-formed Fossil artifact.`) **as the first line**, which causes
> **the line-1 gate of Section 4.1 to fail**. For PGP- or SSH-signed content,
> the marker line is instead spliced in immediately before the signature block…

## 3. The defect

Verified against Fossil 2.28 (`52445a27f1`) by source reading and by running the
code.

### 3.1 Position — the marker is appended, never inserted first

`sterilize_manifest()` (`src/manifest.c:2238`) builds its output with
`blob_appendf`. The marker lands **after the final card**, not before the first.
Observed on a real checkout:

```
U tester
Z 633267631377a908d4e09def80ef9fd5
# Remove this line to create a well-formed Fossil manifest.
```

### 3.2 Failure mechanism — residue, not the line-1 gate

Because the marker is trailing, it never reaches the line-1 gate. It is rejected
by §4.1's rule that any bytes left unconsumed after the card loop MUST cause
rejection — equivalently §4.2.1's residual-bytes clause. A reader that
implemented only the stated line-1 mechanism would **accept** a sterilized
artifact, which is the precise outcome sterilization exists to prevent.

### 3.3 Literal text — the quoted string is never emitted

The format string (`src/manifest.c:2242`) is:

```c
"# Remove this line to create a well-formed Fossil %s.\n"
```

`%s` is `manifest` for `CFTYPE_MANIFEST` and `control artifact` for every other
type. The draft's `…well-formed Fossil artifact.` is emitted by no code path.

### 3.4 Internal contradiction

"as the first line" and "spliced in immediately before the signature block"
cannot both hold: the signature block is trailing, so a marker before it is
after the cards. The signed description is the accurate one, and it
generalizes — the unsigned case is the same insertion point with an empty
signature.

## 4. Impact

Two mechanisms, in opposite directions.

**A reader built to §4.1.2 under-rejects.** It looks for a line-1 marker,
finds none, and parses a sterilized artifact as a structural artifact.

**A writer built to §4.1.2 diverges byte-for-byte from Fossil**, so its output
is not the artifact any Fossil-derived name refers to.

This is not theoretical, and the consumer-side cost is measurable. zit's Git
import proves a mirror's `manifest`/`manifest.uuid` pair by stripping the
sterilizer and checking the result against the name the mirror recorded. Written
to the trailing-only reading of the implementation — which is correct for the
unsigned case — it failed for every PGP-clearsigned check-in, because there the
marker sits mid-blob, immediately before `-----BEGIN PGP SIGNATURE-----`. On a
36,383-commit SQLite mirror:

| Proof outcome | Count |
|---|---|
| Proven by trailing strip | 35,307 |
| Proven **only** once the marker is spliced from before the signature block | 1,076 |
| Unproven | 0 |

All 1,076 date from 2009–2011, when SQLite's `clearsign` setting was on. Each
had its mirror metadata retained permanently — the observable symptom of a
correct implementation built against an incorrect description of where the
marker goes.

## 5. Correction applied

Replace the first paragraph of §4.1.2 with text that states the position once,
in the general (signature-aware) form, names the correct rejection mechanism,
and quotes the two literals the implementation actually emits:

```diff
-   This subsection is informative.  To render content deliberately
-   unparseable as a structural artifact, a writer inserts a comment line
-   beginning with # (of the form # Remove this line to create a well-
-   formed Fossil artifact.) as the first line, which causes the line-1
-   gate of Section 4.1 to fail.  For PGP- or SSH-signed content, the
-   marker line is instead spliced in immediately before the signature
-   block, with the boundary recomputed by the same wrapper-detection
-   logic of Section 4.1.1, so the signature remains trailing.
+   This subsection is informative.  To render content deliberately
+   unparseable as a structural artifact, a writer appends a comment line
+   beginning with # after the final card, and before the signature block
+   when the content is PGP- or SSH-signed, so that a signature remains
+   trailing; the boundary is located by the same wrapper-detection logic
+   of Section 4.1.1.  The marker reads # Remove this line to create a
+   well-formed Fossil manifest. for a check-in and # Remove this line to
+   create a well-formed Fossil control artifact. for any other type.
+   Because the marker follows the cards, it is Section 4.1's rule that
+   unconsumed bytes after the card loop MUST cause rejection —
+   equivalently the residual-bytes clause of Section 4.2.1 — that rejects
+   the artifact, not the line-1 gate.
```

## 6. Rationale

- **One statement of position.** The contradiction exists because position is
  asserted twice. Stating it once, in the form that covers both cases, removes
  it.
- **The mechanism is load-bearing.** Naming the wrong gate produces a reader
  that fails open on exactly the input sterilization targets. This is the most
  consequential of the three defects.
- **The literals are quoted because they are compared.** Consumers match the
  marker to strip it; a consumer matching the draft's string matches nothing
  Fossil writes.
- **Informative, not normative.** §4.1.2 does not impose requirements — the
  correction changes only what the section describes, not what any reader or
  writer is required to do.

### Note on the producer side

Fossil's own use of `sterilize_manifest()` narrowed in `722c248d53` (shipped
2.18, 2022-02-23), which removed it from `tar.c` and `zip.c`, so archive exports
are no longer sterilized while checkouts and Git export still are. That is a
producer-policy change and needs no draft text; it is recorded here because it
makes §4.1.2's accuracy matter more, not less — consumers can no longer assume
content arriving from a Fossil-derived tarball has been sterilized at all.

---

*Prepared against the frozen final `draft-fossil-artifact-format-00`. The zit
reference implementation already emits and strips the marker at the position
described in §5; this erratum records the divergence so the draft can settle it.*

## 7. Resolution verification

- All three defects verified twice: by reading Fossil 2.28 source
  (`src/manifest.c:2238`, `:2242`) and by observing a live checkout's `manifest`
  file and a published SQLite mirror's blobs.
- The 1,076/35,307/0 split is reproducible: export `sqlite.fossil` with
  `fossil git export`, then for each commit compare `manifest` stripped both
  ways against `manifest.uuid`.
- Clean-room effect: an implementer no longer has to discover, from a foreign
  check-in appearing in an unrelated repository, that the marker is trailing
  rather than leading and that the text varies by artifact type.
