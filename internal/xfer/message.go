package xfer

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/danmestas/go-libfossil/internal/hash"
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
	// ContentTypeDebug is the §4 debug media type: the same card content as
	// ContentTypeUncompressed, carried uncompressed so a human can read it. A
	// server dispatches a request that carries it (§4-3); a client reads a
	// reply that carries it as plain cards (§4-8).
	ContentTypeDebug = "application/x-fossil-debug"
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

// MaxBodyBytes caps an xfer reply body a client reads off the wire. Decode
// can bound only what it is handed, so without this a peer could stream a
// reply without end and the client would hold all of it before decoding.
//
// It is MaxDecompressedBytes plus headroom for the container's framing, so no
// body Decode would accept is refused: a compressed container decompresses to
// at most MaxDecompressedBytes, and zlib grows incompressible input by far
// less than the headroom; an uncompressed clone reply is a round this
// module's server flushes at DefaultCloneBatchBytes and fossil at its
// max-download setting (5 MB by default), each plus one artifact. That last
// artifact is what this bounds: one whose stored form is larger cannot be
// cloned, as MaxDecompressedBytes already says of a compressed reply. A server
// bounds the other direction itself (sync.ServeHTTP reads at most 50 MiB).
const MaxBodyBytes = MaxDecompressedBytes + 1<<20

// ErrBodyTooLarge reports a reply body longer than MaxBodyBytes. It is the
// peer's fault and resending the same request gets the same reply, so a
// transport returns it without tagging it retryable.
var ErrBodyTooLarge = fmt.Errorf("xfer: reply body exceeds %d bytes", MaxBodyBytes)

// ReadBody reads an xfer reply body, refusing one longer than MaxBodyBytes
// once the cap is passed rather than after holding all of it.
func ReadBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, ErrBodyTooLarge
	}
	return body, nil
}

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
	return strings.EqualFold(mediaType(contentType), ContentTypeCompressed)
}

// KnownContentType reports whether a Content-Type is a §4 media type a server
// dispatches a request from: the compressed container, the uncompressed card
// text, or the debug type (§4-3). Decode treats every other type as plain card
// text, so a server reading bodies from untrusted peers uses this to reject a
// body that carries no framing signal instead of guessing at its framing.
func KnownContentType(contentType string) bool {
	mt := mediaType(contentType)
	return strings.EqualFold(mt, ContentTypeCompressed) ||
		strings.EqualFold(mt, ContentTypeUncompressed) ||
		strings.EqualFold(mt, ContentTypeDebug)
}

// DecodeReply decodes a sync reply, selecting its framing by §4's reply rule,
// which is not a request's. A reply with no Content-Type is the compressed
// container. A present type is matched as fossil's http.c matches it: by
// case-insensitive prefix, so parameters and suffixes ride along. An
// application/x-fossil-debug or -uncompressed prefix is plain card text, any
// other application/x-fossil prefix is the compressed container, and any other
// type (a proxy's HTML error page, say) is not a sync reply and is refused.
func DecodeReply(data []byte, contentType string) (*Message, error) {
	switch {
	case hasPrefixFold(contentType, ContentTypeDebug),
		hasPrefixFold(contentType, ContentTypeUncompressed):
		return Decode(data, ContentTypeUncompressed)
	case contentType == "", hasPrefixFold(contentType, ContentTypeCompressed):
		return Decode(data, ContentTypeCompressed)
	default:
		return nil, fmt.Errorf("xfer: reply Content-Type %q is not a sync reply", contentType)
	}
}

// hasPrefixFold reports whether s begins with prefix, ignoring letter case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// mediaType strips any parameters (e.g. "; charset=...") from a Content-Type,
// leaving the bare media type §4 signals framing with.
func mediaType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.TrimSpace(contentType)
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

// MaxCFileInflateBytes caps the bytes all cfile payloads in one message may
// inflate to, together. Each cfile is already held to its own USIZE, but USIZE
// is the sender's claim and zlib inflates about 1000:1, so without a message
// budget a body of a few megabytes holding many cfiles costs tens of gigabytes
// -- on a server too, which decodes a request before looking at its cards.
//
// Like MaxDecompressedBytes it is a local guard, not a protocol rule. It sits
// far above a clone round: fossil stops filling a round once its compressed
// output passes its max-download setting (5 MB by default), and this module's
// server stops at DefaultCloneBatchBytes of artifact content. A round would
// need a compression ratio over 200:1 throughout to reach it. A single
// artifact larger than this cannot be received as a cfile.
//
// Why 1 GiB: it is the point between those two pressures. Lower, and rounds of
// highly compressible content from a stock fossil server start to fail. Higher,
// and one hostile message buys more memory than a single sync exchange should
// hold: a body is at most MaxBodyBytes (65 MiB), and this keeps its inflation
// near sixteen times that, where zlib alone allows about a thousand times.
const MaxCFileInflateBytes = 1 << 30 // 1 GiB

// DecodeUncompressed decodes cards from uncompressed data.
//
// Each login card records the SHA-1 of the bytes after its line, which its
// nonce must equal (see LoginCard.NonceCoversTail). Only the decoder still
// has those bytes, and a re-encoding of the parsed cards is not them.
func DecodeUncompressed(data []byte) (*Message, error) {
	return decodeMessage(data, MaxCFileInflateBytes)
}

// decodeMessage decodes cards from uncompressed data, letting every cfile in
// it inflate to at most budget bytes in total.
func decodeMessage(data []byte, budget int64) (*Message, error) {
	src := bytes.NewReader(data)
	r := bufio.NewReader(src)
	msg := &Message{}
	for {
		card, err := decodeCard(r, &budget)
		if err == io.EOF {
			return msg, nil
		}
		if err != nil {
			return nil, fmt.Errorf("xfer: decode card %d: %w", len(msg.Cards), err)
		}
		if lc, ok := card.(*LoginCard); ok {
			consumed := len(data) - src.Len() - r.Buffered()
			lc.tailSHA1 = hash.SHA1(data[consumed:])
		}
		msg.Cards = append(msg.Cards, card)
		// A BadFileCard means a file/cfile/uvfile line could not be framed, so
		// its payload length is unknown and no later card can be located. Stop
		// here, as fossil's page_xfer breaks out of its card loop; the caller
		// acts on the cards decoded so far.
		if _, bad := card.(*BadFileCard); bad {
			return msg, nil
		}
	}
}
