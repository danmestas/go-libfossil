package clearsign

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// verifySSH checks fossil's SSH envelope (src/clearsign.c): the header, a
// blank line, the artifact verbatim, then the armored detached signature
// ssh-keygen -Y sign wrote over it. Fossil's layout is the only one
// accepted, so the signed message is exactly what a reader parses. The
// signature must carry namespace.
func verifySSH(authorized, namespace string, signed []byte) error {
	want, err := parseSSHKey(authorized)
	if err != nil {
		return err
	}
	body := deck.StripClearsign(signed)
	rest, ok := bytes.CutPrefix(signed, sshHeader)
	if !ok || !bytes.HasPrefix(rest, body) {
		return fmt.Errorf("%w: not fossil's SSH envelope", ErrBadSignature)
	}
	armor := rest[len(body):]
	blob, err := dearmorSSH(armor)
	if err != nil {
		return err
	}
	sig, err := parseSSHSig(blob)
	if err != nil {
		return err
	}
	if !bytes.Equal(sig.publicKey, want.Marshal()) {
		return fmt.Errorf("%w: signed by another key", ErrBadSignature)
	}
	// PROTOCOL.sshsig forbids the SHA-1 "ssh-rsa" signature algorithm; an RSA
	// key signs with rsa-sha2-256 or rsa-sha2-512.
	if sig.signature.Format == ssh.KeyAlgoRSA {
		return fmt.Errorf("%w: SHA-1 ssh-rsa signature", ErrBadSignature)
	}
	if sig.namespace != namespace {
		return fmt.Errorf("%w: namespace %q, want %q", ErrBadSignature, sig.namespace, namespace)
	}
	var digest []byte
	switch sig.hash {
	case "sha256":
		d := sha256.Sum256(body)
		digest = d[:]
	case "sha512":
		d := sha512.Sum512(body)
		digest = d[:]
	default:
		return fmt.Errorf("%w: hash algorithm %q", ErrBadSignature, sig.hash)
	}
	// PROTOCOL.sshsig: the signature covers the preamble, namespace,
	// reserved field, hash algorithm and digest, each but the first an SSH
	// string.
	message := ssh.Marshal(struct {
		Namespace, Reserved, Hash string
		Digest                    []byte
	}{sig.namespace, sig.reserved, sig.hash, digest})
	message = append([]byte("SSHSIG"), message...)
	if err := want.Verify(message, sig.signature); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	return nil
}

func parseSSHKey(authorized string) (ssh.PublicKey, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("more than one key")
	}
	return key, nil
}

// dearmorSSH decodes "-----BEGIN SSH SIGNATURE-----", base64 lines, and
// "-----END SSH SIGNATURE-----", with nothing after.
func dearmorSSH(armor []byte) ([]byte, error) {
	inner, ok := bytes.CutPrefix(armor, sshSigBegin)
	if ok {
		inner, ok = bytes.CutSuffix(inner, sshSigEnd)
	}
	if !ok {
		return nil, fmt.Errorf("%w: malformed SSH signature armor", ErrBadSignature)
	}
	blob, err := base64.StdEncoding.DecodeString(string(bytes.ReplaceAll(inner, []byte("\n"), nil)))
	if err != nil {
		return nil, fmt.Errorf("%w: SSH signature armor: %v", ErrBadSignature, err)
	}
	return blob, nil
}

type sshSig struct {
	publicKey                 []byte
	namespace, reserved, hash string
	signature                 *ssh.Signature
}

// parseSSHSig reads the SSHSIG blob of PROTOCOL.sshsig: the six-byte
// preamble, version 1, then the public key, namespace, reserved field, hash
// algorithm and signature as SSH strings.
func parseSSHSig(blob []byte) (sshSig, error) {
	bad := func(what string) (sshSig, error) {
		return sshSig{}, fmt.Errorf("%w: SSH signature: %s", ErrBadSignature, what)
	}
	rest, ok := bytes.CutPrefix(blob, []byte("SSHSIG"))
	if !ok || len(rest) < 4 {
		return bad("no SSHSIG preamble")
	}
	if v := binary.BigEndian.Uint32(rest); v != 1 {
		return bad(fmt.Sprintf("version %d", v))
	}
	var fields struct {
		PublicKey []byte
		Namespace string
		Reserved  string
		Hash      string
		Signature []byte
	}
	if err := ssh.Unmarshal(rest[4:], &fields); err != nil {
		return bad(err.Error())
	}
	var sig struct {
		Format string
		Blob   []byte
		Rest   []byte `ssh:"rest"`
	}
	if err := ssh.Unmarshal(fields.Signature, &sig); err != nil {
		return bad(err.Error())
	}
	return sshSig{
		publicKey: fields.PublicKey,
		namespace: fields.Namespace,
		reserved:  fields.Reserved,
		hash:      fields.Hash,
		signature: &ssh.Signature{Format: sig.Format, Blob: sig.Blob, Rest: sig.Rest},
	}, nil
}
