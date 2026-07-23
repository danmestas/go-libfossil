package content

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// BenchmarkExpandOldestVersion re-measures issue #85's delta-chain table.
//
// Goal: quantify the cost of expanding the OLDEST version of a file whose
// history is N revisions deep, after #81/#82 landed.
//
// Methodology: build one ~26 KB, 400-line file with N revisions, stored the
// way content_deltify stores them -- newest version full, every older version
// a delta against the version one newer -- so the oldest version sits at the
// bottom of an (N-1)-deep chain. Expand that oldest rid; Go's benchmark
// harness reports ns/op (== per-Expand wall time) and B/op + allocs/op
// (== alloc/Expand). Revision counts 12/50/200 mirror the issue's table.
func BenchmarkExpandOldestVersion(b *testing.B) {
	for _, revisions := range []int{12, 50, 200} {
		b.Run(fmt.Sprintf("revisions=%d", revisions), func(b *testing.B) {
			benchmarkExpandOldest(b, revisions, smallFileLines)
		})
	}
}

// BenchmarkExpandOldestVersionLarge is the >64 KiB counterpart of
// BenchmarkExpandOldestVersion. It exists to test one specific claim: that
// delta.Apply's fixed-growth path is gated behind deltaInitialCap (64 KiB),
// so only targets larger than that see #81/#82's allocation fix.
//
// Same chain shape, same revision counts; the only variable is target size.
// Comparing B/op per link between the two benchmarks isolates the threshold:
// if the gate is real, per-link allocation relative to target size drops here
// and does not in the small case.
func BenchmarkExpandOldestVersionLarge(b *testing.B) {
	for _, revisions := range []int{12, 50, 200} {
		b.Run(fmt.Sprintf("revisions=%d", revisions), func(b *testing.B) {
			benchmarkExpandOldest(b, revisions, largeFileLines)
		})
	}
}

// benchmarkExpandOldest builds a `revisions`-deep backward chain over a file
// of `lines` lines and times Expand of the oldest (deepest) version.
func benchmarkExpandOldest(b *testing.B, revisions, lines int) {
	b.Helper()
	d := setupBenchDB(b)
	oldestRID, depth := buildBackwardChain(b, d, revisions, lines)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := Expand(d, oldestRID)
		if err != nil {
			b.Fatalf("Expand oldest rid=%d: %v", oldestRID, err)
		}
		if len(got) == 0 {
			b.Fatalf("Expand returned empty content for rid=%d", oldestRID)
		}
	}
	// Reported after the loop: ResetTimer deletes user metrics, so a depth
	// reported before it never reaches the output.
	b.StopTimer()
	b.ReportMetric(float64(depth), "chain-depth")
}

// setupBenchDB opens a fresh repository schema in a temp file. It mirrors
// setupTestDB but takes testing.TB so benchmarks can share it.
func setupBenchDB(tb testing.TB) *db.DB {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "chain.fossil")
	d, err := db.Open(path)
	if err != nil {
		tb.Fatalf("db.Open: %v", err)
	}
	if err := db.CreateRepoSchema(d); err != nil {
		tb.Fatalf("CreateRepoSchema: %v", err)
	}
	tb.Cleanup(func() { d.Close() })
	return d
}

// buildBackwardChain stores `revisions` versions of a ~26 KB file and then
// deltifies them the way Fossil's content_deltify does: every version but the
// newest is rewritten as a delta against the version one newer, leaving the
// newest whole. It returns the rid of the oldest version and the depth of its
// delta chain (revisions-1), so expanding that rid replays the entire chain
// from the whole tip back down.
//
// Going through content.Deltify (not blob.StoreDelta) is deliberate: Deltify
// expands the source before diffing, so a chain of deltas-against-deltas comes
// out canonical-correct, which is exactly the on-disk shape a real repository
// carries and the one issue #85 is about reading.
func buildBackwardChain(tb testing.TB, d *db.DB, revisions, lines int) (oldest libfossil.FslID, depth int) {
	tb.Helper()
	if revisions < 2 {
		panic("buildBackwardChain: revisions must be >= 2")
	}

	// All versions stored whole first, oldest (rev 0) at rids[0].
	rids := make([]libfossil.FslID, revisions)
	for k := 0; k < revisions; k++ {
		rid, _, err := blob.Store(d, revisionContent(k, lines))
		if err != nil {
			tb.Fatalf("Store revision %d: %v", k, err)
		}
		rids[k] = rid
	}

	// Rewrite each older version as a delta against the one newer. Each
	// target is deltified exactly once and is still whole when it is, which
	// is what content.Deltify's never-redeltify rule requires.
	err := d.WithTx(func(tx *db.Tx) error {
		for k := 0; k < revisions-1; k++ {
			saved, err := Deltify(tx, rids[k], rids[k+1])
			if err != nil {
				return fmt.Errorf("deltify revision %d against %d: %w", k, k+1, err)
			}
			if saved <= 0 {
				return fmt.Errorf("deltify revision %d declined (saved=%d); "+
					"content must differ enough to delta", k, saved)
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("build backward chain: %v", err)
	}

	depth = revisions - 1
	if rids[0] <= 0 {
		panic("buildBackwardChain: oldest rid must be > 0")
	}
	return rids[0], depth
}

// File sizes for the two benchmark arms. Each generated line is 64 bytes, so
// smallFileLines lands at ~26 KB (below delta.Apply's 64 KiB deltaInitialCap)
// and largeFileLines at ~211 KB (above it). The threshold is what the two arms
// are there to straddle.
const (
	smallFileLines = 400
	largeFileLines = 3200
)

// revisionContent returns the content of revision `rev`: a deterministic
// `lines`-line file whose lines drift with rev so consecutive revisions
// differ by a handful of lines, the shape content_deltify sees in practice.
func revisionContent(rev, lines int) []byte {
	buf := make([]byte, 0, lines*66)
	for i := 0; i < lines; i++ {
		// Most lines are stable across revisions; a strided subset changes
		// with rev so each revision is a small delta off its neighbour.
		token := i
		if i%13 == 0 {
			token = i + rev
		}
		line := fmt.Sprintf(
			"line %04d token %08d lorem ipsum dolor sit amet consectetur\n",
			i, token)
		buf = append(buf, line...)
	}
	// Guard the size assumption the two arms rest on: 64 bytes per line, so
	// the small arm must stay under and the large arm over the 64 KiB cap.
	if lines == smallFileLines && len(buf) >= 64*1024 {
		panic("revisionContent: small arm must stay below the 64 KiB cap")
	}
	if lines == largeFileLines && len(buf) <= 64*1024 {
		panic("revisionContent: large arm must exceed the 64 KiB cap")
	}
	return buf
}
