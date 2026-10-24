---
title: Horizons
weight: 70
---

A horizon cuts a store by time. It names a check-in **C**, a rule, and a depth. The check-ins the rule keeps are the **held set**. A blob is elided when no check-in in the held set names it. Structure (manifests, tags, control artifacts) is never elided by a content horizon.

A horizon is a member of a store cone, so everything on [Sparse cones](/docs/concepts/sparse-cones/) about store cones, elided artifacts and compaction applies to it too. The behaviour follows `draft-fossil-horizon-00`, implemented in step with zit.

The held set is computed from the check-in graph the repository holds. It is stored nowhere (§3.1).

## Rules

- **descendants** (the default): C and every check-in descended from it, through any parent, merges included.
- **leaves**: the descendants, plus, for every childless check-in that does not descend from C, every descendant of each check-in where its ancestry meets C's.

A check-in is childless when no other check-in names it as a parent, open or closed. A branch tip merged into another branch has a child, so it is not childless. Under the leaves rule, any two childless check-ins have their merge base in the held set, so merging branches that are still being extended never crosses the horizon. Both rules give a set that includes every child of every check-in in it.

The leaves rule has a cost you should check before using it. An old branch that was never merged forks from C's ancestry near the root, and the rule then holds every descendant of that fork. On fossil-scm.org's repository it held 20,907 of 21,008 check-ins at every depth tried. The descendants rule, with C 10, 100, 426 and 1000 check-ins back from the tip, saves 97, 86, 56 and 41 percent of that history (§3.4). Use the leaves rule when the repository keeps few old unmerged branches.

`repo cone cost <check-in>` (`Repo.HorizonCost`) reports what each rule would hold at a check-in, read from a repository that has the history:

```
$ libfossil -R repo.fossil repo cone cost trunk
horizon at trunk
descendants  1 check-ins, 3 blobs, 6 B expanded, 57 B stored
leaves  1 check-ins, 3 blobs, 6 B expanded, 57 B stored
```

## Depth

- **content** (the default): which blobs are held. Every check-in's structure is still held (§4).
- **structural** (`Horizon.Structural`, `--structural`): also cuts the manifests below C (§5). It needs a seed, a control artifact the origin mints so a clone can derive tag and branch state without C's ancestry.

A content horizon keeps all structure and elides only blobs; a structural horizon also leaves out the manifests below C.

## Seeds are never pushed

A seed is private to the clone that holds it, so a go-libfossil origin refuses any pushed control artifact that looks like one (§5.2.3 and its F.1 erratum):

- its U card is `go-libfossil`, the label go-libfossil writes on every seed it mints; or
- it carries a `horizon-seed` T card, whatever its U card.

The test runs when the artifact first becomes readable, on its content after any delta is applied. A push that carries a seed the origin can read within that push is refused whole: nothing it carried is stored, and the reply is one error card, `seed refused: a seed is private to the clone that holds it; it is never pushed`, which a go-libfossil pusher reports among its sync errors. A delta whose base has not arrived is stored as any dangling delta is, and withheld, never announced or sent, until something supplies the base: a later push, a pull, or a check-in or tag that writes the same bytes. That supplies only the base, so it is stored as usual, with no error, and the seed is refused then.

A refused seed's hash is recorded in a table of the store's own that no configuration group carries, so no peer pulls it. The seed stays stored, so a delta built on it stays readable, but it is never linked, announced or sent, and a re-push of it is refused at once. Pulling the same hash from a peer stores nothing new: the row is already held, and stays refused. Any other seed-shaped artifact a store receives by pull or clone is an ordinary control artifact.

Two consequences follow:

- Another implementation's label alone is ordinary. A control artifact under zit's `U zit-seed` with no `horizon-seed` T card is stored when pushed; one that carries a `horizon-seed` card is refused.
- A user whose login is `go-libfossil` cannot push control artifacts (tags, branch moves, closes) to a go-libfossil origin, since they carry that login as their U card. go-libfossil does not forbid the login; do not give it to a user.

## Signed seeds

An origin with the `clearsign` setting on (and the legacy `omitsign` off) signs each seed it mints with its `pgp-command`, the same settings fossil and zit sign check-ins with (§5.2.4). It keeps one signature per seed, so a seed keeps its name while the tag state it records is unchanged.

A clone given the origin's public key verifies every seed against it: at clone time, on every pull, and when it rebuilds. A seed that is unsigned, or signed by another key, is discarded and the clone falls back to the content horizon. A clone with no key accepts an unsigned seed. The key is the clone's own `horizon-seed-key` setting, never something the origin sends. `CloneOpts.SeedKey` and `repo clone --seed-key` set it when the clone is made; `Repo.SetConfig` refuses a value that is neither an SSH public key nor an OpenPGP fingerprint.

- **SSH:** the origin signs with `ssh-keygen -q -Y sign -n fossilscm -f KEY`; the clone's key is the public key line, verified in process.
- **PGP:** the origin signs with `gpg --clearsign -o`, fossil's default; the clone's key is the key's 40 or 64 digit fingerprint, verified by `gpg` against the user's keyring.

An origin signs only for a requester that may read, after the request is authorized, and keeps the signature in its own table; a store that cannot write, such as a read-only mirror, signs again for every request. Setting the origin's own `horizon-seed-key` to its public key makes it check a kept signature before serving it, so replacing the key behind an unchanged `pgp-command` takes effect at once. A clone that cannot run its verifier (gpg missing, or timed out) keeps the seed it holds and reports the error rather than falling back. Settings are read from the repository only: go-libfossil reads no global fossil configuration, so `clearsign` and `pgp-command` set with `fossil settings --global` do not apply.

[Signing](/docs/concepts/signing/) covers the signing settings and the verifier in more detail.

## Moving the horizon

Moving C earlier and pulling fetches what the deeper horizon keeps. Moving it later keeps what you hold until you compact. A horizon store cannot be a checkout's cone: a horizon governs what the repository holds, not which paths a checkout writes.

## Library usage

### Build a horizon cone

```go
h := libfossil.Horizon{
    Checkin:    "9f2c...",                   // full 40 or 64 digit hash
    Rule:       libfossil.HorizonDescendants, // or HorizonLeaves
    Structural: false,                        // true for the structural cut
}

// Horizon only: every path, cut by time.
hc, err := libfossil.NewHorizonCone(nil, nil, h)

// Horizon plus paths.
hc, err = libfossil.NewHorizonCone([]string{"src"}, nil, h)

got, ok := hc.Horizon()               // read it back
noHorizon, err := hc.WithHorizon(nil) // drop it, keep the paths
```

`Checkin` must be a full hash, and it is lower-cased. Dropping the horizon of a horizon-only cone is an error, since nothing would be left; use `SetStoreCone(nil)` to make the store dense.

### Clone into a horizon store

```go
t := libfossil.NewHTTPTransport("https://example.com/repo")
repo, res, err := libfossil.Clone(ctx, "repo.fossil", t, libfossil.CloneOpts{
    StoreCone: hc,
    SeedKey:   "ssh-ed25519 AAAA...", // optional; structural only
})
if err != nil {
    log.Fatal(err)
}
defer repo.Close()
fmt.Println(res.Elided, res.HorizonServed, res.StructuralServed, res.HorizonFallback)
```

- `HorizonServed`: replies where the server cut at the horizon (`pragma horizon-served`, §6.2.5). The other rounds were classified locally.
- `StructuralServed`: those served at the structural level.
- `HorizonFallback`: a structural clone kept the content horizon instead (§6.3).

`SyncResult` has the same fields.

`HorizonHistoryURL`, on both results, is the address the origin gives for querying history below the horizon outside the sync protocol, when it gives one. An origin sets it with `repo config set horizon-history-url https://...` (`SettingHorizonHistoryURL`); only one http or https URL with no spaces or credentials is accepted. `repo clone` prints it as `History:`.

### Deepen, merge across, and verify the seed

Setting, replacing and dropping a horizon on an existing store use `SetStoreCone` and `SetStoreConeContext`, as for any [store cone](/docs/concepts/sparse-cones/).

```go
// Move the horizon down far enough to reach a check-in, which may lie
// below it. C moves to that check-in when it is an ancestor of C, else to
// a common ancestor of the two, asked of the origin when the store cannot
// see one. The rule, the depth and the path members stay.
sr, err := repo.DeepenHorizon(ctx, target, libfossil.RequiredContentOpts{Peer: t})

// Merge. With a Peer, a merge where either side's ancestry crosses a
// structural horizon asks the origin for the base, deepening to it when it
// lies below. Without one it fails with an error matching
// libfossil.ErrBelowHorizon, even when the store finds a base it holds.
mr, err := co.MergeContext(ctx, libfossil.CheckoutMergeOpts{Version: rid},
    libfossil.RequiredContentOpts{Peer: t})

// Check the seed against the history it summarizes. This fetches every
// manifest below C and leaves a content-horizon store.
v, err := repo.VerifySeed(ctx, libfossil.RequiredContentOpts{Peer: t})
```

Deepening a structural store discards its seed before it fetches the new one, so the old seed cannot shadow the new one. If the transfer fails, the old horizon and seed are put back (§5.8).

A merge never runs against a base the store does not hold. A bisection whose good end lies below the horizon is refused the same way, naming the horizon and the deepen.

An origin checks each pushed check-in that creates a branch: if the branch it cancels is not the one this origin derives for the parent, the clone's seed was stale, and the whole push is refused with `stale seed: ...` (§5.7). Pull, push a control artifact that cancels the parent's real branch tag on the check-in, and push again.

## Command line

### Clone a horizon store

```
libfossil repo clone URL repo.fossil --horizon <full hash>
libfossil repo clone URL repo.fossil --horizon <full hash> --leaves --cone src
libfossil repo clone URL repo.fossil --horizon <full hash> --structural --seed-key "ssh-ed25519 AAAA..."
```

- `--horizon=STRING`: clone a horizon store: elide blobs below this check-in, given as its full 40 or 64 digit hash.
- `--leaves`: with `--horizon`, also hold every open leaf's ancestry back to the check-in.
- `--structural`: with `--horizon`, also elide the manifests below the check-in, deriving their branch and tag state from the origin's seed.
- `--seed-key=STRING`: the origin's public key, an SSH public key line or an OpenPGP fingerprint. With `--structural`, a seed not signed by it is discarded and the clone falls back to the content horizon. It is kept as the `horizon-seed-key` setting.

`--cone` and `--cone-not` combine with these. The clone needs a full hash because nothing is local yet to resolve a name against.

### Change the horizon

```
libfossil -R repo.fossil repo cone store --horizon <check-in>
libfossil -R repo.fossil repo cone store --horizon <check-in> --leaves
libfossil -R repo.fossil repo cone store --horizon <check-in> --structural
libfossil -R repo.fossil repo cone store --no-horizon
libfossil -R repo.fossil repo cone deepen <check-in>
libfossil -R repo.fossil repo cone cost <check-in>
```

- `--horizon=STRING`: elide blobs below this check-in (name, branch or hash prefix), fetching what a deeper horizon keeps. Unlike `clone`, a local name is fine.
- `--leaves`: with `--horizon`, also hold every open leaf's ancestry back to the check-in.
- `--structural`: with `--horizon`, also elide the manifests below the check-in; the next pull fetches the origin's seed.
- `--no-horizon`: drop the horizon, keeping the path members, and fetch what it elided.
- `cone deepen`: move the horizon down to reach the check-in, fetching what the deeper horizon keeps.
- `cone cost`: report what each rule would hold at the check-in, from this repository.

### Merge and verify

```
libfossil -R repo.fossil repo merge <version> -d DIR
libfossil -R repo.fossil repo verify --seed
```

- `merge`: a merge where either side's ancestry crosses a structural horizon asks the remote for the base, and deepens to it when it lies below. A merge whose ancestry stays above the horizon contacts no peer.
- `verify --seed`: verify the structural seed by fetching the history below the horizon. The store stays a content-horizon store.

`store`, `deepen`, `merge` and `verify` take `--remote` (default: the saved last-sync-url), `--sync-user`, `--sync-pass` (never saved), `--sync-private` and `--timeout` (default 10m; 0 disables it).

## Interoperability

**zit.** The horizon format matches zit's byte for byte. Horizon sync works in both directions against zit's `horizon-rfc` revision.

**Stock fossil 2.28.** A horizon store refuses a dense clone, because the clone would be silently incomplete.

**A peer that knows nothing of horizons.** A horizon clone from stock Fossil works. The client asks for nothing below the horizon, records the below-horizon blobs it is sent as elided instead of storing them, and classifies locally. `HorizonServed` stays 0. Stock Fossil can announce a new check-in only inside a cluster. A horizon store holds no clusters, so it requests the cluster members as transit and classifies what arrives (§4.5). Without that a new check-in would not be learned until a later one named it.

## Limits

- **The structural horizon needs an origin that serves it.** Against a peer that does not answer `horizon-served structural` (stock fossil, or a server without a current seed) the clone falls back to the content horizon and reports `HorizonFallback` (§6.3).
- **PGP verification needs gpg.** A PGP-signed seed is verified by running `gpg`, so a clone keyed with a fingerprint needs gpg installed and the key in its keyring; on js/wasm only SSH keys verify. gpg-agent hangs at start under a very large open-file limit, which a macOS shell can set to 2^31 (`ulimit -n`): lower it, to 4096 say, in the environment that signs or verifies with gpg.
- **The horizon cursor needs a transport that names its peer.** The cursor (`pragma horizon-cursor`, §6.2.6) lets an exchange skip re-announcing what both sides hold. The HTTP transport keys it by URL; a custom `Transport` keeps none and announces in full.
- **A horizon is not a checkout cone.** Give a checkout a horizon and you get `ErrHorizonCheckout`.
- **Compaction is irreversible locally.** `repo cone compact` drops the held blobs below the horizon; deepening later needs a peer that still has them.
