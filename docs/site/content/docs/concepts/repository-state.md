---
title: Repository state
weight: 30
---

[Artifacts](/docs/concepts/artifacts/) are the only facts a repository records. Everything else, such as which check-in is the tip of trunk, which branch a check-in is on, or which leaves are open, follows from them. `draft-fossil-repo-state-00` specifies that derivation, so two implementations reading the same artifacts reach the same answers. The one exception is the current trunk, which also reads the repository's `main-branch` setting (§9).

## The tables

A `.fossil` file stores the artifacts in `blob` and `delta`, and keeps the derived state in tables beside them, so a query does not re-parse every artifact.

| Table | Holds |
|---|---|
| `blob` | Every artifact, compressed, by `rid` and name (`uuid`). A row with no content is a phantom. |
| `delta` | Which blobs are stored as deltas, and against what. |
| `phantom` | Names the repository knows of but does not hold yet. |
| `event` | One row per check-in, tag, wiki edit, ticket change and so on: the timeline. |
| `plink` | Parent links between check-ins, with which parent is primary (§3). |
| `mlink` | Which file changed in which check-in, and from which earlier version. |
| `filename` | The file names `mlink` refers to. |
| `tag`, `tagxref` | Tag names, and each tag's effect on each check-in: cancel, singleton or propagating (§5). |
| `leaf` | Check-ins with no child on their own branch (§7). |
| `orphan` | Delta manifests whose baseline is absent. |

`blob` and `delta` are the source. The others are derived.

## Crosslinking

**Crosslinking** reads one artifact and writes its effects into the derived tables (§4). A check-in adds its `plink` and `mlink` rows and its `event` row. Its `T` cards, or a control artifact's, set `tagxref` rows and propagate along primary children until a cancel or another value blocks them (§5.4). Leaves are rechecked as parents gain children and as tags move (§7.6).

Artifacts can arrive in any order. Sync, clone, a commit and a rebuild all crosslink, and an artifact whose parent has not arrived yet is linked again when it does.

Branches are not stored as such. A check-in's branch is the value of its `branch` tag, inherited from its primary parent unless a tag on it says otherwise (§6). Name resolution (§8) turns a branch name, tag or `trunk` into one check-in: `Repo.ResolveVersion(name)`. `Repo.CurrentTrunk()` returns the tip of the main branch (§9).

## Rebuild gives the same rows

Because the derived tables are a function of the artifacts, they can be thrown away and computed again. go-libfossil's rebuild (`verify.Rebuild`, internal) empties the crosslinked tables and runs the same crosslink sweep a clone runs, so derived state has one owner. The tables it refills are `event`, `plink`, `leaf`, `mlink`, `tagxref`, `forumpost`, `attachment`, `backlink`, `cherrypick` and `orphan`, plus the sync bookkeeping tables `unclustered` and `unsent`.

The conformance suite checks this against fossil itself:

- Each scripted case in `conformance/repostate_*_test.go` loads a sequence of artifacts into go-libfossil (one sweep, one artifact at a time, and through rebuild) and into fossil(1). The rows must equal the expectation written from the draft, and fossil's rows must equal it too: its per-artifact crosslink for a crosslink case, `fossil rebuild` on a copy for a rebuild case. The compared tables are `plink`, `tagxref`, `event`, `phantom`, `leaf`, `orphan` and `mlink`.
- `TestRepoStateCorpus` takes real repositories (named in `CONFORMANCE_REPOS`) and derives their tables three ways: `fossil rebuild` on one copy, as the reference; go-libfossil's rebuild on a second; a crosslink sweep over emptied tables on a third. It compares `event`, `leaf`, `mlink`, `phantom`, `plink` and `tagxref` column by column. It runs only when fossil(1) is present.

Where fossil departs from the draft text, the case records it and names the draft change proposed. The requirements are listed in `conformance/requirements/repostate.tsv`.

## Where the code is

- `internal/manifest`: crosslinking (`crosslink.go`), re-derivation (`rederive.go`), the timeline.
- `internal/tag`: tag application and propagation.
- `internal/leaf`: the leaf table.
- `internal/verify`: integrity checks and rebuild.
- `internal/repo`: the schema and the `main-branch` setting.
