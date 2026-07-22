package deck

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestArtifactTypeConstants(t *testing.T) {
	if Checkin != 0 {
		t.Fatalf("Checkin = %d, want 0", Checkin)
	}
	if Control != 7 {
		t.Fatalf("Control = %d, want 7", Control)
	}
}

func TestDeckZeroValue(t *testing.T) {
	var d Deck
	if d.Type != Checkin {
		t.Fatalf("zero Deck.Type = %d, want Checkin(0)", d.Type)
	}
	if len(d.F) != 0 {
		t.Fatal("zero Deck.F should be empty")
	}
	if !d.D.IsZero() {
		t.Fatal("zero Deck.D should be zero time")
	}
}

func TestTagTypeConstants(t *testing.T) {
	if TagSingleton != '+' {
		t.Fatalf("TagSingleton = %c, want +", TagSingleton)
	}
	if TagPropagating != '*' {
		t.Fatalf("TagPropagating = %c, want *", TagPropagating)
	}
	if TagCancel != '-' {
		t.Fatalf("TagCancel = %c, want -", TagCancel)
	}
}

func TestFossilEncode(t *testing.T) {
	tests := []struct{ in, want string }{
		{"hello", "hello"},
		{"hello world", `hello\sworld`},
		{"line\nbreak", `line\nbreak`},
		{`back\slash`, `back\\slash`},
		{"a b\nc\\d", `a\sb\nc\\d`},
		{"", ""},
	}
	for _, tt := range tests {
		got := FossilEncode(tt.in)
		if got != tt.want {
			t.Errorf("FossilEncode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFossilDecode(t *testing.T) {
	tests := []struct{ in, want string }{
		{"hello", "hello"},
		{`hello\sworld`, "hello world"},
		{`line\nbreak`, "line\nbreak"},
		{`back\\slash`, `back\slash`},
		{`a\sb\nc\\d`, "a b\nc\\d"},
		{"", ""},
	}
	for _, tt := range tests {
		got := FossilDecode(tt.in)
		if got != tt.want {
			t.Errorf("FossilDecode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFossilEncodeDecodeRoundTrip(t *testing.T) {
	inputs := []string{"simple", "has spaces", "has\nnewlines", `has\backslash`, ""}
	for _, s := range inputs {
		if got := FossilDecode(FossilEncode(s)); got != s {
			t.Errorf("round-trip(%q) = %q", s, got)
		}
	}
}

func TestVerifyZ(t *testing.T) {
	body := "D 2024-01-15T10:30:00.000\nU testuser\n"
	h := md5.Sum([]byte(body))
	zLine := fmt.Sprintf("Z %x\n", h)
	manifest := []byte(body + zLine)
	if err := VerifyZ(manifest); err != nil {
		t.Fatalf("VerifyZ failed on valid manifest: %v", err)
	}
}

func TestVerifyZBadChecksum(t *testing.T) {
	bad := []byte("D 2024-01-15T10:30:00.000\nU testuser\nZ 00000000000000000000000000000000\n")
	if err := VerifyZ(bad); err == nil {
		t.Fatal("VerifyZ should fail on bad checksum")
	}
}

func TestVerifyZTooShort(t *testing.T) {
	if err := VerifyZ([]byte("short")); err == nil {
		t.Fatal("VerifyZ should fail on short input")
	}
}

func TestMarshalMinimalCheckin(t *testing.T) {
	d := &Deck{
		Type: Checkin,
		C:    "initial commit",
		D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		F:    []FileCard{{Name: "hello.txt", UUID: "da39a3ee5e6b4b0d3255bfef95601890afd80709"}},
		R:    "d41d8cd98f00b204e9800998ecf8427e",
		T: []TagCard{
			{Type: TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
		U: User("testuser"),
	}
	data, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := VerifyZ(data); err != nil {
		t.Fatalf("Z-card failed: %v", err)
	}
	s := string(data)
	if idx := strings.Index(s, "C "); idx > strings.Index(s, "D ") {
		t.Fatal("C after D — card ordering wrong")
	}
}

func TestMarshalDCardFormat(t *testing.T) {
	d := &Deck{Type: Checkin, D: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC), U: User("test")}
	data, _ := d.Marshal()
	if !strings.Contains(string(data), "D 2024-01-15T10:30:00.000\n") {
		t.Fatalf("D-card format wrong in:\n%s", data)
	}
}

func TestMarshalFossilEncoding(t *testing.T) {
	d := &Deck{
		Type: Checkin,
		C:    "fix the space bug",
		D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		U:    User("test user"),
	}
	data, _ := d.Marshal()
	s := string(data)
	if !strings.Contains(s, `C fix\sthe\sspace\sbug`) {
		t.Fatalf("C-card not encoded:\n%s", s)
	}
	if !strings.Contains(s, `U test\suser`) {
		t.Fatalf("U-card not encoded:\n%s", s)
	}
}

func TestMarshalWCard(t *testing.T) {
	d := &Deck{
		Type: Wiki,
		D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		L:    "TestPage",
		U:    User("test"),
		W:    []byte("Hello wiki world"),
	}
	data, _ := d.Marshal()
	if !strings.Contains(string(data), "W 16\nHello wiki world\n") {
		t.Fatalf("W-card wrong:\n%s", data)
	}
}

// TestMarshalRenameFCardPlaceholder pins the F-card serialization for a
// rename (#51). Canonical Fossil forces a " w" permission placeholder when a
// renamed file's perm would otherwise be empty, so the prior-name field stays
// in its 4th positional slot (src/checkin.c:1999). An executable rename keeps
// its real perm instead of the placeholder.
func TestMarshalRenameFCardPlaceholder(t *testing.T) {
	const uuid = "a8009a7a528d87778c356da3a55d964719e818666a04e4f960c9e2439e35f138"
	cases := []struct {
		name string
		card FileCard
		want string
	}{
		{
			name: "empty-perm-forces-w",
			card: FileCard{Name: "greet.txt", UUID: uuid, OldName: "hello.txt"},
			want: "F greet.txt " + uuid + " w hello.txt\n",
		},
		{
			name: "executable-rename-keeps-x",
			card: FileCard{Name: "run.sh", UUID: uuid, Perm: "x", OldName: "old.sh"},
			want: "F run.sh " + uuid + " x old.sh\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Deck{
				Type: Checkin,
				D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				F:    []FileCard{c.card},
				U:    User("test"),
			}
			data, err := d.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.Contains(string(data), c.want) {
				t.Fatalf("F-card wrong: want %q in:\n%s", c.want, data)
			}
		})
	}
}

// --- Task 8: Parse tests ---

func TestParseMinimalCheckin(t *testing.T) {
	d := &Deck{
		Type: Checkin,
		C:    "initial commit",
		D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		F:    []FileCard{{Name: "hello.txt", UUID: "da39a3ee5e6b4b0d3255bfef95601890afd80709"}},
		R:    "d41d8cd98f00b204e9800998ecf8427e",
		T: []TagCard{
			{Type: TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
		U: User("testuser"),
	}
	data, _ := d.Marshal()
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.C != "initial commit" {
		t.Fatalf("C = %q", parsed.C)
	}
	if len(parsed.F) != 1 || parsed.F[0].Name != "hello.txt" {
		t.Fatalf("F = %+v", parsed.F)
	}
	if len(parsed.T) != 2 {
		t.Fatalf("T count = %d", len(parsed.T))
	}
}

func TestParseFossilEncodedFields(t *testing.T) {
	d := &Deck{
		Type: Checkin,
		C:    "fix the space bug",
		D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		U:    User("test user"),
	}
	data, _ := d.Marshal()
	parsed, _ := Parse(data)
	if parsed.C != "fix the space bug" {
		t.Fatalf("C = %q, want decoded", parsed.C)
	}
	if parsed.U == nil || *parsed.U != "test user" {
		t.Fatalf("U = %v, want decoded", parsed.U)
	}
}

// TestParseUCardStates exercises the three U-card presence states a
// manifest can carry: wholly absent, present-but-empty, and present with
// a value. Canonical fossil resolves the empty case to "anonymous" at
// parse time (src/manifest.c:1008-1016); a wholly absent U-card must stay
// distinguishable (nil) so crosslink can bind SQL NULL instead of a bare
// empty string. Regression coverage for issue #50.
func TestParseUCardStates(t *testing.T) {
	body := "D 2024-01-15T10:30:00.000\n"
	h := md5.Sum([]byte(body))
	absent, err := Parse([]byte(fmt.Sprintf("%sZ %x\n", body, h)))
	if err != nil {
		t.Fatalf("Parse (absent U-card): %v", err)
	}
	if absent.U != nil {
		t.Fatalf("absent U-card: U = %v, want nil", *absent.U)
	}

	emptyBody := "D 2024-01-15T10:30:00.000\nU \n"
	h = md5.Sum([]byte(emptyBody))
	present, err := Parse([]byte(fmt.Sprintf("%sZ %x\n", emptyBody, h)))
	if err != nil {
		t.Fatalf("Parse (empty U-card): %v", err)
	}
	if present.U == nil || *present.U != "anonymous" {
		t.Fatalf("empty U-card: U = %v, want \"anonymous\"", present.U)
	}

	valueBody := "D 2024-01-15T10:30:00.000\nU alice\n"
	h = md5.Sum([]byte(valueBody))
	valued, err := Parse([]byte(fmt.Sprintf("%sZ %x\n", valueBody, h)))
	if err != nil {
		t.Fatalf("Parse (valued U-card): %v", err)
	}
	if valued.U == nil || *valued.U != "alice" {
		t.Fatalf("valued U-card: U = %v, want \"alice\"", valued.U)
	}

	// Whitespace-only U-card: canonical fossil's next_token() skips leading
	// whitespace, so "U   " yields zUser==NULL and resolves to "anonymous"
	// the same as a truly empty U-card — not literal whitespace.
	wsBody := "D 2024-01-15T10:30:00.000\nU   \n"
	h = md5.Sum([]byte(wsBody))
	whitespace, err := Parse([]byte(fmt.Sprintf("%sZ %x\n", wsBody, h)))
	if err != nil {
		t.Fatalf("Parse (whitespace-only U-card): %v", err)
	}
	if whitespace.U == nil || *whitespace.U != "anonymous" {
		t.Fatalf("whitespace-only U-card: U = %v, want \"anonymous\"", whitespace.U)
	}
}

func TestParseWikiManifest(t *testing.T) {
	d := &Deck{
		Type: Wiki,
		D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		L:    "TestPage",
		U:    User("admin"),
		W:    []byte("Hello wiki content"),
	}
	data, _ := d.Marshal()
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.L != "TestPage" {
		t.Fatalf("L = %q", parsed.L)
	}
	if string(parsed.W) != "Hello wiki content" {
		t.Fatalf("W = %q", parsed.W)
	}
}

func TestParseBadZCard(t *testing.T) {
	_, err := Parse([]byte("D 2024-01-15T10:30:00.000\nU test\nZ 00000000000000000000000000000000\n"))
	if err == nil {
		t.Fatal("should fail on bad Z-card")
	}
}

func TestParseCardOrdering(t *testing.T) {
	body := "U test\nD 2024-01-15T10:30:00.000\n"
	h := md5.Sum([]byte(body))
	manifest := fmt.Sprintf("%sZ %x\n", body, h)
	_, err := Parse([]byte(manifest))
	if err == nil {
		t.Fatal("should reject out-of-order cards")
	}
}

// --- Task 9: R-card tests ---

func TestComputeREmpty(t *testing.T) {
	d := &Deck{Type: Checkin}
	r, err := d.ComputeR(nil)
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	if r != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("R = %q, want md5('')", r)
	}
}

func TestComputeRSingleFile(t *testing.T) {
	content := []byte("hello world")
	d := &Deck{
		Type: Checkin,
		F:    []FileCard{{Name: "hello.txt", UUID: "abc123"}},
	}
	getContent := func(uuid string) ([]byte, error) {
		if uuid == "abc123" {
			return content, nil
		}
		return nil, fmt.Errorf("unknown: %s", uuid)
	}
	r, err := d.ComputeR(getContent)
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	h := md5.New()
	h.Write([]byte("hello.txt"))
	h.Write([]byte(fmt.Sprintf(" %d\n", len(content))))
	h.Write(content)
	want := fmt.Sprintf("%x", h.Sum(nil))
	if r != want {
		t.Fatalf("R = %q, want %q", r, want)
	}
}

func TestComputeRSortedByName(t *testing.T) {
	files := map[string][]byte{"uuid-a": []byte("aaa"), "uuid-b": []byte("bbb")}
	d := &Deck{
		Type: Checkin,
		F: []FileCard{
			{Name: "b.txt", UUID: "uuid-b"},
			{Name: "a.txt", UUID: "uuid-a"},
		},
	}
	getContent := func(uuid string) ([]byte, error) { return files[uuid], nil }
	r, _ := d.ComputeR(getContent)
	h := md5.New()
	h.Write([]byte("a.txt"))
	h.Write([]byte(" 3\n"))
	h.Write([]byte("aaa"))
	h.Write([]byte("b.txt"))
	h.Write([]byte(" 3\n"))
	h.Write([]byte("bbb"))
	if r != fmt.Sprintf("%x", h.Sum(nil)) {
		t.Fatalf("R mismatch — files not sorted?")
	}
}

// --- Task 10: Round-trip tests and benchmarks ---

func TestRoundTripCheckin(t *testing.T) {
	d := &Deck{
		Type: Checkin,
		C:    "test with spaces and\nnewlines",
		D:    time.Date(2024, 6, 15, 14, 30, 45, 123000000, time.UTC),
		F: []FileCard{
			{Name: "src/main.go", UUID: "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
			{Name: "README.md", UUID: "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d", Perm: "x"},
		},
		P: []string{"1234567890123456789012345678901234567890"},
		R: "d41d8cd98f00b204e9800998ecf8427e",
		T: []TagCard{{Type: TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}},
		U: User("developer"),
	}
	data1, _ := d.Marshal()
	parsed, err := Parse(data1)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	data2, _ := parsed.Marshal()
	if !bytes.Equal(data1, data2) {
		t.Fatalf("round-trip mismatch:\n%s\nvs\n%s", data1, data2)
	}
}

func TestRoundTripWiki(t *testing.T) {
	d := &Deck{
		Type: Wiki,
		D:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		L:    "Test Page",
		N:    "text/x-markdown",
		U:    User("admin"),
		W:    []byte("# Hello\n\nWiki content."),
	}
	data1, _ := d.Marshal()
	parsed, _ := Parse(data1)
	data2, _ := parsed.Marshal()
	if !bytes.Equal(data1, data2) {
		t.Fatalf("wiki round-trip mismatch")
	}
}

func BenchmarkMarshal(b *testing.B) {
	d := &Deck{
		Type: Checkin,
		C:    "benchmark",
		D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		P:    []string{"1234567890123456789012345678901234567890"},
		R:    "d41d8cd98f00b204e9800998ecf8427e",
		T:    []TagCard{{Type: TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}},
		U:    User("benchuser"),
	}
	for i := 0; i < 50; i++ {
		d.F = append(d.F, FileCard{
			Name: fmt.Sprintf("src/file%03d.go", i),
			UUID: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Marshal()
	}
}

func BenchmarkParse(b *testing.B) {
	d := &Deck{
		Type: Checkin,
		C:    "benchmark",
		D:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		P:    []string{"1234567890123456789012345678901234567890"},
		R:    "d41d8cd98f00b204e9800998ecf8427e",
		T:    []TagCard{{Type: TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}},
		U:    User("benchuser"),
	}
	for i := 0; i < 50; i++ {
		d.F = append(d.F, FileCard{
			Name: fmt.Sprintf("src/file%03d.go", i),
			UUID: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		})
	}
	data, _ := d.Marshal()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Parse(data)
	}
}
