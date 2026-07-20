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
//   - Below: it must stay under zlibCMFAliasBytes, so that no length prefix
//     this implementation writes can present as a zlib header. Enforced at
//     compile time immediately below. Read that constant's comment before
//     trusting the guard: it bounds what we emit, not what we accept.
//
// Consequence worth naming, and it is not the simple one. An artifact larger
// than this bound can never be cloned, because the server must send it whole
// to make progress. But the real threshold is lower and is not a property of
// the artifact alone: a round already holding bytes has that much less room,
// so the ceiling is this bound minus whatever the round accumulated before the
// artifact. With a 16 MB budget an artifact much above 48 MiB clones only when
// it happens to lead its round, and fails when ordinary artifacts precede it.
// That ordering dependence is issue #109; it is a defect this fix exposes
// rather than one this fix introduces or resolves.
const MaxDecompressedBytes = 64 << 20 // 64 MiB

// zlibCMFAliasBytes is the declared length at which the first byte of a §4.1
// container's big-endian length prefix can itself be a valid zlib CMF byte.
// RFC 1950 requires CM == 8 in the low nibble, so the lowest aliasing prefix
// byte is 0x08, reached at a declared length of 0x08000000.
//
// It matters because Decode attempts unprefixed zlib before the prefixed
// container, and format 1 returns immediately if it succeeds -- format 2 is
// consulted only when format 1 fails. A prefix that presents as a zlib header
// therefore gets first refusal on a legitimate container.
//
// Be precise about what the guard below is worth, because it is less than it
// looks:
//
//   - It does NOT bound aliasing on input. The aliasing byte comes from the
//     length the *peer* declared, and this decoder never reads that field --
//     it slices past it and counts bytes actually inflated. No value of
//     MaxDecompressedBytes constrains what a peer declares. A peer declaring
//     0x081D0000 while sending 46 bytes produces a parsing header at offset 0,
//     confirmed by probe.
//   - What actually protects a real container in that case is the rest of the
//     stream, not arithmetic: after consuming two prefix bytes as a header,
//     inflate must still parse the remaining two prefix bytes plus the real
//     zlib stream as DEFLATE and match its Adler-32. It does not -- measured
//     `flate: corrupt input` on all three aliasing lengths probed -- so format
//     1 fails, format 2 runs, and the container decodes correctly. The
//     residual risk is a stream that both parses as DEFLATE and hits a 32-bit
//     checksum by chance.
//   - What the guard DOES buy is narrow and real: this implementation's own
//     Encode writes uint32(len(raw)) as the prefix, so holding the bound below
//     this threshold means a body libfossil emits can never carry an aliasing
//     prefix. It is an invariant on our output, not a defence of our input.
//
// The airtight fix is to stop guessing the framing -- select on Content-Type
// per §4, or reject a declared length above the bound before attempting format
// 1. Both are redesigns of framing selection and neither belongs in #104.
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
// framing. Format 1 wins outright if it decodes; format 2 is tried only when
// format 1 fails. The error comparison at the end therefore decides nothing
// about which framing is used -- it only selects which failure to report when
// both have already failed. It returns errNotZlib only when neither framing is
// a zlib stream at all.
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
