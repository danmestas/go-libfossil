package deck

import "testing"

// escapePairs is the complete §4.3 escape table: a single raw byte and the
// two-character wire escape canonical fossil writes for it. Verified against
// fossil 2.28 test check-in, whose C card rendered a TAB as \t, a space as \s,
// a backslash as \\, a VT as \v and an FF as \f.
var escapePairs = []struct {
	name string
	raw  byte
	wire string
}{
	{"newline", '\n', `\n`},
	{"space", ' ', `\s`},
	{"tab", '\t', `\t`},
	{"carriage_return", '\r', `\r`},
	{"vertical_tab", '\v', `\v`},
	{"form_feed", '\f', `\f`},
	{"nul", 0, `\0`},
	{"backslash", '\\', `\\`},
}

// TestFossilEncodeEscapeTable checks that every §4.3 escape encodes to the
// exact wire form canonical fossil uses, with a literal payload on each side so
// the escape is exercised in context rather than in isolation.
func TestFossilEncodeEscapeTable(t *testing.T) {
	for _, p := range escapePairs {
		raw := "x" + string(p.raw) + "y"
		want := "x" + p.wire + "y"
		if got := FossilEncode(raw); got != want {
			t.Errorf("%s: FossilEncode(%q) = %q, want %q", p.name, raw, got, want)
		}
	}
}

// TestFossilDecodeEscapeTable checks the inverse: each wire escape decodes back
// to its raw byte.
func TestFossilDecodeEscapeTable(t *testing.T) {
	for _, p := range escapePairs {
		wire := "x" + p.wire + "y"
		want := "x" + string(p.raw) + "y"
		if got := FossilDecode(wire); got != want {
			t.Errorf("%s: FossilDecode(%q) = %q, want %q", p.name, wire, got, want)
		}
	}
}

// TestFossilEscapeRoundTrip checks that every §4.3 escape survives an
// encode/decode round trip byte-for-byte.
func TestFossilEscapeRoundTrip(t *testing.T) {
	for _, p := range escapePairs {
		raw := "before" + string(p.raw) + "after"
		if got := FossilDecode(FossilEncode(raw)); got != raw {
			t.Errorf("%s: round trip = %q, want %q", p.name, got, raw)
		}
	}
}

// TestFossilDecodeUnknownEscapeDropsBackslash checks the §4.3 rule that an
// unrecognized escape sequence decodes by dropping the backslash and keeping
// the following character literally (matches canonical fossil defossilize).
func TestFossilDecodeUnknownEscapeDropsBackslash(t *testing.T) {
	if got, want := FossilDecode(`back\qslash`), "backqslash"; got != want {
		t.Errorf("FossilDecode(%q) = %q, want %q", `back\qslash`, got, want)
	}
}

// TestFossilDecodeTrailingBackslash checks that a lone trailing backslash (which
// canonical fossil never emits, since backslash always encodes to \\) is left
// literal rather than swallowing a nonexistent following byte.
func TestFossilDecodeTrailingBackslash(t *testing.T) {
	if got, want := FossilDecode(`trail\`), `trail\`; got != want {
		t.Errorf("FossilDecode(%q) = %q, want %q", `trail\`, got, want)
	}
}

// TestFossilDecodeCanonicalCard checks decode against the exact C-card wire bytes
// a real fossil 2.28 check-in produced for a comment containing a TAB, spaces, a
// backslash, a VT and an FF, and that the value re-marshals to identical bytes.
// How: the wire string below was copied verbatim from `fossil artifact tip`.
func TestFossilDecodeCanonicalCard(t *testing.T) {
	wire := `tab\there\sand\sback\\slash\sand\svt\vff\f`
	want := "tab\there and back\\slash and vt\vff\f"
	got := FossilDecode(wire)
	if got != want {
		t.Fatalf("FossilDecode(%q) = %q, want %q", wire, got, want)
	}
	if remarshal := FossilEncode(got); remarshal != wire {
		t.Errorf("re-marshal = %q, want identical wire %q", remarshal, wire)
	}
}

// TestFossilControlBytesRemarshalIdentical checks the acceptance criterion that a
// field value carrying real TAB/CR/VT/FF/NUL bytes decodes and then re-marshals
// to the same wire bytes it came from.
func TestFossilControlBytesRemarshalIdentical(t *testing.T) {
	wire := `a\tb\rc\vd\fe\0f`
	decoded := FossilDecode(wire)
	want := "a\tb\rc\vd\fe\x00f"
	if decoded != want {
		t.Fatalf("FossilDecode(%q) = %q, want %q", wire, decoded, want)
	}
	if got := FossilEncode(decoded); got != wire {
		t.Errorf("FossilEncode(FossilDecode(%q)) = %q, want %q", wire, got, wire)
	}
}
