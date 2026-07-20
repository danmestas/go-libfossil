package xfer

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Message is a sequence of cards forming one xfer request or response.
type Message struct {
	Cards []Card
}

// MaxDecompressedBytes caps how far one message may expand during
// decompression. It bounds the expansion only. The compressed body itself
// arrives through the transport, which reads it whole before calling Decode
// (sync.HTTPTransport.Exchange), so this is not a bound on the memory a
// hostile peer can cost the process -- it stops a small body from inflating
// without limit, nothing more. Bounding the body on the way in belongs to the
// transport and is not done today.
//
// It is a local resource guard, not a protocol rule: §4.1 places no limit on
// the decompressed size of the compressed container. This implementation is
// deliberately stricter than canonical here, which sizes its output buffer
// from the attacker-supplied length prefix (blob.c blob_uncompress); the bound
// below counts bytes actually inflated and ignores the advertised length.
//
// Because it is a guard and not a rule, it has to sit above anything a
// conforming peer will send, or a legitimate exchange fails -- which is how
// issue #104 arose, with the bound below what this implementation's own server
// emitted in one clone round. Two constraints fix the range:
//
//   - Above: a clone round carries sync.DefaultCloneBatchBytes of expanded
//     content plus the whole artifact that crossed that budget, so the bound
//     must clear the budget twice over. Enforced at compile time in
//     internal/sync.
//   - Below: it must stay under zlibCMFAliasBytes, or the §4.1 length prefix
//     of a legitimate container starts to present as a zlib header. Enforced
//     at compile time immediately below.
//
// Consequence worth naming: a single artifact larger than this bound cannot be
// cloned, because the server has to send it whole to make progress.
const MaxDecompressedBytes = 64 << 20 // 64 MiB

// zlibCMFAliasBytes is the decompressed length at which the first byte of a
// §4.1 container's big-endian length prefix can itself be a valid zlib CMF
// byte. RFC 1950 requires CM == 8 in the low nibble, so the lowest aliasing
// prefix byte is 0x08, reached when the declared length is 0x08000000.
//
// This matters because Decode attempts unprefixed zlib before the prefixed
// container. That ordering is safe for two reasons and the second one is the
// one that decays: decompressContainer prefers whichever framing actually
// decodes, so a spurious header match still loses to a real container; and
// below this threshold the prefix cannot present as a CMF at all, so the
// spurious match never even starts. Raising MaxDecompressedBytes past this
// point keeps the first reason and loses the second, which is a narrower
// safety margin than the code was written against.
const zlibCMFAliasBytes = 0x0800_0000 // 128 MiB

// Compile-time guards on MaxDecompressedBytes. Each subtraction underflows and
// fails the build if the bound leaves its valid range, rather than letting a
// clone fail at runtime on a message this implementation itself produced.
const (
	_ = uint(zlibCMFAliasBytes - 1 - MaxDecompressedBytes)
	_ = uint(MaxDecompressedBytes - 1) // must be positive.
)

// errNotZlib marks a payload whose first two bytes are not a zlib header
// (RFC 1950). It is the only decompression outcome that licenses the caller to
// try a different framing: once a stream is recognized as zlib, a failure to
// decode it is a real failure and must be reported, never reinterpreted.
var errNotZlib = errors.New("xfer: not a zlib stream")

// Encode serializes all cards and zlib-compresses the result.
// Uses Fossil's compression format: 4-byte big-endian uncompressed size prefix
// followed by standard zlib data.
func (m *Message) Encode() ([]byte, error) {
	if m == nil {
		panic("xfer.Message.Encode: m must not be nil")
	}
	raw, err := m.EncodeUncompressed()
	if err != nil {
		return nil, err
	}
	var zbuf bytes.Buffer
	// 4-byte big-endian uncompressed size prefix (Fossil's blob_compress format).
	var sizePrefix [4]byte
	binary.BigEndian.PutUint32(sizePrefix[:], uint32(len(raw)))
	zbuf.Write(sizePrefix[:])
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("xfer: message zlib write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("xfer: message zlib close: %w", err)
	}
	return zbuf.Bytes(), nil
}

// EncodeUncompressed serializes all cards without zlib compression.
func (m *Message) EncodeUncompressed() ([]byte, error) {
	var buf bytes.Buffer
	for i, c := range m.Cards {
		if err := EncodeCard(&buf, c); err != nil {
			return nil, fmt.Errorf("xfer: encode card %d (%T): %w", i, c, err)
		}
	}
	return buf.Bytes(), nil
}

// Decode decodes an xfer message. It tries three formats in order:
//  1. Unprefixed zlib. No Fossil wire format produces this: every
//     application/x-fossil body carries the §4.1 length prefix, on both the
//     request side (http.c) and the reply side (cgi.c), both through
//     blob_compress. It is kept as a tolerance for a peer that omits the
//     prefix, not because canonical emits it.
//  2. 4-byte BE length prefix + zlib (the §4.1 compressed container, which is
//     also what Encode produces, and what every Fossil peer sends).
//  3. Uncompressed card data (clone protocol v3, x-fossil-uncompressed).
//
// Compressed bytes are never handed to the card parser. A payload that is
// recognizable as zlib but fails to decode — corrupt stream, or larger than
// MaxDecompressedBytes — is reported as the transport failure it is. Falling
// through to format 3 in that case parses the still-compressed body as card
// text, which yields thousands of nonsense cards and finally an arbitrary
// syntax error thousands of cards deep, naming neither the real fault nor the
// real position (issue #104).
func Decode(data []byte) (*Message, error) {
	if len(data) == 0 {
		return &Message{}, nil
	}
	raw, err := decompressContainer(data)
	if err == nil {
		return DecodeUncompressed(raw)
	}
	if !errors.Is(err, errNotZlib) {
		return nil, err
	}
	// Format 3: the body is not zlib under either framing, so it is either
	// uncompressed card data or garbage. The card parser tells them apart.
	return DecodeUncompressed(data)
}

// decompressContainer decompresses a message body under either compressed
// framing, preferring whichever one decodes. It returns errNotZlib only when
// neither framing is a zlib stream at all.
func decompressContainer(data []byte) ([]byte, error) {
	// Format 1: unprefixed zlib. See Decode; a tolerance, not a Fossil format.
	rawDirect, errDirect := decompressBounded(data)
	if errDirect == nil {
		return rawDirect, nil
	}
	if len(data) < 4 {
		return nil, errDirect
	}
	// Format 2: 4-byte BE length prefix + zlib (§4.1).
	rawPrefixed, errPrefixed := decompressBounded(data[4:])
	if errPrefixed == nil {
		return rawPrefixed, nil
	}
	// Both framings failed. Report the one that was actually recognized as
	// zlib; a length prefix can coincidentally parse as a zlib header, so
	// "format 1 failed" alone does not mean the body is corrupt.
	if errors.Is(errDirect, errNotZlib) {
		return nil, errPrefixed
	}
	return nil, errDirect
}

// decompressBounded decompresses one zlib stream with a size cap.
// It returns errNotZlib when data does not begin with a zlib header, and a
// descriptive error when a real zlib stream fails to decode or exceeds
// MaxDecompressedBytes.
func decompressBounded(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotZlib, err)
	}
	defer zr.Close()
	lr := io.LimitReader(zr, MaxDecompressedBytes+1)
	raw, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("xfer: decompress message: %w", err)
	}
	if len(raw) > MaxDecompressedBytes {
		return nil, fmt.Errorf(
			"xfer: decompressed message exceeds %d bytes", MaxDecompressedBytes)
	}
	return raw, nil
}

// DecodeUncompressed decodes cards from uncompressed data.
func DecodeUncompressed(data []byte) (*Message, error) {
	r := bufio.NewReader(bytes.NewReader(data))
	msg := &Message{}
	for {
		card, err := DecodeCard(r)
		if err == io.EOF {
			return msg, nil
		}
		if err != nil {
			return nil, fmt.Errorf("xfer: decode card %d: %w", len(msg.Cards), err)
		}
		msg.Cards = append(msg.Cards, card)
	}
}
