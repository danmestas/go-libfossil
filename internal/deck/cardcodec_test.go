package deck

import (
	"crypto/md5"
	"fmt"
	"strings"
	"testing"
)

// withZCard appends the §4.7.19 Z checksum card to a manifest body so Parse
// accepts it. The cards in body must already appear in ascending-letter
// order, which Parse enforces.
func withZCard(body string) []byte {
	sum := md5.Sum([]byte(body))
	return []byte(fmt.Sprintf("%sZ %x\n", body, sum))
}

// --- #90: J-card decode direction (§4.7.8) ---

// TestParseJCardDecodesValueKeepsNameVerbatim pins §4.7.8: the J-card value
// is escape-decoded while the field name is stored verbatim, matching
// canonical manifest.c which defossilizes only zValue and compares the raw
// zName. A value carrying escaped spaces must arrive decoded; a name
// carrying an escape must arrive unchanged.
func TestParseJCardDecodesValueKeepsNameVerbatim(t *testing.T) {
	d := &Deck{}
	if err := parseJCard(d, `a\sname a\svalue`); err != nil {
		t.Fatalf("parseJCard: %v", err)
	}
	if len(d.J) != 1 {
		t.Fatalf("J count = %d, want 1", len(d.J))
	}
	if got := d.J[0].Name; got != `a\sname` {
		t.Errorf("Name = %q, want verbatim %q", got, `a\sname`)
	}
	if got := d.J[0].Value; got != "a value" {
		t.Errorf("Value = %q, want decoded %q", got, "a value")
	}
}

// TestMarshalJCardEncodesValueKeepsNameVerbatim is the write-side mirror of
// the decode direction: the value is escape-encoded on the way out and the
// name is written verbatim, so a decoded value with spaces reproduces its
// wire escapes.
func TestMarshalJCardEncodesValueKeepsNameVerbatim(t *testing.T) {
	var b strings.Builder
	d := &Deck{J: []TicketField{{Name: "comment", Value: "a value with spaces"}}}
	marshalCards(&b, d)
	if !strings.Contains(b.String(), "J comment a\\svalue\\swith\\sspaces\n") {
		t.Fatalf("J value not escape-encoded on marshal:\n%s", b.String())
	}
}

// TestJCardWireRoundTrip pins that Marshal reproduces the original wire
// bytes for an unmodified J-card whose value contains spaces (§4.7.8).
func TestJCardWireRoundTrip(t *testing.T) {
	wire := withZCard("D 2024-01-15T10:30:00.000\nJ comment a\\svalue\\swith\\sspaces\n")
	d, err := Parse(wire)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := d.J[0].Value; got != "a value with spaces" {
		t.Fatalf("Value = %q, want decoded", got)
	}
	out, _ := d.Marshal()
	if !strings.Contains(string(out), "J comment a\\svalue\\swith\\sspaces\n") {
		t.Fatalf("J line not reproduced on round-trip:\n%s", out)
	}
}

// --- #91: T-card marshal escape-encodes the tag name (§4.7.16) ---

// TestTCardNameWithSpaceRoundTrips pins that a tag name containing a space
// round-trips Marshal -> Parse unchanged. Marshal must escape-encode the
// name so "sym-my branch" reaches the wire as "sym-my\sbranch" and parses
// back to a single name token, not two.
func TestTCardNameWithSpaceRoundTrips(t *testing.T) {
	d := &Deck{
		T: []TagCard{{Type: TagPropagating, Name: "sym-my branch", UUID: "*"}},
	}
	out, _ := d.Marshal()
	if !strings.Contains(string(out), "T *sym-my\\sbranch *\n") {
		t.Fatalf("tag name not escape-encoded on marshal:\n%s", out)
	}
	parsed, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.T) != 1 {
		t.Fatalf("T count = %d, want 1", len(parsed.T))
	}
	if got := parsed.T[0].Name; got != "sym-my branch" {
		t.Errorf("Name = %q, want %q", got, "sym-my branch")
	}
	if got := parsed.T[0].UUID; got != "*" {
		t.Errorf("UUID = %q, want %q", got, "*")
	}
}

// TestTCardTargetHashStaysUnencoded pins §4.7.16: the target token is never
// decoded on the way in, so it must not be encoded on the way out.
func TestTCardTargetHashStaysUnencoded(t *testing.T) {
	target := strings.Repeat("a", 40)
	var b strings.Builder
	d := &Deck{T: []TagCard{{Type: TagSingleton, Name: "sym-x", UUID: target}}}
	marshalCards(&b, d)
	if !strings.Contains(b.String(), "T +sym-x "+target+"\n") {
		t.Fatalf("target hash was altered on marshal:\n%s", b.String())
	}
}

// --- #93: parseTCard §4.7.16 validation ---

// TestParseTCardRejectsHexOnlyName pins that a T-card name which after the
// sign character is entirely hex digits is rejected, because it cannot be
// told apart from a hash target (§4.7.16).
func TestParseTCardRejectsHexOnlyName(t *testing.T) {
	hexName := strings.Repeat("a", 40)
	wire := withZCard("D 2024-01-15T10:30:00.000\nT +" + hexName + " *\n")
	if _, err := Parse(wire); err == nil {
		t.Fatalf("Parse accepted a hex-only tag name %q, want error", hexName)
	}
}

// TestParseTCardRejectsInvalidTarget pins that a target which is neither a
// valid artifact hash nor the literal '*' is rejected (§4.7.16).
func TestParseTCardRejectsInvalidTarget(t *testing.T) {
	wire := withZCard("D 2024-01-15T10:30:00.000\nT +sym-x notahash\n")
	if _, err := Parse(wire); err == nil {
		t.Fatalf("Parse accepted an invalid T-card target, want error")
	}
}

// TestParseTCardAcceptsValidTargets guards against over-rejection: a real
// 40-hex target and the literal '*' must both still parse (§4.7.16).
func TestParseTCardAcceptsValidTargets(t *testing.T) {
	target := strings.Repeat("b", 40)
	wire := withZCard("D 2024-01-15T10:30:00.000\nT +sym-x " + target + "\n")
	if _, err := Parse(wire); err != nil {
		t.Fatalf("Parse rejected a valid 40-hex target: %v", err)
	}
	wireStar := withZCard("D 2024-01-15T10:30:00.000\nT +sym-x *\n")
	if _, err := Parse(wireStar); err != nil {
		t.Fatalf("Parse rejected the literal '*' target: %v", err)
	}
}
