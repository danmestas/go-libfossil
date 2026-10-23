---
title: Sub-manifests
weight: 50
---

A check-in manifest has one `F` card per file in the tree, not per changed file. Its size grows with the width of the tree, and every check-in pays it. Sub-manifests split the file list into a tree of smaller artifacts, so a typical check-in rewrites the root manifest and one leaf. The format is `draft-fossil-submanifests-00`.

Two implementations exist: zit writes, reads and serves sub-manifests, and go-libfossil writes and reads them. The two interoperate. Fossil does not implement the draft (§1.3).

## The tree

A **sub-manifest** carries a contiguous slice of a check-in's sorted file list: `F` cards in a leaf, or `S` cards that point to lower sub-manifests in an interior node. A manifest whose files are given by `S` cards is a **tree manifest**. Walking the tree in order gives exactly the file list a flat manifest would carry (§5.1).

```
S src/a.c 5d0c...
```

An `S` card names the first file of the slice and the sub-manifest that holds it (§3.6). Sub-manifests are a ninth artifact type, `submanifest` (§4.1).

Where the boundaries fall depends only on file names. A name starts a new leaf when the SHA3-256 of the name has at least `k` leading zero bits (§6.1). Editing a file changes one leaf. Adding or removing a name can change a few until the boundaries line up again (§6.5). `k` is never written into an artifact, and readers do not need it.

## Settings

Three repository settings control writing. They travel by configuration exchange: a clone asks for them in its cleanup round and adopts them, and a pull does not (§6.3).

| Setting | Meaning | Default |
|---|---|---|
| `enable-submanifests` | Write new check-ins as trees (§8.1). | off |
| `submanifest-k` | The partition parameter, 1 to 12 (§6.3). | unpinned |
| `submanifest-auto-retune` | Let a commit re-pin `k` when it drifts (§6.4). | off |

When emission is first enabled with no pinned `k`, one is selected by the cost in §6.3 over the file list of the main branch's tip (the newest check-in when no check-in is on the main branch), then refined by replaying the last 200 check-ins. A tree with too few files for any `k` to split has no selection, and enabling fails; give `--k` explicitly. A pinned `k` outside 1 to 12 makes commits fall back to flat manifests with a warning.

A re-tune is forward only. The next commit shares only the sub-manifests whose bytes are the same under both values. With `submanifest-auto-retune` on, a commit re-pins `k` when the best candidate costs more than 15% less than the pinned one, zit's margin. With it off, the drift is only logged.

## Command line

```
$ libfossil -R repo.fossil repo submanifests status
enable-submanifests:     off
submanifest-k:           (unpinned)
submanifest-auto-retune: off
trees in repository:     no

$ libfossil -R repo.fossil repo submanifests enable --k 4
$ libfossil -R repo.fossil repo submanifests retune --k 5
$ libfossil -R repo.fossil repo submanifests disable
```

- `enable [--k N]`: write new check-ins as trees, pinning `k` (selected when `--k` is absent). It prints a warning that fossil peers will be refused the trees.
- `retune --k N`: re-pin `k` for later commits.
- `disable`: write flat manifests again. Existing trees stay.
- `status`: show the three settings and whether the repository holds any tree.

The library calls are `Repo.EnableSubmanifests(k)` (0 selects; it returns the pinned `k` and the warning), `Repo.RetuneSubmanifests(k)` and `Repo.DisableSubmanifests()`.

## Sync

A peer that reads sub-manifests says so with `pragma send-submanifests` in every request and reply (§7.2). go-libfossil always sends it, whatever its settings. A peer that has not sent it is refused every sub-manifest and every tree manifest, with an explicit error. Nothing is converted or downgraded. Every fossil release is such a peer, so a repository with trees serves fossil only the history before the first tree.

A server whose `enable-submanifests` is off also sends `pragma avoid-submanifests`. It binds only commits made during that sync session. go-libfossil commits outside a session and pushes afterwards, so the pragma never binds its commits: whether a commit writes a tree is decided by the local `enable-submanifests` alone. The pragma does not stop a server storing a tree pushed to it.

## Things to know before enabling

- **It is a flag day.** Every reader of the repository must understand sub-manifests before any writer emits one (§8). A repository that a fossil peer serves must not enable them.
- **Trees cannot be retired.** Disabling stops new trees; artifact names are immutable, so the ones written stay, and fossil peers are still refused them.
- **Sparse stores hold every tree node.** A [sparse store](/docs/concepts/sparse-cones/) keeps all sub-manifest nodes, so history stays readable without the file content.

## Where the code is

- `internal/manifest`: tree construction (`treebuild.go`), writing and settings (`submanifest_write.go`), reading (`tree.go`).
- `internal/sync/submanifest.go`: the capability gate, both pragmas, and configuration exchange of the settings.
- `repo_submanifest.go` and `cli/repo_submanifests.go`: the public API and command.
