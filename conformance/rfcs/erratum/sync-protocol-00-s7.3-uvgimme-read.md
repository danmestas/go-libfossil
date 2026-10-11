# Erratum — draft-fossil-sync-protocol-00 §7.3

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-00` |
| **Section** | 7.3 — Unversioned Files (server handling of `uvgimme`); cross-references §3.3 and §9.2 |
| **Type** | Technical (the requirement changes: a permission condition is added) |
| **Status** | Proposed — tracks a fossil change adding the same Read check to `page_xfer()` |
| **Reported** | 2026-10-11 |
| **Submitter** | Dan Mestas |
| **Found via** | go-libfossil's sync conformance suite; reproduced on fossil 2.28 [52445a27f1] and 2.29 trunk [b0fad57ecb] |

## 1. Summary

§7.3 tells a server to answer every syntactically valid `uvgimme NAME` with
the file, and says the request "has no received-write-capability
requirement". It sets no Read condition either. That copies the reference
implementation, whose `uvgimme` handler is the only way to read unversioned
content that does not check Read: the `/uv` and `/uvlist` pages, the
unversioned catalog (§9.2 already says "with Read permission"), and `gimme`
for ordinary artifacts all do. As written, a server that follows the draft
sends any unversioned file to a requester with no capabilities at all, on a
repository that denies anonymous read, provided the requester knows or
guesses the file's name.

The correction adds the Read condition, and specifies the refusal the way
§3.3 already specifies it for `gimme`: emit neither content nor error.

## 2. Location

- §7.3, the `uvgimme` paragraph (~lines 1299-1310): "On a syntactically
  valid server-received uvgimme NAME, the server MUST look up NAME. ...
  This request reads server state and has no received-write-capability
  requirement."

For comparison, unchanged:

- §9.2 (~line 1870): "On a mismatch with Read permission, the server sends
  ... the full catalog."
- §3.3 (~lines 532-535): "Receiving gimme HASH records peer-lacks. If
  content is held and direction and policy permit, send it now or
  subsequently. If direction forbids sending, emit neither content nor
  error and do not defer."

## 3. The hazard

A legitimate client only sends `uvgimme` for names it learned from the
catalog, and §9.2 already withholds the catalog from a requester without
Read. So the missing condition never shows up in an ordinary exchange: a
stock client without Read gets no catalog and never asks. It shows up only
when a request names a file directly. On fossil 2.28 and on 2.29 trunk, with
every capability removed from `nobody`, `anonymous`, `reader` and
`developer`, this request

```
pragma client-version 22800 20260311 113146
uvgimme secret.txt
```

is answered with the full `uvfile secret.txt ... 33 0` card and the file's
content, while `pragma uv-hash`, a `gimme` for the same content, and
`GET /uv/secret.txt` all refuse the same requester.

## 4. Impact

Wire-visible, and a confidentiality issue. A server implementing §7.3 as
written discloses unversioned content (release files, download pages,
anything stored with `fossil uv add`) of a private repository to anyone
who can reach its sync URL. Public repositories are unaffected: what
`uvgimme` returns there, `/uv/` already serves.

## 5. Correction

In §7.3, replace the first sentence about server-received `uvgimme` and the
paragraph's last sentence:

```diff
-   changes invalidate cached catalog hash.  On a syntactically valid
-   server-received uvgimme NAME, the server MUST look up NAME.  If NAME
-   is absent, it emits no card.
+   changes invalidate cached catalog hash.  On a syntactically valid
+   server-received uvgimme NAME from a requester with Read permission,
+   the server MUST look up NAME.  Without Read permission, the server
+   emits neither content nor error for the card, as for a gimme whose
+   direction forbids sending (Section 3.3); the catalog is withheld
+   from the same requester (Section 9.2).  For this check, Clone
+   permission implies Read.  If NAME is absent, it emits no card.
```

```diff
-   SIZE 0, and FLAGS 1.  This request reads server state and has no
-   received-write-capability requirement.
+   SIZE 0, and FLAGS 1.  This request reads server state: it requires
+   Read permission, as the catalog does, and has no UV write
+   requirement.
```

## 6. Rationale

- **One rule for reading unversioned content.** Every other read path, in
  the draft (§9.2) and in the reference implementation (`/uv`, `/uvlist`,
  the catalog), requires Read. `uvgimme` is the request that fetches the
  content the catalog describes, so it takes the same condition.
- **No change to ordinary exchanges.** A client without Read never receives
  the catalog and so never sends `uvgimme`; a client with Read is answered
  exactly as before.
- **The refusal reuses an existing rule.** "Emit neither content nor error"
  is §3.3's wording for a `gimme` the server will not answer, so the
  refusal stays silent and adds no new wire behavior.
- **Clone implies Read** so that `fossil clone -u` keeps working for a
  principal that may clone but has no separate Read. The reference
  implementation's trunk already sets Read whenever Clone is held
  (`if( g.perm.Clone ) g.perm.Read = 1;` in `page_xfer()`).

## 7. Application and verification

- **Reference implementation.** A three-line patch to the `uvgimme` branch
  of `page_xfer()` (`src/xfer.c`) answers only when `g.perm.Read` is set.
  Built from 2.29 trunk [b0fad57ecb] and tested against nine cases:
  - a bare `uvgimme` from a requester with no capabilities is refused (the
    unpatched build sends the file);
  - Read-only and Clone-only requesters are still answered;
  - `fossil clone -u` for the default `nobody`, a Clone-only `nobody` and
    an admin, and `fossil uv sync` for an admin and an anonymous reader,
    behave identically with and without the patch.

  The patch also applies cleanly to 2.28, where a Clone-only principal
  already receives no catalog.
- **go-libfossil.** v0.11.1 (GHSA-mvx3-6793-8x4r) answers `uvgimme` only
  to a requester with Read, with Clone implying Read. Its sync conformance
  suite carries the requirement.

---

*Prepared against `draft-fossil-sync-protocol-00`. The section is faithful
to the reference implementation as it stood; the correction follows the
implementation's intended behavior once the matching fix lands. The RFC
remains the oracle; the reference implementation is used only to confirm
the observable behavior, per the interop-verification policy.*
