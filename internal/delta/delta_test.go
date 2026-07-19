package delta

import (
	"bytes"
	"testing"
)

func TestApply_InsertOnly(t *testing.T) {
	source := []byte{}
	target := []byte("hello")
	cs := Checksum(target)
	delta := encodeDelta(uint64(len(target)), nil, target, cs)

	got, err := Apply(source, delta)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("Apply = %q, want %q", got, target)
	}
}

func TestApply_CopyFromSource(t *testing.T) {
	source := []byte("hello world")
	target := []byte("hello Go")
	cs := Checksum(target)
	delta := manualDelta(uint64(len(target)), []deltaOp{
		{opType: '@', offset: 0, length: 6},
		{opType: ':', data: []byte("Go")},
	}, cs)

	got, err := Apply(source, delta)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("Apply = %q, want %q", got, target)
	}
}

func TestApply_ChecksumMismatch(t *testing.T) {
	source := []byte{}
	target := []byte("hello")
	badChecksum := uint32(999999)
	delta := encodeDelta(uint64(len(target)), nil, target, badChecksum)

	_, err := Apply(source, delta)
	if err == nil {
		t.Fatal("expected checksum error")
	}
}

func TestApply_InvalidDelta(t *testing.T) {
	_, err := Apply([]byte{}, []byte{})
	if err == nil {
		t.Fatal("expected error on empty delta")
	}
}

// TestOutputSize verifies that OutputSize reads the target length straight
// out of a delta's header, without touching the source it was created
// against — this is what lets a receiver learn how large a delta will
// expand to before the base is available to apply it.
func TestOutputSize(t *testing.T) {
	source := []byte("original content here")
	target := []byte("original content modified further")
	d := Create(source, target)

	got, err := OutputSize(d)
	if err != nil {
		t.Fatalf("OutputSize: %v", err)
	}
	if got != uint64(len(target)) {
		t.Fatalf("OutputSize = %d, want %d", got, len(target))
	}
}

func TestOutputSize_InvalidDelta(t *testing.T) {
	_, err := OutputSize([]byte("not a delta"))
	if err == nil {
		t.Fatal("expected error for malformed delta header")
	}
}

func TestChecksum(t *testing.T) {
	data := []byte("hello")
	c1 := Checksum(data)
	c2 := Checksum(data)
	if c1 != c2 {
		t.Fatalf("Checksum not deterministic: %d != %d", c1, c2)
	}
	c0 := Checksum([]byte{})
	if c0 != 0 {
		t.Fatalf("Checksum(empty) = %d, want 0", c0)
	}
}

func TestCreate_SmallInputs(t *testing.T) {
	tests := []struct {
		name   string
		source string
		target string
	}{
		{"identical", "hello", "hello"},
		{"append", "hello", "hello world"},
		{"prepend", "world", "hello world"},
		{"replace", "aaaa", "bbbb"},
		{"empty_source", "", "new content"},
		{"empty_target", "old content", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte(tt.source)
			tgt := []byte(tt.target)

			d := Create(src, tgt)
			if len(d) == 0 {
				t.Fatal("Create returned empty delta")
			}

			got, err := Apply(src, d)
			if err != nil {
				t.Fatalf("Apply failed: %v", err)
			}
			if !bytes.Equal(got, tgt) {
				t.Fatalf("round-trip failed: got %q, want %q", got, tgt)
			}
		})
	}
}

func TestCreate_LargeInput(t *testing.T) {
	source := bytes.Repeat([]byte("The quick brown fox jumps. "), 4000)
	target := make([]byte, len(source))
	copy(target, source)
	copy(target[50000:], []byte("CHANGED CONTENT HERE!"))

	d := Create(source, target)

	if len(d) > len(target)/2 {
		t.Fatalf("delta too large: %d bytes for %d byte target", len(d), len(target))
	}

	got, err := Apply(source, d)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatal("round-trip failed for large input")
	}
}

func TestCreate_RoundTrip_FossilValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fossil validation in short mode")
	}
	source := []byte("original content of the file\nwith multiple lines\nand some data\n")
	target := []byte("original content of the file\nwith MODIFIED lines\nand some data\nplus new stuff\n")

	d := Create(source, target)
	got, err := Apply(source, d)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("round-trip failed")
	}
}

func BenchmarkApply(b *testing.B) {
	source := bytes.Repeat([]byte("abcdefghij"), 1000)
	target := append(bytes.Repeat([]byte("abcdefghij"), 999), []byte("CHANGED!")...)
	cs := Checksum(target)
	delta := encodeDelta(uint64(len(target)), nil, target, cs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Apply(source, delta)
	}
}

func BenchmarkCreate(b *testing.B) {
	source := bytes.Repeat([]byte("abcdefghij"), 1000)
	target := make([]byte, len(source))
	copy(target, source)
	copy(target[5000:], []byte("XXXXXXXXXXXX"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Create(source, target)
	}
}

// --- Test helpers ---

type deltaOp struct {
	opType byte
	offset uint64
	length uint64
	data   []byte
}

func manualDelta(targetLen uint64, ops []deltaOp, checksum uint32) []byte {
	var buf bytes.Buffer
	writeInt(&buf, targetLen)
	buf.WriteByte('\n')
	for _, op := range ops {
		switch op.opType {
		case '@':
			// Fossil format: count@offset,
			writeInt(&buf, op.length)
			buf.WriteByte('@')
			writeInt(&buf, op.offset)
			buf.WriteByte(',')
		case ':':
			writeInt(&buf, uint64(len(op.data)))
			buf.WriteByte(':')
			buf.Write(op.data)
		}
	}
	writeInt(&buf, uint64(checksum))
	buf.WriteByte(';')
	return buf.Bytes()
}

func encodeDelta(targetLen uint64, source, literal []byte, checksum uint32) []byte {
	var buf bytes.Buffer
	writeInt(&buf, targetLen)
	buf.WriteByte('\n')
	if len(literal) > 0 {
		writeInt(&buf, uint64(len(literal)))
		buf.WriteByte(':')
		buf.Write(literal)
	}
	writeInt(&buf, uint64(checksum))
	buf.WriteByte(';')
	return buf.Bytes()
}

const zDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz~"

func writeInt(buf *bytes.Buffer, v uint64) {
	if v == 0 {
		buf.WriteByte('0')
		return
	}
	var tmp [13]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = zDigits[v&0x3f]
		v >>= 6
	}
	buf.Write(tmp[i:])
}
