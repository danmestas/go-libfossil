package delta

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/testutil"
)

// TestFossilDeltaFormat_CopyCommand verifies count@offset order (was swapped).
func TestFossilDeltaFormat_CopyCommand(t *testing.T) {
	source := []byte("ABCDEFGHIJKLMNOP")
	target := []byte("EFGHIJKL") // copy 8 from offset 4
	cs := Checksum(target)

	var d []byte
	d = append(d, encodeTestInt(uint64(len(target)))...)
	d = append(d, '\n')
	d = append(d, encodeTestInt(8)...)
	d = append(d, '@')
	d = append(d, encodeTestInt(4)...)
	d = append(d, ',')
	d = append(d, encodeTestInt(uint64(cs))...)
	d = append(d, ';')

	got, err := Apply(source, d)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("Apply = %q, want %q", got, target)
	}
}

// TestFossilDeltaFormat_MixedCommands exercises copy + insert interleaving.
func TestFossilDeltaFormat_MixedCommands(t *testing.T) {
	source := []byte("The quick brown fox jumps over the lazy dog")
	target := []byte("The slow brown cat jumps over the lazy dog!")
	cs := Checksum(target)

	var d []byte
	d = append(d, encodeTestInt(uint64(len(target)))...)
	d = append(d, '\n')
	// copy 4@0, ("The ")
	d = append(d, encodeTestInt(4)...)
	d = append(d, '@')
	d = append(d, encodeTestInt(0)...)
	d = append(d, ',')
	// 4:slow
	d = append(d, encodeTestInt(4)...)
	d = append(d, ':')
	d = append(d, "slow"...)
	// copy 7@9, (" brown ")
	d = append(d, encodeTestInt(7)...)
	d = append(d, '@')
	d = append(d, encodeTestInt(9)...)
	d = append(d, ',')
	// 3:cat
	d = append(d, encodeTestInt(3)...)
	d = append(d, ':')
	d = append(d, "cat"...)
	// copy 24@19,
	d = append(d, encodeTestInt(24)...)
	d = append(d, '@')
	d = append(d, encodeTestInt(19)...)
	d = append(d, ',')
	// 1:!
	d = append(d, encodeTestInt(1)...)
	d = append(d, ':')
	d = append(d, '!')
	// checksum;
	d = append(d, encodeTestInt(uint64(cs))...)
	d = append(d, ';')

	got, err := Apply(source, d)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("Apply = %q, want %q", got, target)
	}
}

// TestFossilDeltaFormat_Asymmetric catches the count/offset swap bug.
// With the old bug, count=3 offset=10 would be misread as count=10 offset=3.
func TestFossilDeltaFormat_Asymmetric(t *testing.T) {
	source := []byte("0123456789ABCDEF")
	target := []byte("ABC") // copy 3 from offset 10
	cs := Checksum(target)

	var d []byte
	d = append(d, encodeTestInt(uint64(len(target)))...)
	d = append(d, '\n')
	d = append(d, encodeTestInt(3)...)
	d = append(d, '@')
	d = append(d, encodeTestInt(10)...)
	d = append(d, ',')
	d = append(d, encodeTestInt(uint64(cs))...)
	d = append(d, ';')

	got, err := Apply(source, d)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(got) != "ABC" {
		t.Fatalf("Apply = %q, want %q", got, target)
	}
}

// TestFossilCLI_DeltaRoundTrip uses `fossil test-delta-create` and
// `fossil test-delta-apply` to generate a ground-truth delta, then
// verifies our Apply produces identical output.
func TestFossilCLI_DeltaRoundTrip(t *testing.T) {
	fossilBin := testutil.RequireFossilBin(t)

	source := bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog. "), 100)
	target := make([]byte, len(source))
	copy(target, source)
	copy(target[500:], []byte("CHANGED CONTENT HERE — verifying delta interop!"))

	dir := t.TempDir()
	srcFile := filepath.Join(dir, "src.bin")
	tgtFile := filepath.Join(dir, "tgt.bin")
	deltaFile := filepath.Join(dir, "delta.bin")
	resultFile := filepath.Join(dir, "result.bin")

	if err := writeFile(srcFile, source); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(tgtFile, target); err != nil {
		t.Fatal(err)
	}

	// Create delta with fossil CLI
	if out, err := exec.Command(fossilBin, "test-delta-create", srcFile, tgtFile, deltaFile).CombinedOutput(); err != nil {
		t.Fatalf("fossil test-delta-create: %v\n%s", err, out)
	}

	fossilDelta, err := readFile(deltaFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Fossil-generated delta: %d bytes (source=%d, target=%d)", len(fossilDelta), len(source), len(target))

	// Apply with our code
	got, err := Apply(source, fossilDelta)
	if err != nil {
		t.Fatalf("Apply fossil delta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("output mismatch: got %d bytes, want %d", len(got), len(target))
	}

	// Also verify fossil's own apply produces same result
	if out, err := exec.Command(fossilBin, "test-delta-apply", srcFile, deltaFile, resultFile).CombinedOutput(); err != nil {
		t.Fatalf("fossil test-delta-apply: %v\n%s", err, out)
	}
	fossilResult, err := readFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fossilResult) {
		t.Fatal("our Apply disagrees with fossil test-delta-apply")
	}
}

// TestFossilCLI_CreateRoundTrip verifies that deltas we Create can be
// applied by the fossil CLI.
func TestFossilCLI_CreateRoundTrip(t *testing.T) {
	fossilBin := testutil.RequireFossilBin(t)

	source := bytes.Repeat([]byte("abcdefghijklmnop"), 500)
	target := make([]byte, len(source))
	copy(target, source)
	copy(target[2000:], []byte("OUR DELTA FORMAT VERIFIED"))

	dir := t.TempDir()
	srcFile := filepath.Join(dir, "src.bin")
	deltaFile := filepath.Join(dir, "delta.bin")
	resultFile := filepath.Join(dir, "result.bin")

	if err := writeFile(srcFile, source); err != nil {
		t.Fatal(err)
	}

	// Create delta with our code
	d := Create(source, target)
	if err := writeFile(deltaFile, d); err != nil {
		t.Fatal(err)
	}
	t.Logf("Our delta: %d bytes (source=%d, target=%d)", len(d), len(source), len(target))

	// Apply with fossil CLI
	if out, err := exec.Command(fossilBin, "test-delta-apply", srcFile, deltaFile, resultFile).CombinedOutput(); err != nil {
		t.Fatalf("fossil test-delta-apply: %v\n%s", err, out)
	}

	fossilResult, err := readFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fossilResult, target) {
		t.Fatalf("fossil couldn't apply our delta: got %d bytes, want %d", len(fossilResult), len(target))
	}
}

func encodeTestInt(v uint64) []byte {
	const enc = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz~"
	if v == 0 {
		return []byte{'0'}
	}
	var tmp [13]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = enc[v&0x3f]
		v >>= 6
	}
	return tmp[i:]
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
