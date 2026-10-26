//go:build !js && !windows

package clearsign

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestSSHVerifyWithoutVerifierFailsClosed: this test binary links no SSH
// verifier, so a well-formed SSH signature by the right key cannot be
// judged. Verify must say so, as ErrCannotVerify, never pass it, and never
// call it bad; an unsigned artifact is still ErrUnsigned.
func TestSSHVerifyWithoutVerifierFailsClosed(t *testing.T) {
	if sshVerifier != nil {
		t.Fatal("an SSH verifier is registered in clearsign's own tests")
	}
	for _, namespace := range []string{SSHNamespace, SeedNamespace} {
		t.Run(namespace, func(t *testing.T) {
			checkin, pub := sshKey(t, "ed25519")
			signer := &Signer{Command: strings.Replace(checkin.Command, "-n "+SSHNamespace,
				"-n "+namespace, 1), Namespace: namespace}
			signed := sign(t, signer, manifest)
			key := mustKey(t, pub)
			err := key.Verify(context.Background(), namespace, signed)
			if !errors.Is(err, ErrSSHVerifierUnavailable) || !errors.Is(err, ErrCannotVerify) ||
				errors.Is(err, ErrBadSignature) {
				t.Fatalf("Verify with no verifier = %v, want ErrSSHVerifierUnavailable", err)
			}
			if !strings.Contains(err.Error(), "go-libfossil/sshverify") {
				t.Errorf("error %q does not name the package to import", err)
			}
			if err := key.Verify(context.Background(), namespace, manifest); err != ErrUnsigned {
				t.Fatalf("unsigned: Verify = %v, want ErrUnsigned", err)
			}
		})
	}
}

// TestSSHVerifyWithoutVerifierStillRefusesTheMalformed: what needs no
// cryptography to refuse (another namespace, a broken envelope) is refused
// as bad whether or not a verifier is linked.
func TestSSHVerifyWithoutVerifierStillRefusesTheMalformed(t *testing.T) {
	signer, pub := sshKey(t, "ed25519")
	key := mustKey(t, pub)
	signed := sign(t, signer, manifest)
	truncated := signed[:len(signed)-len(sshSigEnd)]
	for name, c := range map[string]struct {
		namespace string
		signed    []byte
	}{
		"namespace": {SeedNamespace, signed},
		"armor":     {SSHNamespace, truncated},
	} {
		err := key.Verify(context.Background(), c.namespace, c.signed)
		if !errors.Is(err, ErrBadSignature) || errors.Is(err, ErrCannotVerify) {
			t.Errorf("%s: Verify = %v, want ErrBadSignature", name, err)
		}
	}
}

// TestParseKeyWithoutVerifier: an SSH key line is still read, and refused
// when it is not one key, with no verifier linked.
func TestParseKeyWithoutVerifier(t *testing.T) {
	_, pub := sshKey(t, "ecdsa")
	if k, err := ParseKey(pub); err != nil || k.String() != pub {
		t.Fatalf("ParseKey(%q) = %v, %v", pub, k, err)
	}
	fields := strings.Fields(pub)
	for _, s := range []string{
		pub + "\n" + pub,
		"ssh-rsa " + fields[1],
		fields[0],
		"ssh-ed25519 AAAA",
	} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) succeeded", s)
		}
	}
}
