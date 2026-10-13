# Erratum — draft-fossil-sync-protocol-01 §9.4 / §9.5

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-01` |
| **Section** | 9.4 — Commit-Lock Pragmas (the server's reply to ci-lock); 9.5 — Private-Content and Discovery Pragmas (the avoid-delta-manifests paragraph) |
| **Type** | Technical (§9.4 omits a reply the server sends to every ci-lock; §9.5 gives the client the wrong behavior) |
| **Status** | Applied in `draft-fossil-sync-protocol-01` (2026-10-13) |
| **Reported** | 2026-10-13 |
| **Submitter** | Dan Mestas |
| **Found via** | Reading Fossil 2.29's `page_xfer()` ci-lock handler; confirmed by posting ci-lock requests to Fossil 2.28 |

## 1. Summary

§9.4 says that after `pragma ci-lock` the server emits `ci-lock-fail` on a
conflict and otherwise "emits no lock result" and "no positive lock
acknowledgement". Fossil's ci-lock handler also emits
`pragma avoid-delta-manifests` whenever the server's
`forbid-delta-manifests` setting is on, which is the default since Fossil
2.29, whether or not the lock was granted. §9.4 does not mention it. The pragma is listed
in §9 and described in the last paragraph of §9.5, but that description
is wrong: it has the client change which artifacts it sends during the
sync. In Fossil the pragma changes no transfer behavior. It makes the
client's pending commit write a baseline manifest instead of a delta
manifest.

## 2. Location

§9.4, first paragraph, from "If its stored CLIENT-ID differs" to "a
matching live row prevents a new row", and second paragraph, "It emits no
positive lock acknowledgement."; §9.5, the paragraph beginning
"avoid-delta-manifests is a server-to-client control pragma".

## 3. The reference behavior

Verified against Fossil 2.29 source; behavior run on Fossil 2.28.

- Server: the `pragma ci-lock` handler (`src/xfer.c:1781-1834`), after
  the conflict scan and the lock write, runs
  `if( db_get_boolean("forbid-delta-manifests",1) ){ @ pragma avoid-delta-manifests }`
  (`:1831-1833`). The test is outside the `seenFault` branch, so the
  pragma follows a `ci-lock-fail` as well as a granted or kept lock. It
  is emitted only by a ci-lock that passes the handler's guard (exactly
  four tokens, a hash-name third token, `:1781-1783`).
- Setting: `forbid-delta-manifests` is a boolean, default on in Fossil
  2.29 (`src/db.c:4899-4906`) and default off in Fossil 2.28
  (`src/db.c:4898`), and is a project setting that `fossil
  configuration` can sync (`src/configure.c:146`).
- Client: the reply handler sets the process flag
  `g.bAvoidDeltaManifests = 1` (`src/xfer.c:2864-2871`) and does nothing
  else. The only reader is `commit` (`src/checkin.c:2788-2792`), which
  then sets `forceBaseline`, so the check-in is written as a baseline
  manifest even with `--delta`. The flag is not stored in the repository
  and is not consulted by the transfer code.
- When it is sent: `commit` runs its pre-commit autosync with
  `SYNC_CKIN_LOCK` for a non-private commit on an existing check-out
  without `--allow-fork` or `--force` (`src/checkin.c:2758-2768`, and
  again at `:3033-3040`); the client sends `pragma ci-lock` in the first
  request of that sync only (`src/xfer.c:2184-2189`, `:2382-2388`). A
  plain `sync`, `push` or `pull` sends no ci-lock and so never receives
  the pragma.
- Run (Fossil 2.28, `fossil http` with an uncompressed `/xfer` body,
  `forbid-delta-manifests` set on): a ci-lock with no live lock is
  answered with `pragma avoid-delta-manifests` alone; a ci-lock from a
  second client ID with `pragma ci-lock-fail LOGIN MTIME` then `pragma
  avoid-delta-manifests`. With the setting off, or unset on 2.28, whose
  default is off, the first reply is empty and the second carries only
  the `ci-lock-fail`. A 2.29 build sends the pragma with the setting
  unset.

## 4. Impact

Observable. A server written from §9.4 omits a card Fossil sends on every
commit-time autosync. A client written from §9.5 alters its transfer and
still writes a delta manifest that a `forbid-delta-manifests` server asked
it to avoid.

## 5. Correction

§9.4, first paragraph:

```diff
    refresh or replacement.  If its stored CLIENT-ID differs from the
    request CLIENT-ID, the server emits pragma ci-lock-fail LOGIN MTIME;
    if it matches, it emits no lock result.  In either case, a matching
-   live row prevents a new row.
+   live row prevents a new row.  After this processing, whether or not
+   a lock was written, the server emits pragma avoid-delta-manifests
+   when its forbid-delta-manifests setting (default on) is true; when
+   ci-lock-fail is emitted, avoid-delta-manifests follows it.  A ci-
+   lock that fails the handler's argument guard emits neither.
```

§9.4, second paragraph:

```diff
-   the current server login value, CLIENT-ID, and current MTIME.  It
-   emits no positive lock acknowledgement.  The transfer dispatcher
+   the current server login value, CLIENT-ID, and current MTIME.  It
+   emits no positive lock acknowledgement; the avoid-delta-manifests
+   pragma above is not one.  The transfer dispatcher
```

§9.5, the avoid-delta-manifests paragraph:

```diff
-   avoid-delta-manifests is a server-to-client control pragma.  Upon
-   receipt, the client removes manifest-delta candidates from any not-
-   yet-dispatched seeded request and sends full manifests for the
-   remainder of the current sync session; it does not affect non-
-   manifest artifacts, already-dispatched data, or persisted repository
-   state.  The flag resets when the session ends.
+   avoid-delta-manifests is a server-to-client pragma with no arguments,
+   sent only in reply to ci-lock (Section 9.4).  It asks the client not
+   to create delta manifests (manifests with a B card, [FossilArtifact])
+   for the commit that requested the lock.  Upon receipt, a client that
+   is about to commit SHOULD write that check-in as a baseline manifest,
+   even if a delta manifest was requested.  The pragma does not change
+   which artifacts the client sends or requests, does not affect
+   artifacts already created, and is not stored in the repository.
```

## 6. Rationale

Puts the reply where a server implementer reads the ci-lock handler, with
the setting that gates it and its position after ci-lock-fail, and
replaces the §9.5 behavior with the one Fossil has: a hint to the commit
that triggered the lock, not a transfer rule. SHOULD rather than MUST,
because a client that never writes delta manifests has nothing to do.

## 7. Verification

- Source: Fossil 2.29 `src/xfer.c:1781-1834`, `:1831-1833`,
  `:2184-2189`, `:2382-2388`, `:2864-2871`; `src/checkin.c:2758-2768`,
  `:2788-2792`, `:3033-3040`; `src/db.c:4899-4906`;
  `src/configure.c:146`; `src/main.c:305`.
- Run: Fossil 2.28 `fossil http` against a scratch repository, with
  `forbid-delta-manifests` set on, set off, and unset (off on 2.28),
  giving the replies in Section 3; a 2.29 build with the setting unset
  sends the pragma. The "default on" of the correction is Fossil 2.29's.
