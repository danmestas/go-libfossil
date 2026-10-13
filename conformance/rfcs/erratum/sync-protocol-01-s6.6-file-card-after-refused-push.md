# Erratum — draft-fossil-sync-protocol-01 §6.6

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-01` |
| **Section** | 6.6 — Capability Model (file and cfile cards when push is not enabled) |
| **Type** | Technical (the text says later cards are still processed; Fossil stops at the first file or cfile card) |
| **Status** | Applied in `draft-fossil-sync-protocol-01` (2026-10-12) |
| **Reported** | 2026-10-12 |
| **Submitter** | Dan Mestas |
| **Found via** | A reader implementing §6.6 from the text alone, then Fossil's source |

## 1. Summary

The -01 refusal paragraph says that after a refused push "later file and
cfile cards are refused individually, and later cards, including a pull, are
still processed". The form of the refusal is not given, and the second half
is wrong: in Fossil the first file or cfile card that arrives while push is
not enabled is answered with a reset `error not authorized to write` and
ends card processing for the request.

## 2. Location

§6.6, the sentence beginning "In both forms push is not enabled for the
request".

## 3. The reference behavior

Fossil 2.28 `page_xfer()` (`src/xfer.c:1360-1392`): for a `file` or `cfile`
card, `if( !isPush )` resets the accumulated output, emits
`error not\sauthorized\sto\swrite`, increments the error count, and executes
`break`, which leaves the card loop. The same `break` follows a handler
validation failure. A `uvfile` card is not gated on push and continues.
After the loop, the end-of-request work for an accepted pull still runs
(`:1922-1925`), as §6.4 already states for a failed login.

## 4. Impact

Observable. A conforming server that keeps parsing would answer a later
`pull` card with inventory that Fossil would not send, and would process
cards Fossil never reads.

## 5. Correction

```diff
-   forms push is not enabled for the request, so later file and cfile
-   cards are refused individually, and later cards, including a pull,
-   are still processed.  A client that sends both cards SHOULD send
+   forms push is not enabled for the request.  Later cards, including
+   a pull, are still processed until the first file or cfile card:
+   that card is refused with reset output and error not authorized to
+   write, and ends card processing for the request, as a handler
+   validation failure does (Section 5.2).  The end-of-request steps
+   for a direction established earlier, such as the igot inventory
+   for an accepted pull, are still performed (Section 6.4).  A client
+   that sends both cards SHOULD send
```

## 6. Rationale

States the refusal form and the stop, which are the same reset-and-stop
shape §5.2 already defines for handler failures.

## 7. Verification

- Source: Fossil 2.28 `src/xfer.c:1360-1392`, `:1922-1925`.
