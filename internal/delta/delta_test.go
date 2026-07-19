package delta

import (
	"bytes"
	"runtime"
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

// TestApply_TinyPayloadHugeHeaderBoundsAllocation is a regression test for
// a durable, remotely-planted memory-exhaustion landmine. A malicious
// delta can declare an enormous target length in its header while
// supplying almost no actual delta bytes. Before this fix, output was
// preallocated directly from that claim (make([]byte, 0, targetLen)), so
// a handful of wire bytes could trigger an allocation scaled to whatever
// the header claimed, up to maxDeltaTargetSize (4 GiB). Because a delta
// can now be stored unverified (see blob.StoreDeltaRaw), a hostile peer
// plants this once and every later Expand of that row repeats the
// attempt -- not a one-off parse spike.
//
// This claims a 100 MiB target from a payload with no command bytes
// after the header (so Apply must still fail with "missing terminator" --
// correctness is unchanged), and asserts the actual allocation growth
// stays orders of magnitude below the claim: proof the allocation is
// bounded by a fixed cap, not by the wire-supplied number.
func TestApply_TinyPayloadHugeHeaderBoundsAllocation(t *testing.T) {
	const hugeTarget = 100 << 20 // 100 MiB, comfortably under maxDeltaTargetSize
	// The header's integer is encoded in Fossil's own base-64 alphabet
	// (see appendInt), not decimal -- fmt.Sprintf("%d", ...) would
	// produce a string that happens to still parse (digits '0'-'9' are
	// valid base-64 digits in this scheme) but as a wildly different,
	// much larger value, which would be rejected by parseTargetLen's
	// maxDeltaTargetSize bound before ever reaching the allocation this
	// test targets. appendInt is the same encoder Create uses.
	header := appendInt(nil, hugeTarget)
	header = append(header, '\n')

	// Sanity: confirm the header round-trips to the value this test
	// actually intends to claim, so a future encoding change can't
	// silently turn this back into a no-op the way the decimal version did.
	got, err := OutputSize(header)
	if err != nil || got != hugeTarget {
		t.Fatalf("fixture bug: header round-trips to (%d, %v), want (%d, nil)", got, err, uint64(hugeTarget))
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	_, err = Apply([]byte{}, header)

	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("expected an error (missing terminator): header has no command bytes")
	}

	grown := after.TotalAlloc - before.TotalAlloc
	const maxSaneGrowth = 4 << 20 // 4 MiB: generous headroom over the 64 KiB cap
	if grown > maxSaneGrowth {
		t.Fatalf("Apply allocated %d bytes for a %d-byte header claiming a %d-byte target -- "+
			"want the allocation bounded by actual work performed, not by the wire-supplied claim",
			grown, len(header), hugeTarget)
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

// TestOutputSize_OverflowRejected is a regression test for wire data that
// overflows the header's integer parse. Eleven base-64 digits at their
// maximum value ('~' == 63) accumulate to exactly math.MaxUint64, which
// casts to int64(-1) -- the same sentinel blob.size uses to mean
// "phantom". A parser that silently wraps instead of rejecting would hand
// StoreDeltaRaw a size that, once cast, makes a freshly-written row with
// real content indistinguishable from a phantom: not re-requestable (no
// phantom-table row), not readable (every "size != -1" check in the
// codebase misclassifies it), permanent corruption from twelve bytes.
//
// This intentionally does not just assert err != nil: a "fix" that
// clamped or saturated the value instead of rejecting it would return a
// non-error result and still be able to produce a negative cast, so the
// value itself is checked whenever no error is returned.
func TestOutputSize_OverflowRejected(t *testing.T) {
	header := []byte("~~~~~~~~~~~\n")

	size, err := OutputSize(header)
	if err == nil {
		if int64(size) < 0 {
			t.Fatalf("OutputSize(overflow header) succeeded with size=%d (int64 %d) -- "+
				"a negative cast is indistinguishable from the phantom sentinel used by blob.size",
				size, int64(size))
		}
		t.Fatalf("OutputSize(overflow header) = %d, want rejection (wire data must not be trusted "+
			"to self-report a size this large)", size)
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
