package blob

import (
	"bytes"
	"testing"
)

// TestEncodeForStorageUsesVerbatimWhenGiven is the storage-layer regression
// test for issue #112: a caller offering already wire-encoded bytes must
// get exactly those bytes back, not a fresh Compress() of data. This is
// what lets a receive path skip re-encoding entirely rather than compress
// then discard.
func TestEncodeForStorageUsesVerbatimWhenGiven(t *testing.T) {
	data := []byte("content whose caller already has an encoded wire form")
	verbatim := []byte("not a real zlib stream, but EncodeForStorage must not care")

	got, err := EncodeForStorage(data, verbatim)
	if err != nil {
		t.Fatalf("EncodeForStorage: %v", err)
	}
	if !bytes.Equal(got, verbatim) {
		t.Fatalf("EncodeForStorage did not return verbatim bytes unchanged:\n  got  %q\n  want %q",
			got, verbatim)
	}
}

// TestEncodeForStorageCompressesWhenVerbatimNil covers the fallback used by
// locally authored content, which never has wire-encoded bytes to offer.
func TestEncodeForStorageCompressesWhenVerbatimNil(t *testing.T) {
	data := []byte("locally authored content, never received over the wire")

	got, err := EncodeForStorage(data, nil)
	if err != nil {
		t.Fatalf("EncodeForStorage: %v", err)
	}
	want, err := Compress(data)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeForStorage(nil verbatim) != Compress(data)")
	}

	decoded, err := Decompress(got)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("round trip failed: got %q, want %q", decoded, data)
	}
}

// TestEncodeForStorageRejectsNilData asserts the documented precondition:
// data is a programmer-controlled argument (unlike verbatim, which is wire
// data), so a nil data slice is a caller bug, not a wire-error to report.
func TestEncodeForStorageRejectsNilData(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("EncodeForStorage(nil data, ...) did not panic")
		}
	}()
	EncodeForStorage(nil, []byte("irrelevant"))
}

// TestEncodeForStorageRejectsEmptyVerbatim guards against a caller passing
// a non-nil-but-empty slice, which would otherwise silently mean "use
// verbatim" while storing zero bytes -- an empty blob.content is never a
// valid encoding, so this must fail loudly rather than corrupt storage.
func TestEncodeForStorageRejectsEmptyVerbatim(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("EncodeForStorage(data, empty non-nil verbatim) did not panic")
		}
	}()
	EncodeForStorage([]byte("data"), []byte{})
}
