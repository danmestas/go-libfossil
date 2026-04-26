package xfer

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// bufioReaderFromBytes is a helper for benchmarks that creates a bufio.Reader from a byte slice.
func bufioReaderFromBytes(data []byte) *bufio.Reader {
	return bufio.NewReader(bytes.NewReader(data))
}

// --- Task 8: Message Encode/Decode ---

func TestMessage_MultiCardRoundTrip(t *testing.T) {
	msg := &Message{
		Cards: []Card{
			&PragmaCard{Name: "client-version", Values: []string{"2.24"}},
			&PullCard{ProjectCode: "proj1", ServerCode: "srv1"},
			&IGotCard{UUID: "abc123def456abc123def456abc123def456abcd"},
			&GimmeCard{UUID: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		},
	}
	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Cards) != len(msg.Cards) {
		t.Fatalf("card count = %d, want %d", len(got.Cards), len(msg.Cards))
	}
	// Verify types
	if _, ok := got.Cards[0].(*PragmaCard); !ok {
		t.Errorf("card 0: got %T, want *PragmaCard", got.Cards[0])
	}
	if _, ok := got.Cards[1].(*PullCard); !ok {
		t.Errorf("card 1: got %T, want *PullCard", got.Cards[1])
	}
	if _, ok := got.Cards[2].(*IGotCard); !ok {
		t.Errorf("card 2: got %T, want *IGotCard", got.Cards[2])
	}
	if _, ok := got.Cards[3].(*GimmeCard); !ok {
		t.Errorf("card 3: got %T, want *GimmeCard", got.Cards[3])
	}
	// Verify values
	pragma := got.Cards[0].(*PragmaCard)
	if pragma.Name != "client-version" || len(pragma.Values) != 1 || pragma.Values[0] != "2.24" {
		t.Errorf("pragma = %+v", pragma)
	}
	pull := got.Cards[1].(*PullCard)
	if pull.ProjectCode != "proj1" || pull.ServerCode != "srv1" {
		t.Errorf("pull = %+v", pull)
	}
}

func TestMessage_UncompressedRoundTrip(t *testing.T) {
	msg := &Message{
		Cards: []Card{
			&PragmaCard{Name: "sym-pressure", Values: []string{"100"}},
			&IGotCard{UUID: "abc123"},
			&GimmeCard{UUID: "def456"},
		},
	}
	data, err := msg.EncodeUncompressed()
	if err != nil {
		t.Fatalf("EncodeUncompressed: %v", err)
	}
	got, err := DecodeUncompressed(data)
	if err != nil {
		t.Fatalf("DecodeUncompressed: %v", err)
	}
	if len(got.Cards) != 3 {
		t.Fatalf("card count = %d, want 3", len(got.Cards))
	}
	if g := got.Cards[1].(*IGotCard); g.UUID != "abc123" {
		t.Errorf("igot UUID = %q, want %q", g.UUID, "abc123")
	}
	if g := got.Cards[2].(*GimmeCard); g.UUID != "def456" {
		t.Errorf("gimme UUID = %q, want %q", g.UUID, "def456")
	}
}

func TestMessage_WithPayloadCards(t *testing.T) {
	manifest := []byte("C initial\\ncheckin\nD 2024-01-01T00:00:00\nP\nR abc123\nU admin\nZ abcdef1234567890")
	msg := &Message{
		Cards: []Card{
			&FileCard{UUID: "abc123def456", Content: manifest},
			&IGotCard{UUID: "abc123def456"},
			&CFileCard{UUID: "def789ghi012", Content: []byte("compressed content data here for testing")},
		},
	}
	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Cards) != 3 {
		t.Fatalf("card count = %d, want 3", len(got.Cards))
	}
	fc := got.Cards[0].(*FileCard)
	if !bytes.Equal(fc.Content, manifest) {
		t.Errorf("file content mismatch: got %d bytes, want %d", len(fc.Content), len(manifest))
	}
	ig := got.Cards[1].(*IGotCard)
	if ig.UUID != "abc123def456" {
		t.Errorf("igot UUID = %q", ig.UUID)
	}
	cf := got.Cards[2].(*CFileCard)
	if !bytes.Equal(cf.Content, []byte("compressed content data here for testing")) {
		t.Errorf("cfile content mismatch: got %q", cf.Content)
	}
}

func TestMessage_Empty(t *testing.T) {
	msg := &Message{}
	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("Encode empty: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode empty: %v", err)
	}
	if len(got.Cards) != 0 {
		t.Errorf("card count = %d, want 0", len(got.Cards))
	}
}

func TestMessage_EmptyUncompressed(t *testing.T) {
	msg := &Message{}
	data, err := msg.EncodeUncompressed()
	if err != nil {
		t.Fatalf("EncodeUncompressed empty: %v", err)
	}
	got, err := DecodeUncompressed(data)
	if err != nil {
		t.Fatalf("DecodeUncompressed empty: %v", err)
	}
	if len(got.Cards) != 0 {
		t.Errorf("card count = %d, want 0", len(got.Cards))
	}
}

func TestMessage_CompressionReducesSize(t *testing.T) {
	// 100 identical igot cards should compress very well
	cards := make([]Card, 100)
	for i := range cards {
		cards[i] = &IGotCard{UUID: "abc123def456abc123def456abc123def456abcd"}
	}
	msg := &Message{Cards: cards}

	uncompressed, err := msg.EncodeUncompressed()
	if err != nil {
		t.Fatalf("EncodeUncompressed: %v", err)
	}
	compressed, err := msg.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if len(compressed) >= len(uncompressed) {
		t.Errorf("compressed (%d) should be smaller than uncompressed (%d)",
			len(compressed), len(uncompressed))
	}
	t.Logf("uncompressed=%d bytes, compressed=%d bytes, ratio=%.1f%%",
		len(uncompressed), len(compressed),
		float64(len(compressed))*100/float64(len(uncompressed)))

	// Verify round-trip through compressed path
	got, err := Decode(compressed)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Cards) != 100 {
		t.Errorf("card count = %d, want 100", len(got.Cards))
	}
}

// --- Task 9: Integration Test ---

func TestMessage_RealisticSyncTraffic(t *testing.T) {
	// Construct a realistic multi-card message mimicking actual fossil sync traffic.
	// This is what a client would send during a pull operation.

	manifestContent := []byte(
		"C initial\\scheckin\n" +
			"D 2024-01-15T10:30:00\n" +
			"F src/main.go abc123def456abc123def456abc123def456abcd\n" +
			"F src/util.go fedcba654321fedcba654321fedcba654321fedc\n" +
			"P\n" +
			"R abc123def456abc123def456abc123def456abcd\n" +
			"U admin\n" +
			"Z 0123456789abcdef0123456789abcdef\n")

	cards := []Card{
		// Protocol negotiation
		&PragmaCard{Name: "client-version", Values: []string{"2.24"}},
		&PragmaCard{Name: "http-auth", Values: []string{"1"}},
		// Pull request
		&PullCard{
			ServerCode:  "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			ProjectCode: "f6e5d4c3b2a1f6e5d4c3b2a1f6e5d4c3b2a1f6e5",
		},
		// Authentication
		&LoginCard{
			User:      "anonymous",
			Nonce:     "1705312200",
			Signature: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		},
		// Session cookie
		&CookieCard{Value: "B26F5C9E901344E8/1705312200/anonymous"},
		// Artifacts we already have
		&IGotCard{UUID: "abc123def456abc123def456abc123def456abcd"},
		&IGotCard{UUID: "111111111111111111111111111111111111abcd"},
		&IGotCard{UUID: "222222222222222222222222222222222222abcd"},
		&IGotCard{UUID: "333333333333333333333333333333333333abcd"},
		&IGotCard{UUID: "444444444444444444444444444444444444abcd", IsPrivate: true},
		// Artifacts we want
		&GimmeCard{UUID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0001"},
		&GimmeCard{UUID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0002"},
		&GimmeCard{UUID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0003"},
		// A file card with manifest-like content
		&FileCard{
			UUID:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb0001",
			Content: manifestContent,
		},
	}

	msg := &Message{Cards: cards}

	// Encode (compressed)
	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	t.Logf("realistic sync message: %d cards, %d bytes compressed", len(cards), len(data))

	// Decode
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(got.Cards) != len(cards) {
		t.Fatalf("card count = %d, want %d", len(got.Cards), len(cards))
	}

	// Verify each card type survived
	expectations := []struct {
		idx  int
		kind string
	}{
		{0, "*xfer.PragmaCard"},
		{1, "*xfer.PragmaCard"},
		{2, "*xfer.PullCard"},
		{3, "*xfer.LoginCard"},
		{4, "*xfer.CookieCard"},
		{5, "*xfer.IGotCard"},
		{6, "*xfer.IGotCard"},
		{7, "*xfer.IGotCard"},
		{8, "*xfer.IGotCard"},
		{9, "*xfer.IGotCard"},
		{10, "*xfer.GimmeCard"},
		{11, "*xfer.GimmeCard"},
		{12, "*xfer.GimmeCard"},
		{13, "*xfer.FileCard"},
	}
	for _, e := range expectations {
		gotType := fmt.Sprintf("%T", got.Cards[e.idx])
		if gotType != e.kind {
			t.Errorf("card[%d] type = %s, want %s", e.idx, gotType, e.kind)
		}
	}

	// Verify specific field values
	pragma0 := got.Cards[0].(*PragmaCard)
	if pragma0.Name != "client-version" || pragma0.Values[0] != "2.24" {
		t.Errorf("pragma 0 = %+v", pragma0)
	}

	login := got.Cards[3].(*LoginCard)
	if login.User != "anonymous" {
		t.Errorf("login user = %q, want %q", login.User, "anonymous")
	}

	igotPrivate := got.Cards[9].(*IGotCard)
	if !igotPrivate.IsPrivate {
		t.Error("igot[9] should be private")
	}

	fc := got.Cards[13].(*FileCard)
	if !bytes.Equal(fc.Content, manifestContent) {
		t.Errorf("file content mismatch: got %d bytes, want %d", len(fc.Content), len(manifestContent))
	}

	// Also test uncompressed path
	rawData, err := msg.EncodeUncompressed()
	if err != nil {
		t.Fatalf("EncodeUncompressed: %v", err)
	}
	got2, err := DecodeUncompressed(rawData)
	if err != nil {
		t.Fatalf("DecodeUncompressed: %v", err)
	}
	if len(got2.Cards) != len(cards) {
		t.Errorf("uncompressed card count = %d, want %d", len(got2.Cards), len(cards))
	}
}

func TestMessage_FossilSanityCheck(t *testing.T) {
	// If fossil is available, create two repos and clone one from the other
	// to verify the fossil binary is working on this system.
	fossilPath, err := exec.LookPath("fossil")
	if err != nil {
		t.Skip("fossil not found in PATH, skipping sanity check")
	}

	dir := t.TempDir()
	repoA := dir + "/repo-a.fossil"
	repoB := dir + "/repo-b.fossil"
	workA := dir + "/work-a"

	// Create repo A
	out, err := exec.Command(fossilPath, "init", repoA).CombinedOutput()
	if err != nil {
		t.Fatalf("fossil init repo-a: %v\n%s", err, out)
	}

	// Open repo A in a working directory
	out, err = exec.Command("mkdir", "-p", workA).CombinedOutput()
	if err != nil {
		t.Fatalf("mkdir work-a: %v\n%s", err, out)
	}

	cmd := exec.Command(fossilPath, "open", repoA)
	cmd.Dir = workA
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil open: %v\n%s", err, out)
	}

	// Clone repo A to repo B using local file clone
	out, err = exec.Command(fossilPath, "clone", repoA, repoB).CombinedOutput()
	if err != nil {
		t.Fatalf("fossil clone: %v\n%s", err, out)
	}

	// Verify repo B exists and has correct project code
	outA, errA := exec.Command(fossilPath, "info", "-R", repoA).CombinedOutput()
	outB, errB := exec.Command(fossilPath, "info", "-R", repoB).CombinedOutput()
	if errA != nil {
		t.Fatalf("fossil info repo-a: %v\n%s", errA, outA)
	}
	if errB != nil {
		t.Fatalf("fossil info repo-b: %v\n%s", errB, outB)
	}

	// Both should contain the same project-code
	projectCodeA := extractProjectCode(string(outA))
	projectCodeB := extractProjectCode(string(outB))
	if projectCodeA == "" {
		t.Fatal("could not extract project-code from repo A")
	}
	if projectCodeA != projectCodeB {
		t.Errorf("project codes differ: A=%q, B=%q", projectCodeA, projectCodeB)
	}
	t.Logf("fossil sanity check passed: project-code=%s", projectCodeA)
}

// extractProjectCode extracts the project-code from fossil info output.
func extractProjectCode(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "project-code:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	return ""
}

// --- Task 10: Benchmarks ---

func BenchmarkEncodeMessage(b *testing.B) {
	// Build a ~50 card message
	cards := make([]Card, 0, 50)
	cards = append(cards, &PragmaCard{Name: "client-version", Values: []string{"2.24"}})
	cards = append(cards, &PragmaCard{Name: "http-auth", Values: []string{"1"}})
	cards = append(cards, &PullCard{ProjectCode: "proj1code", ServerCode: "srv1code"})
	cards = append(cards, &LoginCard{User: "anonymous", Nonce: "12345", Signature: "sig"})
	cards = append(cards, &CookieCard{Value: "cookie-value"})

	// 20 igot cards
	for i := 0; i < 20; i++ {
		cards = append(cards, &IGotCard{UUID: fmt.Sprintf("%040x", i)})
	}
	// 10 gimme cards
	for i := 0; i < 10; i++ {
		cards = append(cards, &GimmeCard{UUID: fmt.Sprintf("%040x", i+100)})
	}
	// 5 file cards with 1KB content
	content1K := bytes.Repeat([]byte("abcdefghijklmnop"), 64) // 1024 bytes
	for i := 0; i < 5; i++ {
		cards = append(cards, &FileCard{
			UUID:    fmt.Sprintf("%040x", i+200),
			Content: content1K,
		})
	}

	msg := &Message{Cards: cards}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := msg.Encode()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeMessage(b *testing.B) {
	// Same payload as BenchmarkEncodeMessage
	cards := make([]Card, 0, 50)
	cards = append(cards, &PragmaCard{Name: "client-version", Values: []string{"2.24"}})
	cards = append(cards, &PragmaCard{Name: "http-auth", Values: []string{"1"}})
	cards = append(cards, &PullCard{ProjectCode: "proj1code", ServerCode: "srv1code"})
	cards = append(cards, &LoginCard{User: "anonymous", Nonce: "12345", Signature: "sig"})
	cards = append(cards, &CookieCard{Value: "cookie-value"})
	for i := 0; i < 20; i++ {
		cards = append(cards, &IGotCard{UUID: fmt.Sprintf("%040x", i)})
	}
	for i := 0; i < 10; i++ {
		cards = append(cards, &GimmeCard{UUID: fmt.Sprintf("%040x", i+100)})
	}
	content1K := bytes.Repeat([]byte("abcdefghijklmnop"), 64)
	for i := 0; i < 5; i++ {
		cards = append(cards, &FileCard{
			UUID:    fmt.Sprintf("%040x", i+200),
			Content: content1K,
		})
	}

	msg := &Message{Cards: cards}
	data, err := msg.Encode()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Decode(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeFileCard(b *testing.B) {
	content := make([]byte, 1<<20) // 1MB
	rand.Read(content)
	c := &FileCard{UUID: "abc123def456abc123def456abc123def456abcd", Content: content}
	var buf bytes.Buffer
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := EncodeCard(&buf, c); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeFileCard(b *testing.B) {
	content := make([]byte, 1<<20) // 1MB
	rand.Read(content)
	c := &FileCard{UUID: "abc123def456abc123def456abc123def456abcd", Content: content}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		b.Fatal(err)
	}
	wire := buf.Bytes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := bufioReaderFromBytes(wire)
		_, err := DecodeCard(r)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeCFileCard(b *testing.B) {
	content := make([]byte, 1<<20) // 1MB
	rand.Read(content)
	c := &CFileCard{UUID: "abc123def456abc123def456abc123def456abcd", Content: content}
	var buf bytes.Buffer
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := EncodeCard(&buf, c); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeCFileCard(b *testing.B) {
	content := make([]byte, 1<<20) // 1MB
	rand.Read(content)
	c := &CFileCard{UUID: "abc123def456abc123def456abc123def456abcd", Content: content}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		b.Fatal(err)
	}
	wire := buf.Bytes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := bufioReaderFromBytes(wire)
		_, err := DecodeCard(r)
		if err != nil {
			b.Fatal(err)
		}
	}
}
