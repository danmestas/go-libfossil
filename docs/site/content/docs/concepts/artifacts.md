---
title: Artifacts
weight: 20
---

Everything a Fossil repository records is an **artifact**: a run of bytes, named by its own hash, that is never changed once written. File contents are artifacts. So are check-ins, tags, wiki pages and tickets, which are written as text **cards**. The format is `draft-fossil-artifact-format-00`.

## Names

An artifact's name is the hash of its exact stored bytes, with nothing added or stripped (§3.1). A 40-digit name is SHA-1; a 64-digit name is SHA3-256. The length alone tells which (§3.2). A signed artifact's name covers its signature wrapper too, so signing a check-in changes its name.

The repository's **hash policy** decides which algorithm names new content (§7.1):

| Policy | Names new content with |
|---|---|
| `sha1`, `auto` | SHA-1 |
| `sha3`, `sha3-only`, `shun-sha1` | SHA3-256 |

A repository with no policy set uses `sha3` unless it holds SHA-1 names and no SHA3 ones, in which case it uses `auto` (§7.2). A new repository is `sha3`. Names already assigned never change (§7.6). `Repo.SetHashPolicy(name)` sets the policy as `fossil hash-policy` does.

## Structural artifacts and cards

A structural artifact is lines of cards: a capital letter, then space-separated arguments, with the cards in sorted order and a `Z` card last (§4). There are eight types, and each allows a fixed set of cards (§5.1, §5.2):

| Type | External name | Allowed cards |
|---|---|---|
| MANIFEST | `checkin` | B C D F N P Q R T U Z |
| CLUSTER | `cluster` | M Z |
| CONTROL | `tag` | D T U Z |
| WIKI | `wiki` | C D L N P U W Z |
| TICKET | `ticket` | D J K U Z |
| ATTACHMENT | `attachment` | A C D N U Z |
| EVENT | `technote` | C D E N P T U W Z |
| FORUM | `forumpost` | D G H I N P U W Z |

Any other byte string is plain content, such as a file's bytes. A parser rejects a structural artifact with a card its type does not allow, or without one its type requires.

[Sub-manifests](/docs/concepts/sub-manifests/) add an `S` card and a ninth type.

## A check-in manifest

This is a check-in made with `libfossil repo ci`:

```
C unsigned
D 2026-10-23T20:45:21.324
F docs/x.md a09fd4d0069e85b78c5234233ffb67179d30e19c2ec94629d224257a920c1234
F src/a.c 7eae391285a140af77b34475a35c4790095acb1f0f5e6ec7e10e962b2405de07
F src/vendor/v.c bedef797cd3184c3c7e620c5c231bbdd523f52ab19ca6301d037c7f2a3f99780
P 2afc4615dadbd7743716f8037223578d7d07a79ebc2cbfd5ba1b6028f133c7f2
R 43fe2394e6714e382e2f6563c13cf020
U dmestas
Z 56b668a3573378eb3e2387902909b385
```

- `C`: the check-in comment.
- `D`: the date, UTC.
- `F`: one per file: path and content hash, then optional permissions and an old name for a rename.
- `P`: the parent check-ins. The first is the primary parent; the rest are merge parents.
- `R`: an MD5 over every file's name, size and content (§6.3). A commit reads its manifest back and checks it. Over a sparse store there is no `R` card.
- `U`: the user.
- `Z`: the MD5 of every byte before it (§6.2).

The first check-in on a branch also carries `T` cards, such as `T *branch * trunk` and `T *sym-trunk *`. A **delta manifest** carries a `B` card naming a baseline manifest, and its `F` cards record only the differences from it (§8).

The name of this check-in is the SHA3-256 of exactly these bytes: `libfossil repo hash --sha3` over the output of `repo cat` prints `7507cc8c3e...`, the name the timeline shows.

## Deltas

Content is stored in the `blob` table, zlib-compressed behind a 4-byte size prefix. A blob can also be stored as a **delta** against another blob, with the pair recorded in the `delta` table (`rid`, `srcid`). Reading it applies the chain of deltas back to a full blob, and the result must hash to the blob's name.

A delta is a header with the target length, a run of copy and insert segments, and a trailer with a checksum of the result (`draft-fossil-delta-format-00` §3). Integers are written in base 64. A delta does not name its source; the storage layer records that (§1).

As in fossil, deltas run backwards: a new version is stored whole and its predecessor is rewritten as a delta against it, so the newest version of a file is always one read away.

## Where the code is

- `internal/deck`: parse and marshal cards, the `Z` card, signature-wrapper stripping.
- `internal/manifest`: build check-ins, compute the `R` card, crosslink.
- `internal/delta`: create and apply deltas (`Create`, `Apply`).
- `internal/hash`: SHA-1 and SHA3-256 names and the hash policy.
- `internal/blob` and `internal/content`: compressed storage, delta chains, verification.
