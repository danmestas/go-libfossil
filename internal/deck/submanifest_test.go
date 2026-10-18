package deck

import (
	"strings"
	"testing"
)

// These tests pin the card-level half of draft-fossil-submanifests-00: the S
// card (§3), the SUBMANIFEST type and its reassignment rule (§4.3), the
// legality table (§4.2) and the per-type semantic checks (§4.4). Each input
// differs from an accepted one by the single property under test, so nothing
// but that property can explain a rejection.

const (
	smH1 = "aa9d404c14fe4dadd2ab398961fb0dfe650a877c061f650d8233a5888e920acf"
	smH2 = "a50a722b465ed67b42ee0fbe61e4a19d6b1d22940ba5740e99e450921e3f52ea"
	smH3 = "0123456789abcdef0123456789abcdef01234567"
)

func mustParse(t *testing.T, body string) *Deck {
	t.Helper()
	d, err := Parse(withZCard(body))
	if err != nil {
		t.Fatalf("Parse(%q): %v", body, err)
	}
	return d
}

func mustReject(t *testing.T, body, wantSubstring string) {
	t.Helper()
	_, err := Parse(withZCard(body))
	if err == nil {
		t.Fatalf("Parse(%q) accepted, want rejection", body)
	}
	if wantSubstring != "" && !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("Parse(%q) error %q, want it to mention %q", body, err, wantSubstring)
	}
}

func TestTreeManifestParsesSCards(t *testing.T) {
	d := mustParse(t, "C tree\nD 2026-07-26T00:00:00\nS README.md "+smH1+
		"\nS src/node11.c "+smH2+"\nU drh\n")
	if d.Type != Checkin {
		t.Fatalf("type %v, want Checkin", d.Type)
	}
	if len(d.S) != 2 {
		t.Fatalf("len(S) = %d, want 2", len(d.S))
	}
	if d.S[1].Name != "src/node11.c" || d.S[1].UUID != smH2 {
		t.Fatalf("S[1] = %+v", d.S[1])
	}
}

func TestSubmanifestTypeByReassignment(t *testing.T) {
	leaf := mustParse(t, "F README.md "+smH1+"\nF src/main.c "+smH2+"\n")
	if leaf.Type != Submanifest {
		t.Fatalf("leaf type %v, want Submanifest", leaf.Type)
	}
	interior := mustParse(t, "S a "+smH1+"\nS b "+smH2+"\n")
	if interior.Type != Submanifest {
		t.Fatalf("interior type %v, want Submanifest", interior.Type)
	}
}

func TestSCardDecodesFilenameNotHash(t *testing.T) {
	d := mustParse(t, `S a\sb `+smH1+"\n")
	if d.S[0].Name != "a b" {
		t.Fatalf("S name %q, want decoded %q", d.S[0].Name, "a b")
	}
	mustReject(t, `S a `+`aa9d404c14fe4dadd2ab398961fb0dfe650a877c061f650d8233a5888e920ac\s`+"\n", "hash")
}

func TestSCardArityAndShape(t *testing.T) {
	mustReject(t, "S a\n", "S-card")
	mustReject(t, "S a "+smH1+" x\n", "S-card")
	mustReject(t, "S a 0123\n", "hash")
	mustReject(t, "S ../a "+smH1+"\n", "filename")
}

func TestSRunStrictlyAscending(t *testing.T) {
	mustReject(t, "S b "+smH1+"\nS a "+smH2+"\n", "order")
	mustReject(t, "S a "+smH1+"\nS a "+smH2+"\n", "order")
}

func TestSAcceptsSha1AndUppercaseHash(t *testing.T) {
	mustParse(t, "S a "+smH3+"\n")
	mustParse(t, "S a "+strings.ToUpper(smH1)+"\n")
}

func TestManifestRejectsFAndS(t *testing.T) {
	mustReject(t, "D 2026-07-26T00:00:00\nF a "+smH3+"\nS b "+smH1+"\n", "")
}

func TestManifestRejectsBAndS(t *testing.T) {
	mustReject(t, "B "+smH1+"\nD 2026-07-26T00:00:00\nS b "+smH2+"\n", "")
}

func TestSubmanifestRejectsFAndS(t *testing.T) {
	mustReject(t, "F a "+smH3+"\nS b "+smH1+"\n", "")
}

func TestSubmanifestRejectsOtherCards(t *testing.T) {
	mustReject(t, "C x\nF a "+smH3+"\n", "not allowed")
	mustReject(t, "F a "+smH3+"\nU drh\n", "not allowed")
}

func TestSubmanifestRejectsDeleteMarker(t *testing.T) {
	mustReject(t, "F a\n", "hash")
}

func TestDLessManifestStillRejected(t *testing.T) {
	// A C-only, D-less artifact was a manifest missing its D card under
	// artifact-format-01; the reassignment makes it a sub-manifest with an
	// illegal C card. Either way it is rejected.
	mustReject(t, "C x\nP "+smH3+"\n", "")
}

func TestSCardOrderAgainstTAndR(t *testing.T) {
	// §3.4: S falls between R and T.
	mustReject(t, "D 2026-07-26T00:00:00\nT +x *\nS a "+smH1+"\nU u\n", "out of order")
	mustParse(t, "D 2026-07-26T00:00:00\nR 0123456789abcdef0123456789abcdef\nS a "+smH1+"\nT +x *\nU u\n")
}

func TestTreeManifestRoundTrips(t *testing.T) {
	body := "C tree\nD 2026-07-26T00:00:00.000\nS README.md " + smH1 +
		"\nS src/node11.c " + smH2 + "\nU drh\n"
	d := mustParse(t, body)
	out, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(withZCard(body)) {
		t.Fatalf("round trip:\n got %q\nwant %q", out, withZCard(body))
	}
	leafBody := "F a\\sb " + smH1 + "\nF c " + smH2 + "\n"
	leaf := mustParse(t, leafBody)
	out, err = leaf.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(withZCard(leafBody)) {
		t.Fatalf("leaf round trip:\n got %q\nwant %q", out, withZCard(leafBody))
	}
}

func TestUndefinedLettersStillRejected(t *testing.T) {
	for _, letter := range []string{"O", "V", "X", "Y"} {
		mustReject(t, "D 2026-07-26T00:00:00\n"+letter+" x\n", "")
	}
}

// §6.2: canonical emission escapes exactly SP, LF and backslash; the
// decoder's other escape octets are written raw.
func TestCanonicalEncodeIsMinimal(t *testing.T) {
	cases := map[string]string{
		"a b":        `a\sb`,
		"a\nb":       `a\nb`,
		`a\b`:        `a\\b`,
		"a\tb\rc":    "a\tb\rc",
		"v\vf\f\x00": "v\vf\f\x00",
		"plain.c":    "plain.c",
	}
	for in, want := range cases {
		if got := CanonicalEncode(in); got != want {
			t.Errorf("CanonicalEncode(%q) = %q, want %q", in, got, want)
		}
		if back := FossilDecode(CanonicalEncode(in)); back != in {
			t.Errorf("FossilDecode(CanonicalEncode(%q)) = %q", in, back)
		}
	}
}

// Appendix A.2: the first leaf of the example tree, byte for byte.
func TestMarshalLeafReproducesAppendixA2(t *testing.T) {
	got := MarshalLeaf([]FileCard{
		{Name: "README.md", UUID: "a4ab3d6bba47168d231f8ebdfaeb9bce15e134d7150bf20800a42551409bfbfb"},
		{Name: "src/main.c", UUID: "ef30bf8ab404da88c777979d0eeb8729f59cff33423382cfb212ed7b70a7f31c"},
	})
	want := "F README.md a4ab3d6bba47168d231f8ebdfaeb9bce15e134d7150bf20800a42551409bfbfb\n" +
		"F src/main.c ef30bf8ab404da88c777979d0eeb8729f59cff33423382cfb212ed7b70a7f31c\n" +
		"Z bdbb5692981c3a9e28c8320434534bf1\n"
	if string(got) != want {
		t.Fatalf("MarshalLeaf:\n got %q\nwant %q", got, want)
	}
}

// §6.2: a leaf writes its filename and old-name tokens canonically, keeps
// the permission token, and forces the "w" placeholder on a rename.
func TestMarshalLeafCanonicalTokens(t *testing.T) {
	got := MarshalLeaf([]FileCard{
		{Name: "a b\tc", UUID: smH1, Perm: "x"},
		{Name: "d", UUID: smH2, OldName: "old name"},
	})
	body := "F a\\sb\tc " + smH1 + " x\nF d " + smH2 + " w old\\sname\n"
	if string(got) != string(withZCard(body)) {
		t.Fatalf("MarshalLeaf:\n got %q\nwant %q", got, withZCard(body))
	}
	d := mustParse(t, body)
	if d.Type != Submanifest || d.F[0].Name != "a b\tc" || d.F[1].OldName != "old name" {
		t.Fatalf("leaf parses as %+v", d)
	}
}

// §6.2 step 3: an interior sub-manifest is one S card per member and a Z
// card; S filenames are canonical.
func TestMarshalInterior(t *testing.T) {
	got := MarshalInterior([]SubmanifestCard{{Name: "a\tb", UUID: smH1}, {Name: "c d", UUID: smH2}})
	body := "S a\tb " + smH1 + "\nS c\\sd " + smH2 + "\n"
	if string(got) != string(withZCard(body)) {
		t.Fatalf("MarshalInterior:\n got %q\nwant %q", got, withZCard(body))
	}
	if d := mustParse(t, body); d.Type != Submanifest || d.S[0].Name != "a\tb" {
		t.Fatalf("interior parses as %+v", d)
	}
}

// A tree manifest's S filenames are canonical too (§6.2), so a manifest the
// writer builds and the one it marshals agree byte for byte.
func TestManifestSCardsMarshalCanonically(t *testing.T) {
	d := &Deck{D: mustParse(t, "D 2026-07-26T00:00:00\nS a "+smH1+"\nS b "+smH2+"\n").D,
		S: []SubmanifestCard{{Name: "a\tb", UUID: smH1}, {Name: "c d", UUID: smH2}}}
	out, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "S a\tb "+smH1+"\nS c\\sd "+smH2+"\n") {
		t.Fatalf("manifest S cards not canonical: %q", out)
	}
}
