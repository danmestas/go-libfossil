# Erratum — draft-fossil-sync-protocol-01 §5.3

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-01` |
| **Section** | 5.3 — Comment Lines (the value of N in the timestamp comment) |
| **Type** | Technical (N is never defined; receivers are told not to act on it) |
| **Status** | Applied in `draft-fossil-sync-protocol-01` (2026-10-12) |
| **Reported** | 2026-10-12 |
| **Submitter** | Dan Mestas |
| **Found via** | A reader implementing §5.3 from the text alone |

## 1. Summary

§5.3 gives the syntax of `# timestamp DATETIME errors N` and says N is
informational, but never says what N counts. A sender has nothing to put
there.

## 2. Location

§5.3, the paragraph beginning "DATETIME is exactly 19 bytes".

## 3. The reference behavior

Fossil 2.28 `page_xfer()` keeps a counter `nErr` (`src/xfer.c:1278`) that is
incremented at each error card the server's own card handling emits:
wrong project, the authorization refusals, multiple or failed login, the
file, cfile and uvfile handler failures, and the script failures
(`:1330-1904`). The closing comment prints it (`:1970`). The counter records errors
raised, not cards present: a later reset discards the cards of earlier
errors but not their count, so a push refused at `:1517` followed by a
file card refused at `:1363` leaves one error card and `errors 2`. Three
error cards are never counted: the unknown-operator `bad command` error
(`:1893`), the private-content refusal emitted by
`server_private_xfer_not_authorized()` (`:1138-1140`), and the SHA3 version
refusal emitted by `xfer_cannot_send_sha3_error()` (`:523-527`), which is why
Appendix A.2 shows that error followed by `errors 0`.

## 4. Impact

Cosmetic. N is informational and no receiver acts on it, but a cleanroom
sender needs a definition.

## 5. Correction

```diff
    additional internal validation; DATETIME estimates skew, while N is
-   informational and not a wire error.
+   informational and not a wire error.  N counts the errors the
+   server raised while handling the request; it can exceed the error
+   cards present, because a reset discards earlier output.  Fossil's
+   server does not count the unknown-operator error, the private-
+   content refusal, or the SHA3 version refusal.  A receiver MUST NOT
+   act on N.
```

## 6. Rationale

Defines N as what Fossil's counter holds, records the three cards Fossil
leaves out and the reset effect so a byte-comparing test is not surprised,
and keeps N informational.

## 7. Verification

- Source: Fossil 2.28 `src/xfer.c:1278`, every `nErr++` between `:1330` and
  `:1904`, `:1893`, `:1138-1140`, `:523-527`, `:1970`; Appendix A.2 of the
  draft.
