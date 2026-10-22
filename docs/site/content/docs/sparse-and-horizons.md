---
title: Sparse cones and horizons
weight: 35
---

A repository does not have to hold everything. A **cone** limits it by path. A **horizon** limits it by age. Both leave the history intact and the blobs they skip can be fetched later.

The behaviour follows two drafts: `draft-fossil-sparse-cone-00` (cones) and `draft-fossil-horizon-00` (horizons). Both are implemented in step with zit.

## Cones

A cone is a set of repository paths. Each member is a directory subtree or a single file, and each is either an include or an exclude. Members are literal prefixes, not patterns.

The longest member that covers a path decides it. An exclude carves a part out of an include, and a deeper include re-includes part of an exclude:

```
include src
exclude src/vendor
include src/vendor/keep
```

`src/a.c` is covered. `src/vendor/v.c` is not. `src/vendor/keep/k.c` is covered again.

A cone must have at least one include, at most 255 members, and no exclude that no include covers. Order does not matter; members are sorted and reduced to the smallest set with the same coverage.

### Checkout cones and store cones

A **checkout cone** limits the working directory. The repository still holds every file. A sparse checkout writes, scans and tracks only the covered files. A commit carries the uncovered files from its parent unchanged, so it is the same check-in a dense checkout would make. Adding, removing, renaming or reverting an uncovered path fails with `ErrOutsideCone`.

A **store cone** limits the repository itself. Blobs outside the cone are not fetched, and can be dropped by compaction. A checkout opens over a sparse store only if its own cone stays inside the store's.

Manifests, sub-manifest nodes and control artifacts are always held, so the whole history stays readable. Only file content is elided.

### Elided is not damaged

An artifact the store lacks is classified one of three ways:

- **transit**: not here yet, and sync will ask for it.
- **shunned**: deliberately refused.
- **elided**: the cone says the store need not hold it.

Only elided is by design. Sync never requests an elided artifact, whether the store is the client or the server, and `verify` reports a store that is complete within its cone as OK. Elision is computed from the cone and never stored, so changing the cone reclassifies. Losing the cone, or one this version cannot read, turns every elided artifact back into a transit one and the store heals toward dense.

## Horizons

A horizon cuts a store by time. It names a check-in **C**, a rule, and a depth. The check-ins the rule keeps are the **held set**. A blob is elided when no check-in in the held set names it. Structure (manifests, tags, control artifacts) is never elided.

The held set is computed from the check-in graph the repository holds. It is stored nowhere.

### Rules

- **descendants** (the default): C and every check-in descended from it, through any parent, merges included.
- **leaves**: the descendants, plus, for every childless check-in that does not descend from C, every descendant of each check-in where its ancestry meets C's.

A check-in is childless when no other check-in names it as a parent, open or closed. A branch tip merged into another branch has a child, so it is not childless. Under the leaves rule, any two childless check-ins have their merge base in the held set, so merging branches that are still being extended never crosses the horizon. Both rules give a set that includes every child of every check-in in it.

The leaves rule has a cost you should check before using it. An old branch that was never merged forks from C's ancestry near the root, and the rule then holds every descendant of that fork. On fossil-scm.org's repository it held 20,907 of 21,008 check-ins at every depth tested. Use it when the repository keeps few old unmerged branches.

The descendants rule saves much more. On the same repository, a structural horizon placed 10, 100, 426 and 1000 check-ins back cut expanded transfer by 97, 86, 56 and 41 percent. A content horizon keeps all structure and elides only blobs; a structural horizon also leaves out the manifests below C.

### Depth

- **content** (available): which blobs are held. Every check-in's structure is still held.
- **structural** (`Horizon.Structural`, `--structural`): also cuts the check-ins below C. It needs a seed, a control artifact the origin mints so a clone can derive tag and branch state without C's ancestry.

### Moving the horizon

Moving C earlier and pulling fetches what the deeper horizon keeps. Moving it later keeps what you hold until you compact. A horizon store cannot be a checkout's cone: a horizon governs what the repository holds, not which paths a checkout writes.

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

A `Cone` is immutable. `With`, `Without` and `WithHorizon` return new ones. `Include`, `Exclude`, `Digest` and `String` read it.

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

### Horizon store cone

```go
h := libfossil.Horizon{
    Checkin: "9f2c...",                   // full 40 or 64 digit hash
    Rule:    libfossil.HorizonDescendants, // or HorizonLeaves
}

// Horizon only: every path, cut by time.
hc, err := libfossil.NewHorizonCone(nil, nil, h)

// Horizon plus paths.
hc, err = libfossil.NewHorizonCone([]string{"src"}, nil, h)

got, ok := hc.Horizon()              // read it back
noHorizon, err := hc.WithHorizon(nil) // drop it, keep the paths
```

`Checkin` must be a full hash, and it is lower-cased. Dropping the horizon of a horizon-only cone is an error, since nothing would be left; use `SetStoreCone(nil)` to make the store dense.

### Clone into a sparse or horizon store

```go
t := libfossil.NewHTTPTransport("https://example.com/repo")
repo, res, err := libfossil.Clone(ctx, "repo.fossil", t, libfossil.CloneOpts{
    StoreCone: hc,
})
if err != nil {
    log.Fatal(err)
}
defer repo.Close()
fmt.Println(res.Elided, res.ConeServed, res.HorizonServed)
```

The result counts:

- `Elided`: artifacts left absent under the cone.
- `ConeServed`: replies where the server acknowledged applying the cone (`pragma cone-served`).
- `HorizonServed`: replies where the server cut at the horizon (`pragma horizon-served`). The other rounds were classified locally.

All three are zero for a dense clone. `SyncResult` has the same three fields. A server that will not serve a sparse clone refuses it.

### Set, deepen and drop on an existing store

```go
// Set or replace, without a peer. Narrowing deletes nothing.
err = repo.SetStoreCone(hc)

// Read it.
cur, err := repo.StoreCone()

// Widen or deepen, and fetch what that newly keeps.
sr, err := repo.SetStoreConeContext(ctx, deeper, libfossil.RequiredContentOpts{
    Peer: t,
})

// Drop the cone. Fetch everything it elided.
sr, err = repo.SetStoreConeContext(ctx, nil, libfossil.RequiredContentOpts{Peer: t})

// Drop held blobs the cone elides. See Limits.
n, err := repo.CompactStore(ctx)
```

`SetStoreCone` moves no content. Widening makes the newly covered elided artifacts wanted again, and the next pull fetches them. `SetStoreConeContext` does the pull itself through an ordinary cone-scoped sync. With artifacts to fetch and no `Peer`, it refuses, names the count, and leaves the cone unchanged. If the transfer fails after the cone is replaced, the wider policy stays and the next pull heals it. While a cone is set, a commit carries no R card.

### Deepen, merge across, and verify the seed

```go
// Move the horizon down far enough to reach a check-in, which may lie
// below it. C moves to that check-in when it is an ancestor of C, else to
// a common ancestor of the two, asked of the origin when the store cannot
// see one. The rule, the depth and the path members stay.
sr, err = repo.DeepenHorizon(ctx, target, libfossil.RequiredContentOpts{Peer: t})

// Merge. With a Peer, a merge whose base lies below a structural horizon
// asks the origin for the base and deepens to it first. Without one it
// fails with an error matching libfossil.ErrBelowHorizon.
mr, err := co.MergeContext(ctx, libfossil.CheckoutMergeOpts{Version: rid},
    libfossil.RequiredContentOpts{Peer: t})

// Check the seed against the history it summarizes. This fetches every
// manifest below C and leaves a content-horizon store.
v, err := repo.VerifySeed(ctx, libfossil.RequiredContentOpts{Peer: t})
```

Deepening a structural store discards its seed before it fetches the new one, so the old seed cannot shadow the new one. If the transfer fails, the old horizon and seed are put back.

A merge never runs against a base the store does not hold. A bisection whose good end lies below the horizon is refused the same way, naming the horizon and the deepen.

An origin checks each pushed check-in that creates a branch: if the branch it cancels is not the one this origin derives for the parent, the clone's seed was stale, and the whole push is refused with `stale seed: ...`. Pull, push a control artifact that cancels the parent's real branch tag on the check-in, and push again.

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

### Clone a sparse or horizon store

```
libfossil repo clone URL repo.fossil --cone src --cone-not src/vendor
libfossil repo clone URL repo.fossil --horizon <full hash>
libfossil repo clone URL repo.fossil --horizon <full hash> --leaves --cone src
```

- `--horizon=STRING`: clone a content-horizon store: elide blobs below this check-in, given as its full 40 or 64 digit hash.
- `--leaves`: with `--horizon`, also hold every open leaf's ancestry back to the check-in.
- `--cone=CONE,...`: clone a sparse store: fetch only this path's subtree (repeatable).
- `--cone-not=CONE-NOT,...`: carve this path out of a `--cone` (repeatable).

The clone needs a full hash because nothing is local yet to resolve a name against.

### Change the store cone

```
libfossil -R repo.fossil repo cone store                          # show it
libfossil -R repo.fossil repo cone store src docs --not src/vendor
libfossil -R repo.fossil repo cone store --horizon <check-in>
libfossil -R repo.fossil repo cone store --horizon <check-in> --leaves
libfossil -R repo.fossil repo cone store --no-horizon
libfossil -R repo.fossil repo cone store --dense
libfossil -R repo.fossil repo cone compact
```

- With no paths, `store` shows the store cone. With paths, it replaces it and fetches what the widening covers.
- `--horizon=STRING`: elide blobs below this check-in (name, branch or hash prefix), fetching what a deeper horizon keeps. Unlike `clone`, a local name is fine.
- `--leaves`: with `--horizon`, also hold every open leaf's ancestry back to the check-in.
- `--no-horizon`: drop the horizon, keeping the path members, and fetch what it elided.
- `--dense`: drop the store cone, fetching everything it elided.
- `compact`: drop held blobs the store cone elides (irreversible locally).

```
libfossil -R repo.fossil repo cone deepen <check-in>   # full hash, or a name
libfossil -R repo.fossil repo merge <version> -d DIR
libfossil -R repo.fossil repo verify --seed
```

- `cone deepen`: move the horizon down to reach the check-in, fetching what the deeper horizon keeps.
- `merge`: a merge whose base lies below a structural horizon asks the remote for the base and deepens to it. An ordinary merge contacts no peer.
- `verify --seed`: verify the structural seed by fetching the history below the horizon. The store stays a content-horizon store.

These take the same `--remote`, `--sync-user`, `--sync-pass`, `--sync-private` and `--timeout` flags.

`store` takes the same `--remote`, `--sync-user`, `--sync-pass`, `--sync-private` and `--timeout` flags as above.

## Interoperability

**zit.** The cone and horizon formats match zit's byte for byte, so a cone's digest is the same in both. Sparse and horizon sync work in both directions against zit's `horizon-rfc` revision. An implementation that predates horizons refuses a cone that carries one.

**Stock fossil 2.28.** A sparse or horizon store must not be served as if it were dense, because the clone would be silently incomplete. A horizon store refuses a dense clone. A store held sparsely is served only when its `partial-mirror` setting is on (`repo config set partial-mirror on`); otherwise it refuses. The refusal puts the push card ahead of the error so fossil 2.28 aborts instead of carrying on. Fossil cannot read sub-manifest check-ins either, so a repository that uses them is for libfossil and zit peers only.

**A peer that knows nothing of horizons.** A horizon clone from stock Fossil works. The client asks for nothing below the horizon, records the below-horizon blobs it is sent as elided instead of storing them, and classifies locally. `HorizonServed` stays 0. Stock Fossil can announce a new check-in only inside a cluster. A horizon store holds no clusters, so it requests the cluster members as transit and classifies what arrives. Without that a new check-in would not be learned until a later one named it.

## Limits

- **Compaction is irreversible locally.** `CompactStore` and `repo cone compact` delete held blobs. Widening later fetches them again, which needs a peer that still has them. Structural artifacts, sub-manifest nodes, unsent or private content and the delta bases of retained artifacts are kept.
- **The structural horizon needs an origin that serves it.** Against a peer that does not answer `horizon-served structural` (stock fossil, or a server without a current seed) the clone falls back to the content horizon and reports `HorizonFallback`.
- **Seed signing is not implemented.** The structural horizon relies on a seed that an origin may sign. go-libfossil implements no signing, so a clone without a known key for its origin accepts an unsigned seed.
- **The horizon cursor needs a transport that names its peer.** The cursor (`pragma horizon-cursor`) lets an exchange skip re-announcing what both sides hold. The HTTP transport keys it by URL; a custom `Transport` keeps none and announces in full.
- **A horizon is not a checkout cone.** Give a checkout a horizon and you get `ErrHorizonCheckout`.
- **A sparse store lacks file content.** Commands that read an elided blob need a peer to fetch it, or fail.
