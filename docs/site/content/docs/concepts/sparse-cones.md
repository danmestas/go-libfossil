---
title: Sparse cones
weight: 60
aliases: ["/docs/sparse-and-horizons/"]
---

A repository does not have to hold everything. A **cone** limits it by path. A [horizon](/docs/concepts/horizons/) limits it by age, and is carried as a member of a cone. Both leave the history intact, and the blobs they skip can be fetched later.

Cones follow `draft-fossil-sparse-cone-00`. They are implemented in step with zit.

## Cones

A cone is a set of repository paths. Each member is a directory subtree or a single file, and each is either an include or an exclude. Members are literal prefixes, not patterns (§3.4).

The longest member that covers a path decides it. An exclude carves a part out of an include, and a deeper include re-includes part of an exclude (§3.3):

```
include src
exclude src/vendor
include src/vendor/keep
```

`src/a.c` is covered. `src/vendor/v.c` is not. `src/vendor/keep/k.c` is covered again.

A cone must have at least one include, at most 255 members, and no exclude that no include covers. Order does not matter; members are sorted and reduced to the smallest set with the same coverage (§3.2).

### Checkout cones and store cones

A **checkout cone** limits the working directory (§4). The repository still holds every file. A sparse checkout writes, scans and tracks only the covered files. A commit carries the uncovered files from its parent unchanged, so it is the same check-in a dense checkout would make. Adding, removing, renaming or reverting an uncovered path fails with `ErrOutsideCone`.

A **store cone** limits the repository itself (§5). Blobs outside the cone are not fetched, and can be dropped by compaction. A checkout opens over a sparse store only if its own cone stays inside the store's.

Manifests, [sub-manifest](/docs/concepts/sub-manifests/) nodes and control artifacts are always held, so the whole history stays readable. Only file content is elided.

### Elided is not damaged

An artifact the store lacks is classified one of three ways (§5.2):

- **transit**: not here yet, and sync will ask for it.
- **shunned**: deliberately refused.
- **elided**: the cone says the store need not hold it.

Only elided is by design. Sync never requests an elided artifact, whether the store is the client or the server, and `verify` reports a store that is complete within its cone as OK. Elision is computed from the cone and never stored, so changing the cone reclassifies. Losing the cone, or one this version cannot read, turns every elided artifact back into a transit one and the store heals toward dense.

## Library usage

### Build a cone

```go
cone, err := libfossil.NewCone(
    []string{"src", "docs"},       // include
    []string{"src/vendor"},        // exclude
)
if err != nil {
    log.Fatal(err)
}
fmt.Println(cone.Covers("src/main.go"))           // true
covered, member := cone.Decide("src/vendor/x.c")  // false, "src/vendor"
```

A `Cone` is immutable. `With`, `Without` and `WithHorizon` return new ones. `Include`, `Exclude`, `Digest`, `String` and `Horizon` read it.

### Sparse checkout

```go
repo, err := libfossil.Open("repo.fossil")
if err != nil {
    log.Fatal(err)
}
defer repo.Close()

co, err := repo.CreateCheckout("work", libfossil.CheckoutCreateOpts{Cone: cone})
if err != nil {
    log.Fatal(err)
}
defer co.Close()

// Widen, narrow, or pass nil to make the checkout dense.
change, err := co.SetCone(wider)
if err != nil {
    log.Fatal(err)
}
fmt.Println(change.Materialized, change.Dematerialized)
```

`SetCone` writes the files the new cone adds and removes the ones it drops from the working directory. The repository keeps them. It refuses, changing nothing, when a file leaving the cone has local changes or a file entering it would overwrite an untracked one. A cone that carries a horizon fails with `ErrHorizonCheckout`.

Over a sparse store, widening needs content the store may not hold. `SetConeContext` takes a `RequiredContentOpts{Peer: transport}` and fetches it. With no peer the widening is refused and nothing changes.

### Clone into a sparse store

```go
t := libfossil.NewHTTPTransport("https://example.com/repo")
repo, res, err := libfossil.Clone(ctx, "repo.fossil", t, libfossil.CloneOpts{
    StoreCone: cone,
})
if err != nil {
    log.Fatal(err)
}
defer repo.Close()
fmt.Println(res.Elided, res.ConeServed)
```

The result counts:

- `Elided`: artifacts left absent under the cone.
- `ConeServed`: replies where the server acknowledged applying the cone (`pragma cone-served`, §6.3). The other rounds were served densely and filtered locally.

Both are zero for a dense clone. `SyncResult` has the same fields. A server that will not serve a sparse clone refuses it. A cone with a horizon clones a horizon store; see [Horizons](/docs/concepts/horizons/).

### Set, widen and drop on an existing store

```go
// Set or replace, without a peer. Narrowing deletes nothing.
err = repo.SetStoreCone(next)

// Read it.
cur, err := repo.StoreCone()

// Widen, and fetch what that newly covers.
sr, err := repo.SetStoreConeContext(ctx, wider, libfossil.RequiredContentOpts{
    Peer: t,
})

// Drop the cone. Fetch everything it elided.
sr, err = repo.SetStoreConeContext(ctx, nil, libfossil.RequiredContentOpts{Peer: t})

// Drop held blobs the cone elides. See Limits.
n, err := repo.CompactStore(ctx)
```

`SetStoreCone` moves no content. Widening makes the newly covered elided artifacts wanted again, and the next pull fetches them. `SetStoreConeContext` does the pull itself through an ordinary cone-scoped sync. With artifacts to fetch and no `Peer`, it refuses, names the count, and leaves the cone unchanged. If the transfer fails after the cone is replaced, the wider policy stays and the next pull heals it. While a store cone is set, a commit carries no R card. `CompactStore` on a dense store fails with `ErrNoStoreCone`.

### List a check-in's files in a cone

```go
files, err := repo.ListFilesIn(rid, cone)
```

`ListFilesIn` returns what `ListFiles` returns, narrowed to the files the cone covers. A nil cone lists everything. On a tree manifest it skips every sub-manifest the cone cannot reach, so it costs the cone plus the path down to it, not the whole tree (§4.5). Over a sparse store it is the listing to use: `ListFiles` walks the whole tree and fails on the first elided node, while `ListFilesIn` never asks for one outside its cone. A cone that reaches a node the store does not hold still fails.

## Command line

### Open a sparse checkout

```
libfossil -R repo.fossil repo open [DIR] --cone src --cone docs --cone-not src/vendor
```

- `--cone=CONE,...`: make a sparse checkout; materialize and track only this path's subtree (repeatable).
- `--cone-not=CONE-NOT,...`: exclude this path's subtree from an enclosing `--cone` (repeatable).

### Edit the checkout cone

All of these take `-d, --dir` for the checkout directory (default `.`).

```
libfossil repo cone show
libfossil repo cone add src/extra
libfossil repo cone add --not src/gen
libfossil repo cone remove src/extra
libfossil repo cone set src docs --not src/vendor
libfossil repo cone dense
libfossil repo cone test src/a.c src/vendor/v.c
```

- `show`: list the cone's members ("dense" when there is none).
- `add <paths>`: widen the cone, or with `--not` carve paths out of it.
- `remove <paths>`: remove members of either polarity.
- `set <paths>`: replace the cone. `--not` takes the exclusions.
- `dense`: drop the cone and materialize every file.
- `test <paths>`: classify paths against the cone and name the deciding member. It exits non-zero when any path is elided, so it works as a predicate. `-q` prints nothing and reports through the status only.

`add`, `remove`, `set` and `dense` can need content the store lacks. They take `--remote` (default: the saved last-sync-url), `--sync-user`, `--sync-pass` (never saved), `--sync-private` and `--timeout` (default 10m; 0 disables it).

### Clone a sparse store

```
libfossil repo clone URL repo.fossil --cone src --cone-not src/vendor
```

- `--cone=CONE,...`: clone a sparse store: fetch only this path's subtree (repeatable).
- `--cone-not=CONE-NOT,...`: carve this path out of a `--cone` (repeatable).

`--horizon`, `--leaves`, `--structural` and `--seed-key` combine with these; see [Horizons](/docs/concepts/horizons/).

### Change the store cone

```
libfossil -R repo.fossil repo cone store                          # show it
libfossil -R repo.fossil repo cone store src docs --not src/vendor
libfossil -R repo.fossil repo cone store --dense
libfossil -R repo.fossil repo cone compact
```

- With no paths, `store` shows the store cone. With paths, it replaces it and fetches what the widening covers. `--not` takes the exclusions.
- `--dense`: drop the store cone, fetching everything it elided.
- `compact`: drop held blobs the store cone elides (irreversible locally).

`store` takes the same `--remote`, `--sync-user`, `--sync-pass`, `--sync-private` and `--timeout` flags as above. Its `--horizon`, `--leaves`, `--structural` and `--no-horizon` flags are described under [Horizons](/docs/concepts/horizons/).

## Interoperability

**zit.** The cone format matches zit's byte for byte, so a cone's digest is the same in both. Sparse sync works in both directions against zit's `horizon-rfc` revision. An implementation that predates horizons refuses a cone that carries one.

**Stock fossil 2.28.** A sparse store must not be served as if it were dense, because the clone would be silently incomplete. A store held sparsely is served only when its `partial-mirror` setting is on (`repo config set partial-mirror on`, `SettingPartialMirror`); otherwise it refuses. The refusal puts the push card ahead of the error so fossil 2.28 aborts instead of carrying on. Fossil cannot read [sub-manifest](/docs/concepts/sub-manifests/) check-ins either, so a repository that uses them is for libfossil and zit peers only.

## Limits

- **Compaction is irreversible locally.** `CompactStore` and `repo cone compact` delete held blobs (§5.4). Widening later fetches them again, which needs a peer that still has them. Structural artifacts, sub-manifest nodes, unsent or private content and the delta bases of retained artifacts are kept.
- **A sparse store lacks file content.** Commands that read an elided blob need a peer to fetch it, or fail.
- **A cone is not an authorization boundary** (§6.5). It limits what a store holds and asks for, not what a peer may read.
