package content

import (
	"bytes"
	"runtime"
	"testing"
)

// TestExpand_DeepChainReusesBuffers is the regression guard for issue #145's
// sub-64 KiB fix. Goal: prove a plain Expand of a deep backward chain no longer
// allocates one full-size output buffer per link.
//
// Methodology: build a ~26 KB file (below delta.Apply's 64 KiB cap) with a
// 60-revision backward delta chain, expand the oldest rid once to warm and to
// capture the correct bytes, then measure TotalAlloc across a second Expand.
// Before the fix each of the ~59 links allocated its own full-size target
// buffer (plus a fresh zlib reader), landing near 2.7x depth*targetLen. The
// ping-pong replay holds a constant two output buffers and reuses one zlib
// reader, so total allocation must stay under a single full buffer per link.
func TestExpand_DeepChainReusesBuffers(t *testing.T) {
	d := setupBenchDB(t)
	const revisions = 60
	oldest, depth := buildBackwardChain(t, d, revisions, smallFileLines)

	want, err := Expand(d, oldest)
	if err != nil {
		t.Fatalf("Expand (warm): %v", err)
	}
	targetLen := len(want)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	got, err := Expand(d, oldest)

	runtime.ReadMemStats(&after)

	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Expand of a depth-%d chain returned wrong content", depth)
	}

	grown := after.TotalAlloc - before.TotalAlloc
	// One full output buffer per link is the pre-fix floor for Apply outputs
	// alone; counting the per-link zlib window the old path landed well above
	// it. Staying under this bound proves the outputs and reader are reused.
	maxBytes := uint64(depth) * uint64(targetLen)
	if grown > maxBytes {
		t.Fatalf("Expand of a depth-%d chain allocated %d bytes (~%.1fx a %d-byte target); "+
			"want under %d (one full buffer per link) -- buffers are not being reused",
			depth, grown, float64(grown)/float64(targetLen), targetLen, maxBytes)
	}
}
