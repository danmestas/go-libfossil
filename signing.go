package libfossil

import "github.com/danmestas/go-libfossil/internal/clearsign"

// SettingClearsign names the boolean repository setting, read as fossil reads
// one, that signs every check-in a commit or a branch creation stores, as
// fossil and zit sign them. Off by default. It signs no horizon seed: see
// SettingHorizonSeedCommand. The signed text is the
// artifact, so a signed check-in's name is the hash of the signed bytes; a
// signer that fails abandons the commit. CommitOpts.NoSign and
// CheckoutCommitOpts.NoSign store one check-in unsigned, as fossil's
// --nosign does, and fossil's legacy omitsign setting turns signing off.
const SettingClearsign = clearsign.SettingClearsign

// SettingPGPCommand names the signing command, as fossil names it. Unset, it
// is fossil's default, "gpg --clearsign -o ", run as COMMAND OUTFILE INFILE.
// A command whose name starts with "ssh" is run as COMMAND INFILE and writes
// a detached INFILE.sig, which is wrapped in fossil's SSH envelope; to sign
// with an SSH key, set it to
//
//	ssh-keygen -q -Y sign -n fossilscm -f ~/.ssh/id_ed25519
//
// The namespace must be fossilscm for a clone to verify the signature. The
// command runs through the shell, so ~ and environment variables expand.
const SettingPGPCommand = clearsign.SettingPGPCommand

// SettingHorizonSeedKey names the setting holding the public key a clone
// verifies its origin's structural horizon seeds with
// (draft-fossil-horizon-00 §5.2.4): an SSH public key line, verified in
// process, or the fingerprint of an OpenPGP key in the user's gpg keyring,
// verified by gpg. With a key set, a seed that is unsigned or not signed by
// that key is discarded and the clone falls back to the content horizon;
// unset, an unsigned seed is accepted. An SSH signature must carry the
// namespace fossil-horizon-seed. CloneOpts.SeedKey sets it when the clone is
// made. It is the clone's own: an origin never sends it.
//
// SSH signatures are verified only in a program that imports package
// github.com/danmestas/go-libfossil/sshverify; without it, a seed under an
// SSH key is refused with ErrSSHVerifierUnavailable.
const SettingHorizonSeedKey = clearsign.SettingSeedKey

// ErrSSHVerifierUnavailable is the error, wrapped, of an operation that had
// to verify an SSH signature in a program that does not link the verifier:
// a sync or rebuild of a store whose SettingHorizonSeedKey is an SSH key
// fails with it, and the seed is neither accepted nor discarded. Import
// package github.com/danmestas/go-libfossil/sshverify for its side effect
// to verify SSH signatures; the library leaves that cryptography out of
// programs that never verify one.
var ErrSSHVerifierUnavailable = clearsign.ErrSSHVerifierUnavailable

// SettingHorizonSeedCommand names the command an origin signs the
// structural horizon seeds it mints with (draft-fossil-horizon-00 §5.2.4),
// configured apart from SettingClearsign and SettingPGPCommand: unset or
// off, seeds go out unsigned even when check-ins are signed. It follows
// pgp-command's contract, and an SSH signer must sign in the namespace
// fossil-horizon-seed, never fossilscm, so that a check-in's signature
// cannot stand in for a seed's:
//
//	ssh-keygen -q -Y sign -n fossil-horizon-seed -f ~/.ssh/id_ed25519
//
// An origin whose command signs in another namespace serves no seed: the
// request fails.
const SettingHorizonSeedCommand = clearsign.SettingSeedCommand
