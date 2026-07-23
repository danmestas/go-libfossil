package xfer

import (
	"strings"
	"testing"
)

// TestContentTypeIsCompressed pins the §4 media-type-to-framing rule: only
// application/x-fossil selects the compressed container; every other type,
// including application/x-fossil-uncompressed and an absent header, selects
// plain card text. Method: map each media type and assert the boolean, which
// is the whole of the dispatch decision. Case and trailing parameters are
// ignored because §4 signals framing by the bare media type.
func TestContentTypeIsCompressed(t *testing.T) {
	cases := []struct {
		contentType string
		compressed  bool
	}{
		{ContentTypeCompressed, true},
		{"application/x-fossil", true},
		{"APPLICATION/X-FOSSIL", true},
		{"application/x-fossil; charset=utf-8", true},
		{"  application/x-fossil  ", true},
		{ContentTypeUncompressed, false},
		{"application/x-fossil-uncompressed", false},
		{"application/x-fossil-uncompressed; charset=utf-8", false},
		{"text/plain", false},
		{"", false},
	}
	for _, c := range cases {
		if got := contentTypeIsCompressed(c.contentType); got != c.compressed {
			t.Errorf("contentTypeIsCompressed(%q) = %v, want %v", c.contentType, got, c.compressed)
		}
	}
}

// TestDecodeDispatchesOnContentType pins that framing follows the Content-Type,
// not the bytes: a compressed container decodes to its real cards when labelled
// compressed, and plain card text decodes when labelled uncompressed but is
// refused when labelled compressed because it is not a zlib stream. Method:
// build a container of known card count and plain card text, decode each under
// its own label, and confirm the plain text is refused under the compressed
// label rather than mis-decompressed.
//
// The reverse pairing -- a compressed body under the uncompressed label -- is
// deliberately not asserted: DecodeUncompressed reads whatever lines it can and
// does not reliably fault on binary input, so its output is unspecified. What
// matters for §4 is that the label chooses the path, which the compressed-label
// cases establish.
func TestDecodeDispatchesOnContentType(t *testing.T) {
	compressed := compressedContainer(t, 4096)
	plaintext := []byte("igot " + strings.Repeat("a", 40) + "\nclone_seqno 3\n")

	if _, err := Decode(compressed, ContentTypeCompressed); err != nil {
		t.Errorf("compressed body under compressed type: %v", err)
	}
	msg, err := Decode(plaintext, ContentTypeUncompressed)
	if err != nil {
		t.Errorf("plain cards under uncompressed type: %v", err)
	} else if len(msg.Cards) != 2 {
		t.Errorf("plain cards under uncompressed type: %d cards, want 2", len(msg.Cards))
	}
	if _, err := Decode(plaintext, ContentTypeCompressed); err == nil {
		t.Error("plain cards under compressed type decoded; they are not a zlib stream")
	}
}

// TestDecodeCompressedFailureNamesDecompression pins acceptance criterion 2 of
// issue #106: a body whose Content-Type says compressed and which fails to
// decompress reports the decompression fault, and is never retried as card
// text. Method: build a body with a small in-bound declared length followed by
// bytes that are not a zlib stream, so the failure occurs at inflation rather
// than at the length check, and assert the error names the zlib failure and not
// a card-parse failure.
func TestDecodeCompressedFailureNamesDecompression(t *testing.T) {
	// Declared length 100 (well under the bound) so the length guard passes and
	// control reaches inflation; the payload after the prefix is not zlib.
	body := append([]byte{0x00, 0x00, 0x00, 0x64}, []byte("this is not a zlib stream at all")...)

	_, err := Decode(body, ContentTypeCompressed)
	if err == nil {
		t.Fatal("Decode accepted a non-zlib body under the compressed Content-Type")
	}
	if !strings.Contains(err.Error(), "zlib") {
		t.Errorf("error does not name the decompression fault: %v", err)
	}
	if strings.Contains(err.Error(), "decode card") {
		t.Errorf("compressed body was retried as cards: %v", err)
	}
}

// TestDecodeEmptyBodyIsEmptyMessage pins that an empty body is an empty message
// under either framing, since a zero-length reply carries no cards. Method:
// decode a nil body under each Content-Type and assert no error and no cards.
func TestDecodeEmptyBodyIsEmptyMessage(t *testing.T) {
	for _, ct := range []string{ContentTypeCompressed, ContentTypeUncompressed, ""} {
		msg, err := Decode(nil, ct)
		if err != nil {
			t.Fatalf("Decode(nil, %q): %v", ct, err)
		}
		if len(msg.Cards) != 0 {
			t.Errorf("Decode(nil, %q) returned %d cards, want 0", ct, len(msg.Cards))
		}
	}
}
