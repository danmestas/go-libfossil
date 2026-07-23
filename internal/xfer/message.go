package xfer

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// Message is a sequence of cards forming one xfer request or response.
type Message struct {
	Cards []Card
}

// ContentTypeCompressed and ContentTypeUncompressed are the two §4 media types
// that signal how an xfer body is framed. A body sent as ContentTypeCompressed
// is the §4.1 compressed container: a 4-byte big-endian pre-compression length
// followed by a zlib stream. A body sent as ContentTypeUncompressed is plain
// card text, which is what a clone-v3 reply uses (its file content is already
// compressed per artifact, so the message itself is not).
//
// §4 says the receiver selects framing on the Content-Type, and canonical
// fossil does exactly that: it decompresses a body if and only if the type is
// ContentTypeCompressed, and treats every other type as plain text. Verified
// against fossil 2.28 -- a pull reply arrives as ContentTypeCompressed with a
// `00 00 00 3e 78 9c ...` prefixed container, and a clone reply arrives as
// ContentTypeUncompressed with `pragma server-version ...` card text. No
// canonical reply is ever an unprefixed raw zlib stream.
const (
	ContentTypeCompressed   = "application/x-fossil"
	ContentTypeUncompressed = "application/x-fossil-uncompressed"
)

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
// below counts bytes actually inflated and also rejects a declared length that
// exceeds it before inflating anything (issue #110).
//
// Because it is a guard and not a rule, it has to sit above anything a
// conforming peer will send, or a legitimate exchange fails -- which is how
// issue #104 arose, with the bound below what this implementation's own server
// emitted in one clone round. A clone round this implementation's server emits
// stays within this bound by construction: the server flushes a round rather
// than let it cross the bound, and sends an over-bound artifact alone
// (internal/sync.emitCloneBatch). The compile-time guard in internal/sync
// pins DefaultCloneBatchBytes below half of this bound so that filler plus one
// budget-sized artifact always fits.
//
// Consequence worth naming: an artifact larger than this bound cannot be
// cloned, because the server must send it whole in a round of its own and the
// client cannot decode a round past this bound. That limit is a property of
// the artifact alone -- it no longer depends on what precedes the artifact in
// its round (issue #109).
const MaxDecompressedBytes = 64 << 20 // 64 MiB

// Compile-time guard: the bound must be positive. The subtraction underflows
// and fails the build if MaxDecompressedBytes is ever set to zero, rather than
// letting a clone fail at runtime on a message this implementation produced.
const _ = uint(MaxDecompressedBytes - 1)

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

// Decode decodes an xfer message, selecting its framing from the §4
// Content-Type rather than by trying each framing and seeing which parses.
// A ContentTypeCompressed body is decoded as the §4.1 compressed container;
// any other type (including ContentTypeUncompressed, or an absent header) is
// decoded as plain card text, matching canonical fossil's rule of
// decompressing a body iff its type is exactly ContentTypeCompressed.
//
// Selecting on Content-Type is what keeps a decompression failure from being
// misread as "wrong framing". A compressed body that fails to inflate -- a
// corrupt stream, or one larger than MaxDecompressedBytes -- is reported as
// the transport fault it is and never re-fed to the card parser, which under
// the old trial-based dispatch read still-compressed bytes as thousands of
// nonsense cards and reported a syntax error thousands of cards deep, naming
// neither the real fault nor the real position (issue #104).
func Decode(data []byte, contentType string) (*Message, error) {
	if len(data) == 0 {
		return &Message{}, nil
	}
	if contentTypeIsCompressed(contentType) {
		return DecodeCompressed(data)
	}
	return DecodeUncompressed(data)
}

// contentTypeIsCompressed reports whether a §4 Content-Type selects the
// compressed container framing. §4 signals framing by the bare media type, so
// any parameters (e.g. "; charset=...") and letter case are ignored.
func contentTypeIsCompressed(contentType string) bool {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), ContentTypeCompressed)
}

// DecodeCompressed decodes a §4.1 compressed container: a 4-byte big-endian
// pre-compression length followed by a zlib stream. It is the framing a
// ContentTypeCompressed body carries and the framing Encode produces.
func DecodeCompressed(data []byte) (*Message, error) {
	raw, err := decompressContainer(data)
	if err != nil {
		return nil, err
	}
	return DecodeUncompressed(raw)
}

// decompressContainer decompresses a §4.1 compressed container. The declared
// length is checked against MaxDecompressedBytes before inflating anything, so
// an oversized body is rejected on its own claim rather than after being
// expanded (issue #110). A stream that fails to inflate is returned as the
// error it is; the caller does not retry it as another framing, because the
// Content-Type already committed this body to the compressed container.
func decompressContainer(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf(
			"xfer: compressed body is %d bytes, too short for a 4-byte length prefix", len(data))
	}
	declared := binary.BigEndian.Uint32(data[:4])
	if declared > MaxDecompressedBytes {
		return nil, fmt.Errorf(
			"xfer: declared container length %d exceeds %d bytes", declared, MaxDecompressedBytes)
	}
	return decompressBounded(data[4:])
}

// decompressBounded decompresses one zlib stream with a size cap. It reports a
// descriptive error when the data is not a zlib stream, when a real zlib stream
// fails to decode, or when it exceeds MaxDecompressedBytes.
func decompressBounded(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("xfer: compressed body is not a valid zlib stream: %w", err)
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
