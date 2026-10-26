---
title: Signing
weight: 80
---

go-libfossil signs two kinds of artifact: check-ins, as fossil and zit sign them, and the seeds an origin mints for [structural horizons](/docs/concepts/horizons/). Each has its own signing command, and both use the same two envelopes, a PGP clear-signed message or the SSH envelope fossil builds around a detached `ssh-keygen` signature (`draft-fossil-artifact-format-00` §4.1.1).

## Settings

| Setting | Constant | Meaning |
|---|---|---|
| `clearsign` | `SettingClearsign` | Sign check-ins. Off by default. |
| `omitsign` | | Fossil's legacy setting. On, it turns check-in signing off again. |
| `pgp-command` | `SettingPGPCommand` | The check-in signing command. Unset, it is fossil's default, `gpg --clearsign -o `. |
| `horizon-seed-command` | `SettingHorizonSeedCommand` | The command an origin signs its horizon seeds with. Unset, seeds go out unsigned. |
| `horizon-seed-key` | `SettingHorizonSeedKey` | The public key a clone verifies its origin's seeds with. |

Settings are read from the repository only. go-libfossil reads no global fossil configuration, so values set with `fossil settings --global` do not apply.

## Signing check-ins

With `clearsign` on, every check-in a commit or a branch creation stores is signed. The command runs as fossil runs it:

- **PGP:** `COMMAND OUTFILE INFILE`, which writes a clear-signed copy of the manifest.
- **SSH:** a command whose name starts with `ssh` runs as `COMMAND INFILE` and writes a detached `INFILE.sig`, which is wrapped in fossil's SSH envelope. The namespace must be `fossilscm`.

```
libfossil -R repo.fossil repo config set clearsign on
libfossil -R repo.fossil repo config set pgp-command \
    "ssh-keygen -q -Y sign -n fossilscm -f ~/.ssh/id_ed25519"
```

The command runs through the shell, so `~` and environment variables expand. A signed check-in looks like this:

```
-----BEGIN SSH SIGNED MESSAGE-----

C signed
D 2026-10-23T20:45:21.089
F docs/x.md a09fd4d0...
...
Z 0e57ef44fd0eec16824d0a54ab247953
-----BEGIN SSH SIGNATURE-----
...
-----END SSH SIGNATURE-----
```

The stored bytes are the whole envelope, so the check-in's name is the hash of the signed bytes (§3.1). Readers strip the envelope before parsing, and the `Z` card covers the manifest inside it (§6.2).

To store one check-in unsigned, as fossil's `--nosign` does:

- `libfossil repo ci --nosign` and `libfossil repo branch new --nosign`.
- `CommitOpts.NoSign` (`Repo.Commit`) and `CheckoutCommitOpts.NoSign` (`Checkout.Checkin`).

### What fails

- **The signer fails or times out** (after two minutes). The commit is abandoned and nothing is stored.
- **The signer changes what it signs.** The envelope must strip back to exactly the manifest that was given, as zit checks it. A dash-escaped or rewritten manifest is refused rather than stored under a name its cards do not match.
- **On js/wasm** no external command can run, so signing fails there.

go-libfossil does not verify check-in signatures when it reads them. To check one, take the output of `libfossil repo cat` and run the signer's own verifier: `gpg --verify` on a PGP envelope, or, for SSH, `ssh-keygen -Y verify -n fossilscm -s SIG` with the manifest between the header and the signature block on standard input. The conformance test `TestSignedCheckin` checks that fossil and zit read the signed check-ins, and that a check-in zit signs with the same SSH key verifies here.

## Signing horizon seeds

An origin signs each structural horizon seed it mints with its `horizon-seed-command` (`draft-fossil-horizon-00` §5.2.4). The command is configured apart from check-in signing: `clearsign` and `pgp-command` never sign a seed, and an origin with no seed command sends its seeds unsigned even when it signs check-ins. It follows `pgp-command`'s contract. An SSH signer signs in the namespace `fossil-horizon-seed`, never `fossilscm`, so that a signature on a check-in or tag cannot stand in for a seed's; an origin whose seed command signs in another namespace serves no seed, and the request fails. PGP has no namespace and relies on the seed's [binding card](/docs/concepts/horizons/#the-binding-card) alone.

```
libfossil -R repo.fossil repo config set horizon-seed-command \
    "ssh-keygen -q -Y sign -n fossil-horizon-seed -f ~/.ssh/id_ed25519"
```

The origin keeps one signature per seed in its own table, so a seed keeps its name while the tag state it records is unchanged. It signs only for a requester allowed to read, after the request is authorized. A store that cannot write, such as a read-only mirror, signs again for every request.

A clone verifies seeds against its `horizon-seed-key` setting:

```go
repo, res, err := libfossil.Clone(ctx, "repo.fossil", t, libfossil.CloneOpts{
    StoreCone: hc, // a cone with a structural horizon
    SeedKey:   "ssh-ed25519 AAAA... origin",
})
```

```
libfossil repo clone URL repo.fossil --horizon <full hash> --structural \
    --seed-key "ssh-ed25519 AAAA... origin"
```

The key is an SSH public key line, verified in process, or a 40 or 64 digit OpenPGP fingerprint, verified by `gpg` against the user's keyring. `Repo.SetConfig` refuses a `horizon-seed-key` that is neither. The key is the clone's own; the origin never sends it.

The clone verifies every seed it holds: at clone time, on every pull, and when it rebuilds.

| Seed | Key set | Result |
|---|---|---|
| Signed by the key, in namespace `fossil-horizon-seed` for SSH | yes | Held. |
| Unsigned, signed by another key, or SSH-signed in another namespace | yes | Discarded; the store falls back to the content horizon. |
| Any | no | Held. A clone with no key accepts an unsigned seed. |
| Verifier could not run (gpg missing or timed out, or an SSH key in a program that does not link `sshverify`) | yes | The seed already held is kept and the error is reported. A new seed is refused. Nothing falls back. |

Whatever its signature, a seed is also refused when its binding card does not name the clone's own horizon or it is dated before the seed the clone holds; see [the binding card](/docs/concepts/horizons/#the-binding-card).

Setting the origin's own `horizon-seed-key` to its public key makes it check a kept signature before serving it, so replacing the key behind an unchanged seed command takes effect at once.

A signature proves where a seed came from, not that its derivation is right (§5.2.4). `repo verify --seed` (`Repo.VerifySeed`) checks the seed against the history it summarizes.

## Verifying SSH signatures from Go

SSH signatures are verified in process, and the library links that cryptography only into a program that asks for it. A program that verifies seeds against an SSH key imports the `sshverify` package for its side effect:

```go
import (
    libfossil "github.com/danmestas/go-libfossil"
    _ "github.com/danmestas/go-libfossil/sshverify"
)
```

The `libfossil` command imports it. A program that does not, and never verifies an SSH signature, leaves out `golang.org/x/crypto/ssh` and the ECDSA, Ed25519 and RSA code it pulls in, which matters to a wasm build. Signing needs no import: it runs `ssh-keygen`. PGP keys are verified by `gpg` either way.

Without the import, nothing signed is accepted unchecked. A clone whose `horizon-seed-key` is an SSH key refuses a new seed, and a sync or rebuild fails with an error wrapping `libfossil.ErrSSHVerifierUnavailable`, which names the package to import; the seed already held is kept, and nothing falls back. A seed whose signature is malformed or in another namespace is still discarded, since that needs no cryptography to tell. A clone with no key is unaffected. An origin whose own `horizon-seed-key` is an SSH key cannot check its kept signatures, so it signs each seed afresh for every request.

## Limits

- **PGP verification needs gpg** installed, with the origin's key in the keyring. On js/wasm only SSH keys verify.
- **gpg-agent hangs under a very large open-file limit.** A macOS shell can set `ulimit -n` to 2^31. Lower it, to 4096 say, in the environment that signs or verifies with gpg.

## Where the code is

- `signing.go`: the setting constants.
- `internal/clearsign`: running the signer, the SSH envelope, gpg verification, and the registry the SSH verifier joins.
- `sshverify`: SSH signature verification in process, linked by importing it.
- `internal/manifest/manifest.go`: signing a check-in before it is stored.
- `internal/sync/horizon_seedsign.go` and `internal/horizon/seed`: signing seeds at the origin and verifying them at the clone.
