//go:build !js && !windows

package clearsign

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/danmestas/go-libfossil/internal/clearsign/signtest"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

// manifest is a control artifact as a seed is written: cards, then Z.
var manifest = []byte("D 2026-10-23T12:00:00\n" +
	"T +branch 0123456789abcdef0123456789abcdef01234567 trunk\n" +
	"U go-libfossil\n" +
	"Z 00000000000000000000000000000000\n")

// sshKey makes an ssh-keygen key of type kind and returns its signer, in
// SSHNamespace, and its public key line.
func sshKey(t *testing.T, kind string) (*Signer, string) {
	command, pub := signtest.SSHKey(t, kind, SSHNamespace)
	return &Signer{Command: command}, pub
}

// gpgKey makes a gpg signing key and returns its signer and fingerprint.
func gpgKey(t *testing.T) (*Signer, string) {
	command, fpr := signtest.GPGKey(t)
	return &Signer{Command: command}, fpr
}

func mustKey(t *testing.T, s string) Key {
	t.Helper()
	k, err := ParseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, s *Signer, plain []byte) []byte {
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

// TestPGPSignVerify signs with gpg --clearsign, fossil's default command,
// and verifies through gpg against the fingerprint.
func TestPGPSignVerify(t *testing.T) {
	signer, fpr := gpgKey(t)
	signed := sign(t, signer, manifest)
	if !bytes.HasPrefix(signed, pgpHeader) {
		t.Fatalf("not a PGP clear-signed message:\n%s", signed)
	}
	if err := mustKey(t, fpr).Verify(context.Background(), SSHNamespace, signed); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	spaced := ""
	for i := 0; i < len(fpr); i += 4 {
		spaced += strings.ToLower(fpr[i:i+4]) + " "
	}
	if err := mustKey(t, spaced).Verify(context.Background(), SSHNamespace, signed); err != nil {
		t.Fatalf("Verify with a spaced lower-case fingerprint: %v", err)
	}
}

// TestPGPVerifyRefuses covers trailing white space, which a clear-signature
// ignores but which changes the artifact and its name; a changed card; a
// fingerprint of another key; and an SSH key given for a PGP signature.
func TestPGPVerifyRefuses(t *testing.T) {
	signer, fpr := gpgKey(t)
	signed := sign(t, signer, manifest)
	trailing := bytes.Replace(signed, []byte("U go-libfossil\n"), []byte("U go-libfossil  \n"), 1)
	changed := bytes.Replace(signed, []byte("trunk"), []byte("trunc"), 1)
	_, sshPub := sshKey(t, "ed25519")
	cases := []struct {
		name   string
		key    Key
		signed []byte
	}{
		{"trailing white space", mustKey(t, fpr), trailing},
		{"changed card", mustKey(t, fpr), changed},
		{"other fingerprint", mustKey(t, strings.Repeat("AB", 20)), signed},
		{"ssh key", mustKey(t, sshPub), signed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.key.Verify(context.Background(), SSHNamespace, c.signed); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("Verify = %v, want ErrBadSignature", err)
			}
		})
	}
}

// TestSignRefusesUnsignedOutput: a command that copies its input instead of
// signing it, or that fails, signs nothing.
func TestSignRefusesUnsignedOutput(t *testing.T) {
	for _, command := range []string{"cp_out() { cp \"$2\" \"$1\"; }; cp_out", "false"} {
		if _, err := (&Signer{Command: command}).Sign(context.Background(), manifest); err == nil {
			t.Errorf("Sign with %q succeeded", command)
		}
	}
}

func TestParseKey(t *testing.T) {
	_, pub := sshKey(t, "ed25519")
	for _, s := range []string{pub, strings.Repeat("0a", 20), strings.Repeat("0A", 32)} {
		if _, err := ParseKey(s); err != nil {
			t.Errorf("ParseKey(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", "ssh-ed25519 notbase64", strings.Repeat("0g", 20), strings.Repeat("0a", 19)} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) succeeded", s)
		}
	}
}

func TestIsSSHCommand(t *testing.T) {
	for command, want := range map[string]bool{
		"ssh-keygen -Y sign -n fossilscm -f k": true,
		"/usr/bin/ssh-keygen -Y sign":          true,
		"gpg --clearsign -o ":                  false,
		"/opt/sshtools/gpg -o":                 false,
	} {
		if got := isSSHCommand(command); got != want {
			t.Errorf("isSSHCommand(%q) = %v, want %v", command, got, want)
		}
	}
}

// TestPGPVerifyRefusesEnvelopes covers what a clear-signature tolerates but
// the artifact parser does not read the same way: bytes after the signature,
// CRLF line ends, and a second signature. Each must fail, and a changed
// line must fail on the text comparison, not by chance.
func TestPGPVerifyRefusesEnvelopes(t *testing.T) {
	signer, fpr := gpgKey(t)
	key := mustKey(t, fpr)
	signed := sign(t, signer, manifest)
	gen := exec.Command("gpg", "--batch", "--quiet", "--pinentry-mode", "loopback", "--passphrase", "",
		"--quick-gen-key", "Second <second@example.org>", "ed25519", "sign", "never")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("second key: %v: %s", err, out)
	}
	twice := sign(t, &Signer{Command: "gpg --batch --pinentry-mode loopback --passphrase '' " +
		"-u " + fpr + " -u Second --clearsign -o"}, manifest)
	cases := map[string][]byte{
		"after the signature": append(append([]byte(nil), signed...), "C extra\n"...),
		"CRLF":                bytes.ReplaceAll(signed, []byte("\n"), []byte("\r\n")),
		"two signatures":      twice,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := key.Verify(context.Background(), SSHNamespace, data); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("Verify = %v, want ErrBadSignature", err)
			}
		})
	}
	trailing := bytes.Replace(signed, []byte("U go-libfossil\n"), []byte("U go-libfossil  \n"), 1)
	if err := key.Verify(context.Background(), SSHNamespace, trailing); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("trailing white space: Verify = %v, want the signed text to differ", err)
	}
}

// TestPGPCannotVerify: with no gpg to run, Verify says it could not judge
// the signature, not that it is bad.
func TestPGPCannotVerify(t *testing.T) {
	signer, fpr := gpgKey(t)
	signed := sign(t, signer, manifest)
	t.Setenv("PATH", t.TempDir())
	err := mustKey(t, fpr).Verify(context.Background(), SSHNamespace, signed)
	if !errors.Is(err, ErrCannotVerify) || errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify without gpg = %v, want ErrCannotVerify alone", err)
	}
}

// TestSignRefusesDashEscaping: gpg dash-escapes a line that starts with a
// dash, so the signed text would not strip back to the artifact.
func TestSignRefusesDashEscaping(t *testing.T) {
	signer, _ := gpgKey(t)
	if _, err := signer.Sign(context.Background(), []byte("-dash\n")); err == nil {
		t.Fatal("Sign accepted a dash-escaped artifact")
	}
}

// TestSSHVerifyRefusesSHA1RSA: an SSHSIG made with the SHA-1 ssh-rsa
// algorithm, which PROTOCOL.sshsig forbids, does not verify even when the
// RSA key and bytes are right.
func TestSSHVerifyRefusesSHA1RSA(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum512(manifest)
	tail := ssh.Marshal(struct {
		Namespace, Reserved, Hash string
		Digest                    []byte
	}{SSHNamespace, "", "sha512", digest[:]})
	sig, err := signer.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, append([]byte("SSHSIG"), tail...), ssh.KeyAlgoRSA)
	if err != nil {
		t.Fatal(err)
	}
	blob := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Version   uint32
		PublicKey []byte
		Namespace string
		Reserved  string
		Hash      string
		Signature []byte
	}{1, signer.PublicKey().Marshal(), SSHNamespace, "", "sha512", ssh.Marshal(sig)})...)
	armor := "-----BEGIN SSH SIGNATURE-----\n" + base64.StdEncoding.EncodeToString(blob) + "\n-----END SSH SIGNATURE-----\n"
	signed := append(append(append([]byte(nil), sshHeader...), manifest...), armor...)
	key := mustKey(t, string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if err := key.Verify(context.Background(), SSHNamespace, signed); !errors.Is(err, ErrBadSignature) ||
		!strings.Contains(err.Error(), "SHA-1") {
		t.Fatalf("Verify = %v, want the SHA-1 signature refused", err)
	}
}

// TestSignRefusesAnotherNamespace: a signer bound to a namespace refuses an
// SSH signature its command made under another, so an origin whose seed
// command names fossilscm sends no seed a clone could only refuse.
func TestSignRefusesAnotherNamespace(t *testing.T) {
	checkin, _ := sshKey(t, "ed25519")
	wrong := &Signer{Command: checkin.Command, Namespace: SeedNamespace}
	if _, err := wrong.Sign(context.Background(), manifest); err == nil || !strings.Contains(err.Error(), SeedNamespace) {
		t.Fatalf("Sign under %s for a %s signer = %v, want a refusal naming %s", SSHNamespace, SeedNamespace, err, SeedNamespace)
	}
	right := &Signer{Command: strings.Replace(checkin.Command, "-n "+SSHNamespace, "-n "+SeedNamespace, 1), Namespace: SeedNamespace}
	sign(t, right, manifest)
}

// TestSeedSignerIsItsOwnSetting: an origin's seed-signing command is
// configured apart from its check-in signing (§5.2.4): clearsign and
// pgp-command sign no seed, and the seed command signs whatever they say.
func TestSeedSignerIsItsOwnSetting(t *testing.T) {
	r, err := repo.Create(filepath.Join(t.TempDir(), "origin.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	set := func(name, value string) {
		t.Helper()
		if err := r.SetConfig(name, value); err != nil {
			t.Fatal(err)
		}
	}
	seedSigner := func() *Signer {
		t.Helper()
		s, err := SeedSigner(r.DB())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	set(SettingClearsign, "on")
	set(SettingPGPCommand, "gpg --clearsign -o")
	if s := seedSigner(); s != nil {
		t.Fatalf("check-in signing alone gives seed signer %+v, want none", s)
	}
	const command = "ssh-keygen -q -Y sign -n fossil-horizon-seed -f k"
	set(SettingSeedCommand, command)
	set(SettingClearsign, "off")
	if s := seedSigner(); s == nil || s.Command != command || s.Namespace != SeedNamespace {
		t.Fatalf("seed signer %+v, want %q in namespace %s", s, command, SeedNamespace)
	}
	set(SettingSeedCommand, "off")
	if s := seedSigner(); s != nil {
		t.Fatalf("seed command off gives signer %+v", s)
	}
}
