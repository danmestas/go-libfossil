// Package clearsign signs artifacts and verifies their signatures, in the
// clear-signing envelopes of draft-fossil-artifact-format-00 §4.1.1: a PGP
// clear-signed message, or the SSH envelope fossil assembles around a
// detached ssh-keygen signature.
//
// Signing runs the repository's pgp-command as fossil and zit run it, so one
// setting signs with either tool. Verification takes the signer's key: an SSH
// public key is checked in process, by the SSHVerifier package sshverify
// registers, so a program that never verifies one does not link SSH
// cryptography; a PGP fingerprint is checked by gpg against the user's
// keyring, since an OpenPGP implementation is not worth a dependency when
// every PGP signer already has gpg.
package clearsign

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// Settings, named as fossil names them. Clearsign turns signing on (fossil
// db_get_boolean, default off) and fossil's legacy Omitsign turns it off
// again; PGPCommand names the signer, defaulting to DefaultCommand. SeedKey
// and SeedCommand are go-libfossil's (draft-fossil-horizon-00 §5.2.4): the
// key a clone verifies its origin's horizon seeds with, and the command an
// origin signs its seeds with, configured apart from check-in signing.
const (
	SettingClearsign   = "clearsign"
	SettingOmitsign    = "omitsign"
	SettingPGPCommand  = "pgp-command"
	SettingSeedKey     = "horizon-seed-key"
	SettingSeedCommand = "horizon-seed-command"
	DefaultCommand     = "gpg --clearsign -o "
)

// SSHNamespace is the ssh-keygen -n namespace a check-in's signature must
// carry. It is the one fossil's documentation gives (www/signing.md), so an
// origin set up by that recipe verifies.
const SSHNamespace = "fossilscm"

// SeedNamespace is the namespace a horizon seed's SSH signature carries,
// never SSHNamespace, so that a signature on a check-in or tag cannot stand
// in for one (draft-fossil-horizon-00 §5.2.4).
const SeedNamespace = "fossil-horizon-seed"

// maxSignedBytes bounds what a signer may hand back: a manifest plus armor,
// never a payload channel.
const maxSignedBytes = 8 << 20

// runTimeout bounds one signer or gpg run, which may wait on an agent.
const runTimeout = 2 * time.Minute

var (
	// ErrUnsigned is returned by Verify for an artifact with no envelope.
	ErrUnsigned = errors.New("clearsign: artifact is not signed")
	// ErrBadSignature is returned by Verify when the envelope does not
	// verify against the key, or covers other bytes than the artifact's.
	ErrBadSignature = errors.New("clearsign: signature does not verify")
	// ErrCannotVerify is returned by Verify when the verifier could not judge
	// the signature at all: gpg is missing, timed out, or could not run. It
	// says nothing about the artifact, so it is no reason to discard one.
	ErrCannotVerify = errors.New("clearsign: signature could not be checked")
)

var (
	pgpHeader    = []byte("-----BEGIN PGP SIGNED MESSAGE-----\n")
	sshHeader    = []byte("-----BEGIN SSH SIGNED MESSAGE-----\n\n")
	sshSigBegin  = []byte("-----BEGIN SSH SIGNATURE-----\n")
	sshSigEnd    = []byte("-----END SSH SIGNATURE-----\n")
	envelopeHead = [][]byte{pgpHeader[:34], sshHeader[:34]}
)

// Signer runs a signing command under fossil's pgp-command contract
// (src/clearsign.c): COMMAND OUTFILE INFILE writes a clear-signed INFILE to
// OUTFILE, except that a command whose name starts with "ssh" is run as
// COMMAND INFILE, writes a detached INFILE.sig, and the SSH envelope is
// assembled around it. Namespace, when set, is the one an SSH signature the
// command makes must carry; the command names it, so Sign checks it.
type Signer struct {
	Command   string
	Namespace string
}

// FromSettings returns the signer q's settings name, or nil when clearsign
// is off or the legacy omitsign is on: fossil signs only then, using
// pgp-command or its default (src/checkin.c, src/branch.c).
func FromSettings(q db.Querier) (*Signer, error) {
	if q == nil {
		panic("clearsign.FromSettings: q must not be nil")
	}
	on, err := setting(q, SettingClearsign)
	if err != nil || !repo.SettingBoolean(on, false) {
		return nil, err
	}
	omit, err := setting(q, SettingOmitsign)
	if err != nil || repo.SettingBoolean(omit, false) {
		return nil, err
	}
	command, err := setting(q, SettingPGPCommand)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(command) == "" {
		command = DefaultCommand
	}
	if falsy(command) {
		return nil, nil
	}
	return &Signer{Command: command}, nil
}

// SeedSigner returns the signer of an origin's horizon seeds, q's
// horizon-seed-command bound to SeedNamespace, or nil when it is unset or
// off: an origin with no seed-signing command sends seeds unsigned, even
// when it signs check-ins (draft-fossil-horizon-00 §5.2.4).
func SeedSigner(q db.Querier) (*Signer, error) {
	if q == nil {
		panic("clearsign.SeedSigner: q must not be nil")
	}
	command, err := setting(q, SettingSeedCommand)
	if err != nil || strings.TrimSpace(command) == "" || falsy(command) {
		return nil, err
	}
	return &Signer{Command: command, Namespace: SeedNamespace}, nil
}

// Sign returns plain wrapped in its envelope. The signer must not alter
// what it signs: the envelope's content is checked to strip back to plain,
// as zit checks it, so a dash-escaped or rewritten manifest is refused
// rather than stored under a name its cards do not match.
func (s *Signer) Sign(ctx context.Context, plain []byte) ([]byte, error) {
	if s == nil || s.Command == "" {
		panic("clearsign.Signer.Sign: no command")
	}
	if len(plain) == 0 || plain[len(plain)-1] != '\n' {
		panic("clearsign.Signer.Sign: plain must be a non-empty artifact ending in LF")
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	var signed []byte
	var err error
	if isSSHCommand(s.Command) {
		var sig []byte
		if sig, err = runDetached(ctx, s.Command, plain); err == nil {
			signed = append(append(append([]byte(nil), sshHeader...), plain...), sig...)
			err = s.checkNamespace(sig)
		}
	} else {
		signed, err = runClearsign(ctx, s.Command, plain)
	}
	if err != nil {
		return nil, fmt.Errorf("clearsign: %s: %w", s.Command, err)
	}
	if envelope(signed) == nil || !bytes.Equal(deck.StripClearsign(signed), plain) {
		return nil, fmt.Errorf("clearsign: %s did not return %d bytes clear-signed unchanged",
			s.Command, len(plain))
	}
	return signed, nil
}

// checkNamespace refuses an armored SSH signature made under another
// namespace than the signer's, when it has one.
func (s *Signer) checkNamespace(armor []byte) error {
	if s.Namespace == "" {
		return nil
	}
	blob, err := dearmorSSH(armor)
	if err != nil {
		return err
	}
	sig, err := parseSSHSig(blob)
	if err != nil {
		return err
	}
	if sig.namespace != s.Namespace {
		return fmt.Errorf("signed in namespace %q, want %q", sig.namespace, s.Namespace)
	}
	return nil
}

// Key is a signer's public identity: an SSH public key, or the fingerprint
// of an OpenPGP key in the user's gpg keyring.
type Key struct {
	ssh string // authorized_keys form, verified in process
	pgp string // fingerprint, upper-case hex, verified by gpg
}

// ParseKey reads an SSH public key line ("ssh-ed25519 AAAA… comment") or an
// OpenPGP fingerprint (40 or 64 hex digits, spaces allowed).
func ParseKey(s string) (Key, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Key{}, errors.New("clearsign: empty key")
	}
	if fpr, ok := fingerprint(s); ok {
		return Key{pgp: fpr}, nil
	}
	if err := checkSSHKey(s); err != nil {
		return Key{}, fmt.Errorf("clearsign: %q is neither an SSH public key nor an OpenPGP fingerprint: %w",
			s, err)
	}
	return Key{ssh: s}, nil
}

// String is the key as ParseKey reads it back.
func (k Key) String() string {
	if k.pgp != "" {
		return k.pgp
	}
	return k.ssh
}

// Verify checks that signed carries a valid signature by k over exactly the
// bytes a reader parses (deck.StripClearsign), and returns ErrUnsigned for
// an artifact with no envelope. An SSH signature must carry namespace:
// SSHNamespace for a check-in, SeedNamespace for a seed. PGP has none. A
// well-formed SSH signature with no SSHVerifier registered is
// ErrSSHVerifierUnavailable, never accepted.
func (k Key) Verify(ctx context.Context, namespace string, signed []byte) error {
	if k.ssh == "" && k.pgp == "" {
		panic("clearsign.Key.Verify: zero Key")
	}
	if namespace == "" {
		panic("clearsign.Key.Verify: empty namespace")
	}
	head := envelope(signed)
	if head == nil {
		return ErrUnsigned
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	if bytes.Equal(head, sshHeader[:34]) {
		if k.ssh == "" {
			return fmt.Errorf("%w: SSH-signed, but the key is an OpenPGP fingerprint", ErrBadSignature)
		}
		return verifySSH(k.ssh, namespace, signed)
	}
	if k.pgp == "" {
		return fmt.Errorf("%w: PGP-signed, but the key is an SSH key", ErrBadSignature)
	}
	return verifyPGP(ctx, k.pgp, signed)
}

// SeedKey returns the key q's horizon-seed-key setting names, or nil when
// none is set: a clone with no key for its origin accepts an unsigned seed
// (draft-fossil-horizon-00 §5.2.4).
func SeedKey(q db.Querier) (*Key, error) {
	if q == nil {
		panic("clearsign.SeedKey: q must not be nil")
	}
	value, err := setting(q, SettingSeedKey)
	if err != nil || strings.TrimSpace(value) == "" {
		return nil, err
	}
	k, err := ParseKey(value)
	if err != nil {
		return nil, fmt.Errorf("setting %s: %w", SettingSeedKey, err)
	}
	return &k, nil
}

// envelope returns the envelope header data opens with, or nil.
func envelope(data []byte) []byte {
	for _, h := range envelopeHead {
		if bytes.HasPrefix(data, h) {
			return h
		}
	}
	return nil
}

// isSSHCommand is fossil's test: the command word's base name starts with
// "ssh" (command_basename).
func isSSHCommand(command string) bool {
	word, _, _ := strings.Cut(strings.TrimSpace(command), " ")
	if i := strings.LastIndexAny(word, `/\`); i >= 0 {
		word = word[i+1:]
	}
	return strings.HasPrefix(word, "ssh")
}

// fingerprint normalizes s as an OpenPGP v4 or v5 fingerprint.
func fingerprint(s string) (string, bool) {
	hex := strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	if len(hex) != 40 && len(hex) != 64 {
		return "", false
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return hex, true
}

func setting(q db.Querier, name string) (string, error) {
	var value string
	err := q.QueryRow("SELECT CAST(value AS TEXT) FROM config WHERE name=?", name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("clearsign: setting %s: %w", name, err)
	}
	return value, nil
}

// falsy is fossil's is_false: the spellings that turn a command off.
func falsy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "no", "false", "0":
		return true
	}
	return false
}
