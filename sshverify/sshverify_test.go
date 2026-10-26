//go:build !js && !windows

package sshverify_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/clearsign/signtest"
	"github.com/danmestas/go-libfossil/internal/deck"
	_ "github.com/danmestas/go-libfossil/sshverify"
)

// manifest is a control artifact as a seed is written: cards, then Z.
var manifest = []byte("D 2026-10-23T12:00:00\n" +
	"T +branch 0123456789abcdef0123456789abcdef01234567 trunk\n" +
	"U go-libfossil\n" +
	"Z 00000000000000000000000000000000\n")

const (
	sshHeader   = "-----BEGIN SSH SIGNED MESSAGE-----\n\n"
	sshSigBegin = "-----BEGIN SSH SIGNATURE-----\n"
)

// sshKey makes an ssh-keygen key of type kind and returns its signer, in
// clearsign.SSHNamespace, and its public key line.
func sshKey(t *testing.T, kind string) (*clearsign.Signer, string) {
	command, pub := signtest.SSHKey(t, kind, clearsign.SSHNamespace)
	return &clearsign.Signer{Command: command}, pub
}

func mustKey(t *testing.T, s string) clearsign.Key {
	t.Helper()
	k, err := clearsign.ParseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, s *clearsign.Signer, plain []byte) []byte {
	t.Helper()
	signed, err := s.Sign(context.Background(), plain)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !bytes.Equal(deck.StripClearsign(signed), plain) {
		t.Fatalf("signed artifact does not strip back to its manifest:\n%s", signed)
	}
	return signed
}

// TestSSHSignVerify signs with ssh-keygen through the pgp-command contract
// and verifies in process, for each key type ssh-keygen signs with.
func TestSSHSignVerify(t *testing.T) {
	for _, kind := range []string{"ed25519", "ecdsa", "rsa"} {
		t.Run(kind, func(t *testing.T) {
			signer, pub := sshKey(t, kind)
			signed := sign(t, signer, manifest)
			if !bytes.HasPrefix(signed, []byte(sshHeader)) {
				t.Fatalf("not fossil's SSH envelope:\n%s", signed)
			}
			err := mustKey(t, pub).Verify(context.Background(), clearsign.SSHNamespace, signed)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

// TestSSHVerifyRefuses covers each way an SSH-signed artifact must fail:
// another key, another namespace, a changed body, a changed signature, and
// no signature at all.
func TestSSHVerifyRefuses(t *testing.T) {
	signer, pub := sshKey(t, "ed25519")
	_, otherPub := sshKey(t, "ed25519")
	key := mustKey(t, pub)
	signed := sign(t, signer, manifest)
	git := &clearsign.Signer{Command: strings.Replace(signer.Command,
		"-n "+clearsign.SSHNamespace, "-n git", 1)}
	wrongNamespace := sign(t, git, manifest)
	changed := bytes.Replace(signed, []byte("trunk"), []byte("trunc"), 1)
	sigAt := bytes.Index(signed, []byte(sshSigBegin)) + len(sshSigBegin) + 40
	flipped := append([]byte(nil), signed...)
	flipped[sigAt] ^= 'A' ^ 'B'
	cases := []struct {
		name   string
		key    clearsign.Key
		signed []byte
		want   error
	}{
		{"other key", mustKey(t, otherPub), signed, clearsign.ErrBadSignature},
		{"namespace", key, wrongNamespace, clearsign.ErrBadSignature},
		{"body", key, changed, clearsign.ErrBadSignature},
		{"signature", key, flipped, clearsign.ErrBadSignature},
		{"unsigned", key, manifest, clearsign.ErrUnsigned},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.key.Verify(context.Background(), clearsign.SSHNamespace, c.signed)
			if !errors.Is(err, c.want) {
				t.Fatalf("Verify = %v, want %v", err, c.want)
			}
		})
	}
}

// TestSSHNamespaceIsTheVerifiers: the namespace an SSH signature must carry
// is the verifier's parameter, so a check-in's fossilscm signature never
// verifies as a seed's, nor a seed's as a check-in's
// (draft-fossil-horizon-00 §5.2.4).
func TestSSHNamespaceIsTheVerifiers(t *testing.T) {
	checkin, pub := sshKey(t, "ed25519")
	key := mustKey(t, pub)
	seedSigner := &clearsign.Signer{Command: strings.Replace(checkin.Command,
		"-n "+clearsign.SSHNamespace, "-n "+clearsign.SeedNamespace, 1)}
	byCheckin, bySeed := sign(t, checkin, manifest), sign(t, seedSigner, manifest)
	for _, c := range []struct {
		name, namespace string
		signed          []byte
		ok              bool
	}{
		{"check-in as check-in", clearsign.SSHNamespace, byCheckin, true},
		{"seed as seed", clearsign.SeedNamespace, bySeed, true},
		{"check-in as seed", clearsign.SeedNamespace, byCheckin, false},
		{"seed as check-in", clearsign.SSHNamespace, bySeed, false},
	} {
		err := key.Verify(context.Background(), c.namespace, c.signed)
		if c.ok && err != nil || !c.ok && !errors.Is(err, clearsign.ErrBadSignature) {
			t.Errorf("%s: Verify = %v, want ok %v", c.name, err, c.ok)
		}
	}
}

// TestParseKeyChecksTheKey: with the verifier linked, ParseKey refuses a
// line of the right form whose key is no key, as it did before verification
// became opt-in.
func TestParseKeyChecksTheKey(t *testing.T) {
	kind := []byte("ssh-ed25519")
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(kind)))
	blob = append(blob, kind...)
	blob = binary.BigEndian.AppendUint32(blob, 31)
	blob = append(blob, make([]byte, 31)...)
	short := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
	if _, err := clearsign.ParseKey(short); err == nil {
		t.Fatalf("ParseKey(%q) accepted a 31-byte Ed25519 key", short)
	}
}

// TestRegisterTwicePanics: importing this package registered the verifier;
// another registration is a programming error.
func TestRegisterTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a second RegisterSSHVerifier did not panic")
		}
	}()
	clearsign.RegisterSSHVerifier(stub{})
}

type stub struct{}

func (stub) CheckKey(string) error                               { return nil }
func (stub) Verify(string, clearsign.SSHSignature, []byte) error { return nil }
