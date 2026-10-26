// Package sshverify links in-process verification of SSH signatures into a
// program that uses go-libfossil. Import it for its side effect:
//
//	import _ "github.com/danmestas/go-libfossil/sshverify"
//
// Without it, the library links no SSH cryptography, and a signature it
// would have to check against an SSH public key, such as a structural
// horizon seed under an SSH libfossil.SettingHorizonSeedKey, is refused with
// libfossil.ErrSSHVerifierUnavailable. Signing needs no import: it runs
// ssh-keygen. PGP signatures are verified by gpg either way.
package sshverify

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/danmestas/go-libfossil/internal/clearsign"
)

func init() {
	clearsign.RegisterSSHVerifier(verifier{})
}

type verifier struct{}

// CheckKey returns an error unless authorized is exactly one SSH public key.
func (verifier) CheckKey(authorized string) error {
	_, err := parseKey(authorized)
	return err
}

// Verify checks sig against message under the key authorized names.
func (verifier) Verify(authorized string, sig clearsign.SSHSignature, message []byte) error {
	want, err := parseKey(authorized)
	if err != nil {
		return err
	}
	if !bytes.Equal(sig.PublicKey, want.Marshal()) {
		return fmt.Errorf("%w: signed by another key", clearsign.ErrBadSignature)
	}
	s := &ssh.Signature{Format: sig.Format, Blob: sig.Blob, Rest: sig.Rest}
	if err := want.Verify(message, s); err != nil {
		return fmt.Errorf("%w: %v", clearsign.ErrBadSignature, err)
	}
	return nil
}

func parseKey(authorized string) (ssh.PublicKey, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("more than one key")
	}
	return key, nil
}
