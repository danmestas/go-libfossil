---
title: Settings
weight: 30
---

A repository setting is one row of the `config` table in the repository file. go-libfossil reads the settings below. Some are fossil's own; the rest are extensions, and each entry says which.

Set a setting with the CLI, or with `Repo.SetConfig(key, value)` in the Go API:

```
libfossil repo -R project.fossil config set clearsign on
libfossil repo -R project.fossil config get clearsign
libfossil repo -R project.fossil config ls
```

`config set` stores the value as given and stamps it with the current time. It checks only the values noted in the entries for `hash-policy`, `horizon-history-url` and `horizon-seed-key`. A setting that is not set has no row; `config get` reports it as not found. The CLI has no command that deletes a setting.

Boolean settings are read as fossil reads them: `on`, `yes`, `true` (any case) or a nonzero number mean true; `off`, `no`, `false`, `0` mean false. Any other text gives the setting's default. The reader is [`SettingBoolean`](https://github.com/danmestas/go-libfossil/blob/main/internal/repo/setting.go#L14). Entries marked "boolean" below use it unless they say otherwise.

go-libfossil reads only the repository's own `config` table. Fossil also falls back to a per-user global configuration in `~/.fossil`; go-libfossil does not.

| Setting | Default | Read by |
| --- | --- | --- |
| [`clearsign`](#clearsign) | off | check-in signing |
| [`omitsign`](#omitsign) | off | check-in signing |
| [`pgp-command`](#pgp-command) | `gpg --clearsign -o ` | check-in signing |
| [`horizon-seed-command`](#horizon-seed-command) | none | seed signing |
| [`horizon-seed-key`](#horizon-seed-key) | none | clone, seed verification |
| [`partial-mirror`](#partial-mirror) | off | sync server |
| [`horizon-history-url`](#horizon-history-url) | none | sync server |
| [`enable-submanifests`](#enable-submanifests) | off | commit, sync |
| [`submanifest-k`](#submanifest-k) | unset | commit, sync |
| [`submanifest-auto-retune`](#submanifest-auto-retune) | off | commit |
| [`hash-policy`](#hash-policy) | derived from content | every artifact store |
| [`case-sensitive`](#case-sensitive) | on, except macOS and Windows | CLI path matching |
| [`merge-strategy`](#merge-strategy) | `three-way` | merge |
| [`main-branch`](#main-branch) | `trunk` | branch membership, leaves |
| [`hash-digits`](#hash-digits) | 10 | hash display |
| [`allow-symlinks`](#allow-symlinks) | off | checkout, file scan |
| [`mtime-changes`](#mtime-changes) | on | checkout file scan |
| [`repo-cksum`](#repo-cksum) | on | checkout |
| [`uv-sync`](#uv-sync) | off | sync server, clone |
| [`lock-timeout`](#lock-timeout) | 60 seconds | check-in lock |
| [`forbid-delta-manifests`](#forbid-delta-manifests) | on | sync server |

The last list on this page covers internal rows that are not meant to be edited.

## clearsign

Turns on check-in signing. When it is on, every check-in that a commit or branch creation stores is signed. It signs no horizon seed: an origin signs those with [`horizon-seed-command`](#horizon-seed-command). See [Signing](/docs/concepts/signing/).

- **Read by:** [`clearsign.FromSettings`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L93), called from the manifest writer ([`manifest.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/manifest.go#L494)).
- **Values:** boolean. Unrecognized text reads as off.
- **Default:** off. With it off, no signing happens.
- **Behaviour:** the signed text is the artifact, so a signed check-in is named by the hash of the signed bytes. If the signer fails, the commit is abandoned. `CommitOpts.NoSign` and `CheckoutCommitOpts.NoSign` store one check-in unsigned.
- **Origin:** fossil 2.28 defines `clearsign` (default off). The envelope format is in `draft-fossil-artifact-format-00` section 4.1.1.
- **Set:** `libfossil repo config set clearsign on`
- **Code:** [`SettingClearsign`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L34), [`libfossil.SettingClearsign`](https://github.com/danmestas/go-libfossil/blob/main/signing.go#L13)

## omitsign

Turns signing off again, even when `clearsign` is on.

- **Read by:** [`clearsign.FromSettings`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L93), after `clearsign`.
- **Values:** boolean. Unrecognized text reads as off.
- **Default:** off.
- **Origin:** fossil's legacy setting. It is not in the list printed by `fossil settings` in 2.28. go-libfossil reads it so a repository that carries it keeps its meaning. No draft specifies it.
- **Set:** `libfossil repo config set omitsign on`
- **Code:** [`SettingOmitsign`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L35)

## pgp-command

The command that signs check-ins. Used only when signing is on.

- **Read by:** [`clearsign.FromSettings`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L93), which builds the `Signer`.
- **Values:** a shell command. The command runs through the shell, so `~` and environment variables expand. A command whose first word starts with `ssh` is run as `COMMAND INFILE`; it must write a detached `INFILE.sig`, and go-libfossil wraps it in fossil's SSH envelope. Any other command is run as `COMMAND OUTFILE INFILE` and must write a clear-signed `INFILE` to `OUTFILE`. A value that reads as false (`off`, `no`, `false`, `0`) turns signing off. An empty value uses the default.
- **Default:** `gpg --clearsign -o `.
- **SSH signing:** set it to `ssh-keygen -q -Y sign -n fossilscm -f ~/.ssh/id_ed25519`. The namespace must be `fossilscm`, or a clone cannot verify the signature.
- **Origin:** fossil 2.28 defines `pgp-command`. Go-libfossil follows fossil's `src/clearsign.c` contract, so one setting signs with either tool.
- **Set:** `libfossil repo config set pgp-command 'ssh-keygen -q -Y sign -n fossilscm -f ~/.ssh/id_ed25519'`
- **Code:** [`SettingPGPCommand`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L36), [`libfossil.SettingPGPCommand`](https://github.com/danmestas/go-libfossil/blob/main/signing.go#L25)

## horizon-seed-command

The command an origin signs the horizon seeds it mints with, configured apart from check-in signing. See [Signing](/docs/concepts/signing/) and [Horizons](/docs/concepts/horizons/).

- **Read by:** [`clearsign.SeedSigner`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L122), called from the horizon seed signer ([`horizon_seedsign.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/horizon_seedsign.go#L43)).
- **Values:** a shell command under `pgp-command`'s contract. An SSH signer must sign in the namespace `fossil-horizon-seed`, never `fossilscm`; an origin whose command signs in another namespace serves no seed, and the request fails. A value that reads as false (`off`, `no`, `false`, `0`), or an empty one, turns seed signing off.
- **Default:** none. With no command, seeds go out unsigned, even when `clearsign` signs check-ins. `clearsign` and `omitsign` do not affect it.
- **Origin:** go-libfossil extension, specified in `draft-fossil-horizon-00` section 5.2.4, which requires the seed-signing command to be configured apart from the check-in one but does not name the setting.
- **Set:** `libfossil repo config set horizon-seed-command 'ssh-keygen -q -Y sign -n fossil-horizon-seed -f ~/.ssh/id_ed25519'`
- **Code:** [`SettingSeedCommand`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L38), [`libfossil.SettingHorizonSeedCommand`](https://github.com/danmestas/go-libfossil/blob/main/signing.go#L50)

## horizon-seed-key

The public key a clone uses to verify the horizon seeds its origin sends. See [Signing](/docs/concepts/signing/) and [Horizons](/docs/concepts/horizons/).

- **Read by:** [`clearsign.SeedKey`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L251), called from the seed code ([`seed/sign.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/horizon/seed/sign.go#L26)) and the seed signer.
- **Values:** an SSH public key line (`ssh-ed25519 AAAA... comment`), checked in process, or an OpenPGP fingerprint of 40 or 64 hex digits (spaces allowed), checked by `gpg` against the user's keyring. `Repo.SetConfig` and `config set` refuse any other non-empty value. An empty value counts as no key.
- **Default:** none. With no key, a clone accepts an unsigned seed.
- **Behaviour:** with a key set, a seed that is unsigned, not signed by that key, or SSH-signed in another namespace than `fossil-horizon-seed` is discarded and the clone falls back to the content horizon.
- **Origin:** go-libfossil extension, specified in `draft-fossil-horizon-00` section 5.2.4. It belongs to the clone; an origin never sends it.
- **Set:** `libfossil repo config set horizon-seed-key 'ssh-ed25519 AAAA...'`, or `CloneOpts.SeedKey` at clone time ([`keepSeedKey`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/clone.go#L327)).
- **Code:** [`SettingSeedKey`](https://github.com/danmestas/go-libfossil/blob/main/internal/clearsign/clearsign.go#L37), [`libfossil.SettingHorizonSeedKey`](https://github.com/danmestas/go-libfossil/blob/main/signing.go#L36)

## partial-mirror

Lets a sparse store serve clones that its cone does not cover. See [Sparse cones](/docs/concepts/sparse-cones/).

- **Read by:** [`handler.servesPartialMirror`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/sparse_policy.go#L25), for clone requests. It covers every serving surface: `HandleSync`, `XferHandler` and `ServeHTTP`.
- **Values:** boolean. Unrecognized text reads as off. The `PartialMirror` serve option overrides the row and turns serving on ([`handler.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/handler.go#L175)).
- **Default:** off. A sparse store then refuses such a clone with one error card and no content, because the clone would get elided phantoms it must resolve elsewhere.
- **Origin:** go-libfossil extension. `draft-fossil-sparse-cone-00` section 5.3 says a server should refuse unless "explicitly configured as a partial mirror". Fossil has no such setting.
- **Set:** `libfossil repo config set partial-mirror on`
- **Code:** [`SettingPartialMirror`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/sparse_policy.go#L21), [`libfossil.SettingPartialMirror`](https://github.com/danmestas/go-libfossil/blob/main/repo_sparse.go#L19)

## horizon-history-url

The address of an interface, outside the sync protocol, where history below a horizon can be queried. An origin appends it to the `horizon-served` pragma. See [Horizons](/docs/concepts/horizons/).

- **Read by:** [`historyURL`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/sparse.go#L175) in the sync server. Clients report it as `CloneResult.HorizonHistoryURL` and `SyncResult.HorizonHistoryURL`.
- **Values:** one `http` or `https` URL with a host, no white space or control bytes, no credentials, at most 2048 bytes. [`CheckHistoryURL`](https://github.com/danmestas/go-libfossil/blob/main/internal/repo/setting.go#L79) enforces this, and `SetConfig` refuses anything else. An empty value names none. A bad value that got into the table some other way is treated as unset when serving.
- **Default:** none.
- **Origin:** go-libfossil extension, `draft-fossil-horizon-00` section 6.2.5.
- **Set:** `libfossil repo config set horizon-history-url https://history.example/q`
- **Code:** [`SettingHorizonHistoryURL`](https://github.com/danmestas/go-libfossil/blob/main/internal/repo/setting.go#L70), [`libfossil.SettingHorizonHistoryURL`](https://github.com/danmestas/go-libfossil/blob/main/repo_sparse.go#L28)

## enable-submanifests

Makes new commits write tree manifests (sub-manifests) instead of flat ones. See [Sub-manifests](/docs/concepts/sub-manifests/).

- **Read by:** [`readSubmanifestSettings`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/submanifest_write.go#L80) at commit ([`emissionK`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/submanifest_write.go#L114)); [`enableSubmanifests`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/submanifest.go#L151) in the sync server, where it decides the capability pragmas; and the required-content state digest.
- **Values:** `on`, `off`, `1`, `0`, `yes`, `no`, `true`, `false` (any case). Any other text reads as off. A commit also emits no tree in a sync session where the server sent `avoid-submanifests`.
- **Default:** off.
- **Behaviour:** once a tree manifest exists, peers that do not announce sub-manifest support (every fossil) are refused artifacts that carry an `S` card. Turning the setting off later does not retire trees already written.
- **Config exchange:** it is a `/project` setting, so clone and pull carry it. A received set is applied all or none: a bad value, or `enable-submanifests` on with no `submanifest-k` from 1 to 12, leaves all three as they were ([`apply`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/submanifest.go#L260)).
- **Origin:** go-libfossil and zit extension. No fossil release has it. Specified in `draft-fossil-submanifests-00` sections 6.3 and 8.1.
- **Set:** prefer `libfossil repo submanifests enable [--k N]`. It pins `submanifest-k` first and prints the warning about what cannot be undone ([`EnableSubmanifests`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/submanifest_write.go#L153)). `libfossil repo submanifests disable` turns it off. `config set enable-submanifests on` works but skips those checks.
- **Code:** [`SettingEnableSubmanifests`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/submanifest_write.go#L31)

## submanifest-k

The partition parameter k that a commit uses to build the tree.

- **Read by:** [`readSubmanifestSettings`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/submanifest_write.go#L80), then `emissionK` at commit.
- **Values:** a decimal integer from 1 to 12. Anything else is not a valid pin. A commit with `enable-submanifests` on and no valid pin logs a warning and writes a flat manifest.
- **Default:** unset.
- **Behaviour:** a valid pin takes precedence over an explicit k on enable; changing it is a re-tune.
- **Origin:** extension, `draft-fossil-submanifests-00` sections 6.3 and 8.1. A `/project` setting.
- **Set:** `libfossil repo submanifests enable --k N` for the first pin, `libfossil repo submanifests retune --k N` to change it, `libfossil repo submanifests status` to read all three. `config set submanifest-k N` stores any text without checking the range.
- **Code:** [`SettingSubmanifestK`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/submanifest_write.go#L32)

## submanifest-auto-retune

Allows a commit to re-pin `submanifest-k` by itself when the analyzed optimum has drifted.

- **Read by:** [`checkinTree.retune`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/submanifest_write.go#L443), after a commit that wrote a tree.
- **Values:** same boolean spellings as `enable-submanifests`. Any other text reads as off.
- **Default:** off. With it off, a drift only logs an advisory.
- **Behaviour:** the re-tune runs after the commit is durable, in its own transaction. If the pin changed meanwhile, nothing is written. A failure is logged and the commit stands.
- **Origin:** extension, `draft-fossil-submanifests-00` section 6.4 (a project-scope boolean, default false, never on by default). A `/project` setting.
- **Set:** `libfossil repo config set submanifest-auto-retune on`. There is no dedicated command.
- **Code:** [`SettingAutoRetune`](https://github.com/danmestas/go-libfossil/blob/main/internal/manifest/submanifest_write.go#L33)

## hash-policy

Which hash names new artifacts: SHA-1 or SHA3-256.

- **Read by:** [`hash.NamingFor`](https://github.com/danmestas/go-libfossil/blob/main/internal/hash/policy.go#L116) (through `policyFor`) before every artifact store; [`PromoteFor`](https://github.com/danmestas/go-libfossil/blob/main/internal/hash/policy.go#L160) rewrites it.
- **Stored form:** the ordinal, 0 to 4: `0` sha1, `1` auto, `2` sha3, `3` sha3-only, `4` shun-sha1. A stored value outside 0 to 4, or not a number, is an error, not a reason to fall back.
- **Default when no row exists:** `sha3` if the repository holds any SHA3-named artifact or no SHA-1 one; otherwise `auto`. It never derives `shun-sha1`. A new repository is created with `hash-policy` = 2. A clone removes the row so that it adopts its source's policy ([`clone.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/clone.go#L303)).
- **Behaviour:** under `auto`, storing the first SHA3-named artifact promotes the row to `sha3`. Under `sha3-only` and `shun-sha1` legacy names are not reused.
- **Origin:** fossil defines the policy (`fossil hash-policy`, stored in the `config` table; it is not in `fossil settings`). Specified in `draft-fossil-artifact-format-00` section 7. It is a `/project` setting in config exchange.
- **Set:** in Go, `Repo.SetHashPolicy("sha3")` ([`repo_admin.go`](https://github.com/danmestas/go-libfossil/blob/main/repo_admin.go#L102)); names are matched exactly and case-sensitively, and `auto` in a repository that already holds a SHA3 artifact stores `sha3`. The CLI has no policy command. `config set hash-policy 2` takes the ordinal; `config set hash-policy sha3` is refused.
- **Code:** [`SetConfig` check](https://github.com/danmestas/go-libfossil/blob/main/internal/repo/repo.go#L302), [`SetHashPolicy`](https://github.com/danmestas/go-libfossil/blob/main/internal/repo/repo.go#L331), [seeded value](https://github.com/danmestas/go-libfossil/blob/main/db/schema.go#L348)

## case-sensitive

Whether file names that differ only in case are different files.

- **Read by:** [`Repo.FilenamesCaseSensitive`](https://github.com/danmestas/go-libfossil/blob/main/internal/repo/setting.go#L57). The only caller is the CLI's PATH matching ([`newRepoPaths`](https://github.com/danmestas/go-libfossil/blob/main/cli/pathmatch.go#L19)), used by `repo cat`, `repo diff` and `repo log`. When it is off, names compare with ASCII case folding.
- **Values:** boolean.
- **Default:** off on macOS and Windows, on elsewhere, as fossil's platform default.
- **Origin:** fossil 2.28 defines `case-sensitive`. No draft specifies it.
- **Set:** `libfossil repo config set case-sensitive off`
- **Code:** [`setting.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/repo/setting.go#L57)

## merge-strategy

The merge strategy used for files that no `.libfossil-merge` rule matches.

- **Read by:** [`merge.LoadResolver`](https://github.com/danmestas/go-libfossil/blob/main/internal/merge/resolve.go#L28), called by `Checkout.Merge` ([`merge.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/checkout/merge.go#L507)).
- **Values:** the name of a registered strategy: `three-way`, `binary`, `last-writer-wins` or `conflict-fork`. An empty value is ignored. An unknown name is not checked when the setting is stored; the merge fails with `unknown merge strategy "NAME" for FILE`.
- **Default:** `three-way`.
- **Precedence:** `libfossil repo merge --strategy NAME` overrides everything. Without it, the first matching glob in `.libfossil-merge` at the checked-out version wins, then this setting.
- **Origin:** go-libfossil extension. Fossil has no such setting and no draft specifies it.
- **Set:** `libfossil repo config set merge-strategy binary`
- **Code:** [`resolve.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/merge/resolve.go#L35)

## main-branch

The name of the default branch, for check-ins with no active branch tag.

- **Read by:** [`repo.MainBranch`](https://github.com/danmestas/go-libfossil/blob/main/internal/repo/mainbranch.go#L21), and the SQL fragments in [`leaf.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/leaf/leaf.go#L61) and [`fork.go`](https://github.com/danmestas/go-libfossil/blob/main/internal/checkout/fork.go#L139).
- **Values:** a branch name. An empty value reads as unset.
- **Default:** `trunk`.
- **Origin:** fossil 2.28 defines `main-branch`. `draft-fossil-repo-state-00` section 6.1 requires readers not to assume `trunk` when it is set. Reading an empty value as unset departs from the draft and from fossil.
- **Set:** `libfossil repo config set main-branch main`

## hash-digits

How many hash characters fossil's `%S` conversion prints.

- **Read by:** [`fsltype.HashDigits`](https://github.com/danmestas/go-libfossil/blob/main/internal/fsltype/printf.go#L23).
- **Values:** an integer, clamped to 6 through 64. Zero reads as unset. The URL length is the value plus 6, at least 16 and at most 64.
- **Default:** 10.
- **Origin:** fossil 2.28 defines `hash-digits`. No draft specifies it.
- **Set:** `libfossil repo config set hash-digits 12`

## allow-symlinks

Whether a symbolic link on disk is seen as itself.

- **Read by:** [`vfile.OpenDisk`](https://github.com/danmestas/go-libfossil/blob/main/internal/vfile/disk.go#L70), when a checkout opens its disk view.
- **Values:** boolean.
- **Default:** off. A link is then read as a file holding the link's target name, as fossil does.
- **Origin:** fossil 2.28 defines `allow-symlinks`. No draft specifies it.
- **Set:** `libfossil repo config set allow-symlinks on`

## mtime-changes

Whether a checkout file scan may trust file modification times.

- **Read by:** [`newScanPolicy`](https://github.com/danmestas/go-libfossil/blob/main/internal/checkout/vfile.go#L310). A hash scan never trusts mtimes, whatever this says.
- **Values:** boolean.
- **Default:** on. With it off, every scan hashes the file.
- **Origin:** fossil 2.28 defines `mtime-changes`. No draft specifies it.
- **Set:** `libfossil repo config set mtime-changes off`

## repo-cksum

Whether a checkout verifies the manifest's `R` card.

- **Read by:** [`checkRCard`](https://github.com/danmestas/go-libfossil/blob/main/internal/checkout/extract.go#L132), after extracting a version.
- **Values:** boolean.
- **Default:** on. A disagreement is a warning, not an error. A sparse store skips the check.
- **Origin:** fossil 2.28 defines `repo-cksum`. No draft specifies it.
- **Set:** `libfossil repo config set repo-cksum off`

## uv-sync

Whether a clone also receives the unversioned-file catalog.

- **Read by:** [`handler.sendCloneUVCatalog`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/handler_uv.go#L50), in the sync server, for a requester with read access.
- **Values:** `1`, `on`, `yes`, `true` (any case) mean on. Anything else, including an unrecognized number, means off.
- **Default:** off.
- **Origin:** fossil 2.28 defines `uv-sync`. No draft cited here specifies it.
- **Set:** `libfossil repo config set uv-sync on`

## lock-timeout

How long a check-in lock (`ci-lock`) stays valid, in seconds.

- **Read by:** [`lockTimeout`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/ckin_lock.go#L21), in the sync server.
- **Values:** an integer, read by SQLite's integer conversion, so text that is not a number reads as 0. A value of 0 or less makes every lock stale at the next `ci-lock`, as in fossil.
- **Default:** 60.
- **Origin:** fossil 2.28 defines `lock-timeout`. `draft-fossil-sync-protocol-00` section 9.4.
- **Set:** `libfossil repo config set lock-timeout 120`

## forbid-delta-manifests

Whether the server tells clients not to send delta manifests (the `avoid-delta-manifests` pragma).

- **Read by:** [`forbidDeltaManifests`](https://github.com/danmestas/go-libfossil/blob/main/internal/sync/ckin_lock.go#L36), in the sync server.
- **Values:** boolean. A missing or NULL row is on, and text that is neither true nor false keeps on.
- **Default:** on.
- **Origin:** fossil 2.28 defines `forbid-delta-manifests` with a default of off. go-libfossil follows `draft-fossil-sync-protocol-00` section 9.4 (fossil 2.29's default), so against a fossil 2.28 server's own default the two differ unless the row is set. It is a `/project` setting in config exchange.
- **Set:** `libfossil repo config set forbid-delta-manifests off`

## Internal rows

These rows are bookkeeping. Do not edit them. Some are written by sync or by `repo clone`, and some are read only by internal code.

- `project-code`, `server-code`: the repository's identities, set at creation or taken from the origin on clone. Fossil defines them.
- `aux-schema`, `content-schema`: schema versions, set at creation. Fossil defines them.
- `last-sync-url`: the default peer, written by `repo clone` and read by commands that take no URL. Fossil defines it.
- `seen-submanifest`: set once the repository stores an artifact with an `S` card, never cleared, never synced. Extension (`draft-fossil-submanifests-00` section 8.1).
- `horizon-cursor:ORIGIN` and `horizon-rekept`: horizon sync state. Extension.
- `ci-lock-HASH` and `libfossil-ci-lock-*`: check-in lock rows. The first form is fossil's.
- `uv-hash`: cached hash of the unversioned catalog.
- `invite-expire:LOGIN`: when a login's invite expires. go-libfossil only.
- `link:URL`: links received in share-links sync.

Other rows that sync carries as configuration, such as `project-name`, `ignore-glob` or the skin settings, pass through the `config` table without go-libfossil acting on them.

See the [CLI reference](/docs/reference/cli/) for the commands above.
