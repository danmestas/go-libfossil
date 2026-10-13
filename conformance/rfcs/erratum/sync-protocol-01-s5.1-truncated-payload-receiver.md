# Erratum — draft-fossil-sync-protocol-01 §5.1

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-01` |
| **Section** | 5.1 — Message Structure (a body that ends inside a payload) |
| **Type** | Technical (the receiver's duty is unstated) |
| **Status** | Applied in `draft-fossil-sync-protocol-01` (2026-10-12) |
| **Reported** | 2026-10-12 |
| **Submitter** | Dan Mestas |
| **Found via** | A reader implementing §5.1 from the text alone |

## 1. Summary

§5.1 says a body that ends before a payload's declared count is malformed
and that senders MUST NOT produce one, and it records that Fossil uses the
bytes present. It does not say what a conforming receiver does with one. The
draft already has the mechanism: a validation failure in the file, cfile or
uvfile handler (§5.2) on the server, and the handler content error of §10.1
on the client, which keeps the processed prefix and continues.

## 2. Location

§5.1, the sentence "A body that ends before a payload's declared count is
malformed; senders MUST NOT produce one."

## 3. The reference behavior

Fossil 2.28 `blob_extract()` (`src/blob.c:674-690`) clamps the count to the
bytes remaining and returns them, so a server stores the short payload as a
full-content file and a client does the same. No error is reported. The
stored bytes hash to HASH only by coincidence. For a handler error the
client does warn, count it and leave the reply loop (`src/xfer.c:2954-2958`),
which is the §10.1 behavior the correction names.

## 4. Impact

Observable. Without a stated duty, one receiver copies Fossil's leniency and
stores a wrong-length artifact, another stops with an error, and a third
reads the following bytes as cards.

## 5. Correction

```diff
    only at the declared count.  A body that ends before a payload's
-   declared count is malformed; senders MUST NOT produce one.  (Fossil
-   instead uses the bytes that are present, and stores a full-content
-   file whose shorter payload still hashes to HASH.)  A cfile sender
+   declared count is malformed; senders MUST NOT produce one.  A
+   receiver MUST NOT store the partial payload: a server treats the
+   card as a validation failure of its handler (Section 5.2), with
+   the error text malformed file line, malformed cfile line or
+   malformed uvfile line, and a client stops reading the reply at
+   that card, keeps the prefix it has already processed, and
+   continues as Section 10.1 describes for a handler content error.
+   (Fossil instead uses the bytes that are present, and stores a
+   full-content file whose shorter payload still hashes to HASH.)  A
+   cfile sender
```

## 6. Rationale

Reuses the existing handler-failure paths on both sides rather than
inventing a new one, names the error texts §5.2 and Appendix C already
define, and keeps Fossil's leniency as a recorded divergence.

## 7. Verification

- Source: Fossil 2.28 `src/blob.c:674-690`; `src/xfer.c:160`, `:280`,
  `:349` (the file, cfile and uvfile extraction sites); `:2954-2958`.
