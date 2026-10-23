package libfossil

import "github.com/danmestas/go-libfossil/internal/clearsign"

// SettingClearsign names the boolean repository setting, read as fossil reads
// one, that signs every check-in a commit or a branch creation stores, as
// fossil and zit sign them, and every horizon seed an origin mints
// (draft-fossil-horizon-00 §5.2.4). Off by default. The signed text is the
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
// unset, an unsigned seed is accepted. CloneOpts.SeedKey sets it when the
// clone is made. It is the clone's own: an origin never sends it.
const SettingHorizonSeedKey = clearsign.SettingSeedKey
