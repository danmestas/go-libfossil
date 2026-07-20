package xfer

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"strings"
	"testing"
)

// compressedContainer builds the §4.1 compressed container -- 4-byte
// big-endian pre-compression length followed by a zlib stream -- around n
// bytes of card text, without ever holding n bytes in memory.
func compressedContainer(t *testing.T, n int) []byte {
	t.Helper()
	if n <= 0 {
		panic("compressedContainer: n must be positive")
	}
	var buf bytes.Buffer
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(n))
	buf.Write(prefix[:])

	zw := zlib.NewWriter(&buf)
	// "igot <hash>\n" repeated: real cards, so a decoder that reaches them
	// fails for a reason other than the payload being unparseable.
	card := []byte("igot " + strings.Repeat("a", 40) + "\n")
	written := 0
	for written < n {
		chunk := card
		if remaining := n - written; remaining < len(chunk) {
			chunk = chunk[:remaining]
		}
		if _, err := zw.Write(chunk); err != nil {
			t.Fatalf("zlib write: %v", err)
		}
		written += len(chunk)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	if buf.Len() >= n {
		t.Fatalf("container did not compress: %d bytes for %d input", buf.Len(), n)
	}
	return buf.Bytes()
}

// TestDecodeOversizeCompressedMessageIsReported checks that a well-formed
// compressed body larger than MaxDecompressedBytes is reported as an oversize
// message. Method: build a valid container whose decompressed length exceeds
// the bound and decode it. The regression this pins (issue #104) is not that
// the decode fails -- it is *how* it failed: the previous fallback chain
// treated the size rejection as "not this framing" and handed the still
// compressed bytes to the card parser, which read compressed noise as cards
// until one of them split into no fields, and reported a card-syntax error
// roughly sixteen thousand cards into a body that held none.
func TestDecodeOversizeCompressedMessageIsReported(t *testing.T) {
	data := compressedContainer(t, MaxDecompressedBytes+1024)

	_, err := Decode(data)
	if err == nil {
		t.Fatal("Decode accepted a message over MaxDecompressedBytes")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error does not name the size bound: %v", err)
	}
	if strings.Contains(err.Error(), "empty line after split") {
		t.Errorf("compressed bytes were parsed as cards: %v", err)
	}
	if strings.Contains(err.Error(), "decode card") {
		t.Errorf("transport failure reported as a card fault: %v", err)
	}
}

// TestDecodeCorruptZlibIsNotParsedAsCards checks that a body with a valid zlib
// header and a corrupt body is reported as a decompression failure. Method:
// truncate a valid container mid-stream, which leaves the header intact.
func TestDecodeCorruptZlibIsNotParsedAsCards(t *testing.T) {
	full := compressedContainer(t, 1<<20)
	truncated := full[:len(full)/2]

	_, err := Decode(truncated)
	if err == nil {
		t.Fatal("Decode accepted a truncated zlib stream")
	}
	if strings.Contains(err.Error(), "decode card") {
		t.Errorf("truncated stream reported as a card fault: %v", err)
	}
}

// TestDecodeRoundTripsLargeMessageUnderBound checks that a message just under
// the bound still decodes, so the guard cannot be satisfied by rejecting
// everything large. Method: encode enough igot cards to pass 32 MiB and
// decode the result.
func TestDecodeRoundTripsLargeMessageUnderBound(t *testing.T) {
	const target = 32 << 20
	msg := &Message{}
	for size := 0; size < target; size += 46 {
		msg.Cards = append(msg.Cards, &IGotCard{UUID: strings.Repeat("b", 40)})
	}
	wire, err := msg.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Cards) != len(msg.Cards) {
		t.Errorf("decoded %d cards, want %d", len(got.Cards), len(msg.Cards))
	}
}

// TestDecodeUncompressedBodyStillWorks checks that plain card text is still
// accepted, since the fix narrows when that fallback is reached. Method:
// decode an uncompressed body of the kind an x-fossil-uncompressed clone
// reply carries.
func TestDecodeUncompressedBodyStillWorks(t *testing.T) {
	body := []byte("igot " + strings.Repeat("c", 40) + "\nclone_seqno 7\n")

	msg, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(msg.Cards) != 2 {
		t.Fatalf("decoded %d cards, want 2", len(msg.Cards))
	}
	if _, ok := msg.Cards[1].(*CloneSeqNoCard); !ok {
		t.Errorf("second card is %T, want *CloneSeqNoCard", msg.Cards[1])
	}
}

// TestMaxDecompressedBytesIsReachable guards the bound against being set below
// what a zlib stream can actually deliver here. Method: decompress a container
// sized exactly at the bound.
func TestMaxDecompressedBytesIsReachable(t *testing.T) {
	data := compressedContainer(t, MaxDecompressedBytes)

	raw, err := decompressContainer(data)
	if err != nil {
		t.Fatalf("decompressContainer at the bound: %v", err)
	}
	if len(raw) != MaxDecompressedBytes {
		t.Errorf("decompressed %d bytes, want %d", len(raw), MaxDecompressedBytes)
	}
}
