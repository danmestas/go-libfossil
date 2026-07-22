package hash

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/testutil"
)

func TestSHA1(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
		{"hello", "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"},
		{"Fossil SCM", "a4304aff1fcb8a78d973db242692175b2e579612"},
	}
	for _, tt := range tests {
		got := SHA1([]byte(tt.input))
		if got != tt.want {
			t.Errorf("SHA1(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSHA3(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a"},
		{"hello", "3338be694f50c5f338814986cdf0686453a888b84f424d792af4b9202398f392"},
	}
	for _, tt := range tests {
		got := SHA3([]byte(tt.input))
		if got != tt.want {
			t.Errorf("SHA3(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSHA1_FossilValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fossil CLI validation in short mode")
	}
	input := []byte("test content for hashing")
	goHash := SHA1(input)

	tmpFile := filepath.Join(t.TempDir(), "testfile")
	os.WriteFile(tmpFile, input, 0644)

	bin := testutil.RequireFossilBin(t)
	out, err := exec.Command(bin, "sha1sum", tmpFile).Output()
	if err != nil {
		t.Fatalf("fossil sha1sum: %v", err)
	}
	fossilHash := strings.Fields(string(out))[0]
	if goHash != fossilHash {
		t.Fatalf("SHA1 mismatch: go=%q fossil=%q", goHash, fossilHash)
	}
}

func TestSHA3Format(t *testing.T) {
	got := SHA3([]byte("test"))
	if len(got) != 64 {
		t.Fatalf("SHA3 length = %d, want 64", len(got))
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("SHA3 contains non-lowercase-hex char: %c", c)
		}
	}
}

func BenchmarkSHA1(b *testing.B) {
	data := make([]byte, 10000)
	for i := range data {
		data[i] = byte(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		SHA1(data)
	}
}

func BenchmarkSHA3(b *testing.B) {
	data := make([]byte, 10000)
	for i := range data {
		data[i] = byte(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		SHA3(data)
	}
}
