# Erratum — draft-fossil-sync-protocol-01 §10.1

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-01` |
| **Section** | 10.1 — Client reply processing (a handler-reported integrity error) |
| **Type** | Technical (a received file whose content does not match its hash fails the round in Fossil; the draft has the client commit the prefix and continue) |
| **Status** | Applied in `draft-fossil-sync-protocol-01` (2026-10-13) |
| **Reported** | 2026-10-13 |
| **Submitter** | Dan Mestas |
| **Found via** | go-libfossil's conformance cases for §10.1, run against Fossil 2.28; confirmed by reading Fossil 2.29's `xfer_accept_file()` and `verify.c` |

## 1. Summary

§10.1 says "A handler-reported content or integrity error warns, preserves
and commits the processed prefix, and follows ordinary continuation." For
a `file` or `cfile` card whose reconstructed content does not match its
HASH, Fossil does not do that. Its handler records the warning but stores
the content anyway, and the stored record is verified when the round's
transaction commits. That verification fails, the transaction is never
committed, and the process aborts: the round's state, the processed prefix
included, is rolled back, and no further round starts. In the draft's own
terms this is an explicit fail-fast path, not a preserved prefix with
continuation.

## 2. Location

§10.1, the sentence beginning "A handler-reported content or integrity
error".

## 3. The reference behavior

Verified against Fossil 2.29 source; behavior run on Fossil 2.28.

- `xfer_accept_file()` (`src/xfer.c:134-222`) checks the content against
  the HASH only on the non-clone path (`hname_verify_hash`, `:210-212`)
  and on a mismatch appends `wrong hash on received artifact: HASH` to
  the transfer's error text, then stores the content regardless
  (`content_put_ex`, `:214`). On the clone path (`:180`, `:192`) there is
  no check at receipt.
- `content_put_ex()` registers every record it writes for verification
  at commit (`src/content.c:643`, `:945`, calling
  `verify_before_commit()`). `verify_at_commit()` (`src/verify.c:62-75`)
  is a commit hook; `verify_rid()` (`:50`) calls `fossil_panic` when a
  record's hash does not match its name. The round's transaction is
  therefore never committed, and the process ends.
- A malformed card, a shunned or unauthorized private file, and a
  refused push are handler-reported errors that leave the stored
  artifacts consistent; for those the draft's prefix rule stands.

Run (go-libfossil, Fossil 2.28): one reply carrying `igot U` and then
`file N1` with content that does not hash to N1. Fossil leaves no phantom
for U and starts no further round; the prefix is gone with the round.

## 4. Impact

Observable. A client written from §10.1 keeps the prefix, including the
mismatched artifact or a phantom for it, and goes on to the next round
where Fossil stops with nothing kept. The two disagree on what the
repository holds after the exchange.

## 5. Correction

§10.1:

```diff
-   A handler-reported content or integrity error warns, preserves and
-   commits the processed prefix, and follows ordinary continuation.
-   Explicit fail-fast paths roll back transaction-visible current-round
-   state and start no further round.
+   A handler-reported content error that leaves the stored artifacts
+   consistent, such as a malformed card or a refused private file,
+   warns, preserves and commits the processed prefix, and follows
+   ordinary continuation.  An integrity error, a received file or cfile
+   whose reconstructed content does not match its HASH, is an explicit
+   fail-fast path: the client MUST NOT commit the round that received
+   it.  Explicit fail-fast paths roll back transaction-visible current-
+   round state, the processed prefix included, and start no further
+   round.  (Fossil stores the content and detects the mismatch when the
+   round's transaction commits, where it aborts; this paragraph is
+   informative.)
```

## 6. Rationale

Keeps the prefix rule for the errors it was written for and names the one
error that cannot be committed without storing an artifact whose name is
not its hash. Fossil's result is made normative as a fail-fast path,
which the paragraph already defines, rather than as the abort that
produces it; a client that checks the hash on receipt and discards the
round reaches the same repository state without aborting.

## 7. Verification

- Source: Fossil 2.29 `src/xfer.c:134-222`; `src/content.c:643`, `:945`;
  `src/verify.c:40-52`, `:62-75`, `:92-104`.
- Run: go-libfossil cases `sync.10.1.integrity-error-prefix` and
  `sync.10.1.integrity-error-continues` against Fossil 2.28, which record
  no phantom and no further round.
