package clearsign

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// ErrSSHVerifierUnavailable is returned by Verify for an SSH signature when
// no SSHVerifier is registered. It wraps ErrCannotVerify: the signature was
// not judged, so it is neither accepted nor taken for a bad one.
var ErrSSHVerifierUnavailable = fmt.Errorf(
	"%w: SSH signature verification is not linked; import _ %q to enable it",
	ErrCannotVerify, "github.com/danmestas/go-libfossil/sshverify")

// SSHSignature is an SSHSIG signature (PROTOCOL.sshsig) taken apart: the
// wire-form public key that made it, and the signature's format, blob and
// any fields after them.
type SSHSignature struct {
	PublicKey []byte
	Format    string
	Blob      []byte
	Rest      []byte
}

// SSHVerifier does the cryptography of SSH signature checks, which most
// programs never need and so do not link: package sshverify registers one.
type SSHVerifier interface {
	// CheckKey returns an error unless authorized is exactly one SSH public
	// key in authorized_keys form.
	CheckKey(authorized string) error
	// Verify returns an error wrapping ErrBadSignature unless sig was made
	// by the key authorized names and signs message.
	Verify(authorized string, sig SSHSignature, message []byte) error
}

// sshVerifier is set once, from the registering package's init, and only
// read after.
var sshVerifier SSHVerifier

// RegisterSSHVerifier makes v the verifier of SSH signatures. It must be
// called once, from an init function; a second call panics.
func RegisterSSHVerifier(v SSHVerifier) {
	if v == nil {
		panic("clearsign.RegisterSSHVerifier: nil verifier")
	}
	if sshVerifier != nil {
		panic("clearsign.RegisterSSHVerifier: a verifier is already registered")
	}
	sshVerifier = v
}

// verifySSH checks fossil's SSH envelope (src/clearsign.c): the header, a
// blank line, the artifact verbatim, then the armored detached signature
// ssh-keygen -Y sign wrote over it. Fossil's layout is the only one
// accepted, so the signed message is exactly what a reader parses. The
// signature must carry namespace. What needs no cryptography is checked
// here, so a malformed signature is bad whether or not a verifier is linked.
func verifySSH(authorized, namespace string, signed []byte) error {
	body := deck.StripClearsign(signed)
	rest, ok := bytes.CutPrefix(signed, sshHeader)
	if !ok || !bytes.HasPrefix(rest, body) {
		return fmt.Errorf("%w: not fossil's SSH envelope", ErrBadSignature)
	}
	blob, err := dearmorSSH(rest[len(body):])
	if err != nil {
		return err
	}
	sig, err := parseSSHSig(blob)
	if err != nil {
		return err
	}
	// PROTOCOL.sshsig forbids the SHA-1 "ssh-rsa" signature algorithm; an RSA
	// key signs with rsa-sha2-256 or rsa-sha2-512.
	if sig.signature.Format == "ssh-rsa" {
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
	if sshVerifier == nil {
		return ErrSSHVerifierUnavailable
	}
	// PROTOCOL.sshsig: the signature covers the preamble, namespace,
	// reserved field, hash algorithm and digest, each but the first an SSH
	// string.
	message := []byte("SSHSIG")
	for _, field := range []string{sig.namespace, sig.reserved, sig.hash, string(digest)} {
		message = appendSSHString(message, []byte(field))
	}
	return sshVerifier.Verify(authorized, sig.signature, message)
}

// checkSSHKey returns an error unless authorized is one SSH public key: the
// registered verifier's judgement, or with none a reading of the line's
// form, since a key no verifier can use need only be kept.
func checkSSHKey(authorized string) error {
	if sshVerifier != nil {
		return sshVerifier.CheckKey(authorized)
	}
	line := strings.TrimSpace(authorized)
	if strings.ContainsAny(line, "\r\n") {
		return errors.New("more than one key")
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return errors.New("no key type and key")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return err
	}
	if kind, _, ok := cutSSHString(blob); !ok || string(kind) != fields[0] {
		return fmt.Errorf("the key is not of type %q", fields[0])
	}
	return nil
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
	namespace, reserved, hash string
	signature                 SSHSignature
}

// parseSSHSig reads the SSHSIG blob of PROTOCOL.sshsig: the six-byte
// preamble, version 1, then the public key, namespace, reserved field, hash
// algorithm and signature as SSH strings, and nothing after. The signature
// is its format and blob as SSH strings, then whatever follows them.
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
	rest = rest[4:]
	var fields [5][]byte // public key, namespace, reserved, hash, signature
	for i := range fields {
		if fields[i], rest, ok = cutSSHString(rest); !ok {
			return bad("truncated")
		}
	}
	if len(rest) != 0 {
		return bad("bytes after the signature")
	}
	format, rest, ok := cutSSHString(fields[4])
	var sigBlob []byte
	if ok {
		sigBlob, rest, ok = cutSSHString(rest)
	}
	if !ok {
		return bad("truncated signature")
	}
	return sshSig{
		namespace: string(fields[1]),
		reserved:  string(fields[2]),
		hash:      string(fields[3]),
		signature: SSHSignature{
			PublicKey: fields[0], Format: string(format), Blob: sigBlob, Rest: rest,
		},
	}, nil
}

// cutSSHString splits the SSH string (RFC 4251 §5) b opens with off it.
func cutSSHString(b []byte) (s, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, b, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, b, false
	}
	return b[4 : 4+n], b[4+n:], true
}

// appendSSHString appends s to b as an SSH string.
func appendSSHString(b, s []byte) []byte {
	return append(binary.BigEndian.AppendUint32(b, uint32(len(s))), s...)
}
