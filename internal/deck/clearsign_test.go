package deck

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
)

// buildClearsigned wraps an inner control-artifact body (cards + Z card,
// where the body already ends in the Z card's trailing '\n') in a PGP
// clearsign envelope of the exact shape fossil produces: an armored header
// line, a "Hash:" armor header, a blank line, the body, then the detached
// signature block. It exists so the synthetic tests exercise the same
// header-skip and trailer-trim edges as the real 2007-era fixture.
func buildClearsigned(innerBody string) []byte {
	env := "-----BEGIN PGP SIGNED MESSAGE-----\n" +
		"Hash: SHA1\n" +
		"\n" +
		innerBody +
		"-----BEGIN PGP SIGNATURE-----\n" +
		"Version: GnuPG v2.0.3 (GNU/Linux)\n" +
		"\n" +
		"iD8DBQFHTnEWvonzZ/CRa7gRAh4O\n" +
		"=LH1V\n" +
		"-----END PGP SIGNATURE-----\n"
	return []byte(env)
}

// zBody appends a correct Z card to cards, returning the inner body the Z
// checksum is computed over plus the Z card itself (ending in '\n').
func zBody(cards string) string {
	sum := md5.Sum([]byte(cards))
	return cards + fmt.Sprintf("Z %x\n", sum)
}

// TestVerifyZClearsignedSynthetic checks that VerifyZ validates the Z card of
// a PGP-clearsigned artifact by looking at the inner content, not the raw
// blob's trailing signature bytes. Method: build a body with a correct Z
// card, wrap it in a clearsign envelope, and assert VerifyZ accepts it.
func TestVerifyZClearsignedSynthetic(t *testing.T) {
	body := zBody("C hello\nD 2024-01-15T10:30:00.000\nU alice\n")
	manifest := buildClearsigned(body)
	if err := VerifyZ(manifest); err != nil {
		t.Fatalf("VerifyZ rejected a valid clearsigned manifest: %v", err)
	}
}

// TestParseClearsignedSynthetic checks that Parse strips the clearsign
// framing and parses the inner cards, rather than choking on the '-----'
// lines. Method: parse the wrapped body and assert the inner card values
// surface on the Deck.
func TestParseClearsignedSynthetic(t *testing.T) {
	body := zBody("C hello\nD 2024-01-15T10:30:00.000\nU alice\n")
	d, err := Parse(buildClearsigned(body))
	if err != nil {
		t.Fatalf("Parse failed on clearsigned manifest: %v", err)
	}
	if d.C != "hello" {
		t.Errorf("C = %q, want %q", d.C, "hello")
	}
	if d.U == nil || *d.U != "alice" {
		t.Errorf("U = %v, want alice", d.U)
	}
}

// TestVerifyZClearsignedBadChecksumSynthetic checks that stripping does not
// mask a genuinely wrong Z card: a clearsigned artifact whose inner Z hash is
// incorrect must still be rejected.
func TestVerifyZClearsignedBadChecksumSynthetic(t *testing.T) {
	body := "C hello\nU alice\nZ 00000000000000000000000000000000\n"
	if err := VerifyZ(buildClearsigned(body)); err == nil {
		t.Fatal("VerifyZ accepted a clearsigned manifest with a bad Z checksum")
	}
}

// TestStripClearsignPlainUnaffected checks that a non-clearsigned artifact is
// returned byte-for-byte unchanged, and idempotently so on already-stripped
// content (the header prefix is absent, so the scan is a no-op).
func TestStripClearsignPlainUnaffected(t *testing.T) {
	plain := []byte(zBody("C hi\nU bob\n"))
	got := stripClearsign(plain)
	if string(got) != string(plain) {
		t.Fatalf("stripClearsign mutated a plain artifact:\n got=%q\nwant=%q", got, plain)
	}
	// Idempotent: stripping the already-inner content is a no-op.
	inner := buildClearsigned(zBody("C hi\nU bob\n"))
	once := stripClearsign(inner)
	twice := stripClearsign(once)
	if string(once) != string(twice) {
		t.Fatalf("stripClearsign not idempotent:\n once=%q\ntwice=%q", once, twice)
	}
}

// TestParseClearsignedRealFossilManifest is the acceptance case: a real
// PGP-clearsigned check-in from the Fossil SCM repo's own 2007-era history
// (aku's commit 61829b07...). Method: load the raw artifact, confirm its
// content-addressed SHA-1 still equals the UUID it has always been known by
// (the fix must NOT change the hash), then confirm VerifyZ accepts it and
// Parse surfaces the inner cards.
func TestParseClearsignedRealFossilManifest(t *testing.T) {
	const uuid = "61829b076bd6e1bb9b40794206cb1dd5f8eca8ea"
	raw, err := os.ReadFile("testdata/clearsigned-61829b07.manifest")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// The artifact hash is computed over the raw blob, framing included;
	// the parser must never alter which UUID this artifact resolves to.
	sum := sha1.Sum(raw)
	if got := hex.EncodeToString(sum[:]); got != uuid {
		t.Fatalf("fixture SHA-1 = %s, want %s (content addressing changed?)", got, uuid)
	}

	if err := VerifyZ(raw); err != nil {
		t.Fatalf("VerifyZ rejected a real clearsigned manifest: %v", err)
	}

	d, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed on real clearsigned manifest: %v", err)
	}
	if d.Type != Checkin {
		t.Errorf("Type = %d, want Checkin", d.Type)
	}
	if d.U == nil || *d.U != "aku" {
		t.Errorf("U = %v, want aku", d.U)
	}
	if len(d.P) != 1 || d.P[0] != "04d76a9e797fbefe8dc872b58c91093443594aa7" {
		t.Errorf("P = %v, want [04d76a9e797fbefe8dc872b58c91093443594aa7]", d.P)
	}
	if d.R != "d58ea6a855cffbe0bfc0bca5ed85243a" {
		t.Errorf("R = %q, want d58ea6a855cffbe0bfc0bca5ed85243a", d.R)
	}
	if len(d.F) == 0 {
		t.Error("expected F cards to be parsed from the clearsigned manifest")
	}
}
