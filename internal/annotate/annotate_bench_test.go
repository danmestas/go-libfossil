package annotate

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

// BenchmarkAnnotateDeepHistory measures Annotate over a file with a long,
// deltified revision history -- the exact shape issue #151 targets.
//
// Methodology: commit `revisions` versions of a ~26 KB, 400-line file, each
// changing a strided subset of lines off the previous. manifest.Checkin
// deltifies as it goes, storing every older file revision as a delta against
// the one newer and leaving the newest whole (verified: the file blob at each
// revision carries a delta.srcid to its successor). Annotate(tip) then walks
// the whole parent chain, loading the file at every revision. Without a shared
// cache each load replays that revision's delta chain from the whole tip:
// O(revisions^2) delta applications. The per-op numbers (ns/op, B/op,
// allocs/op) are what routing the walk through content.Cache is meant to move.
func BenchmarkAnnotateDeepHistory(b *testing.B) {
	for _, revisions := range []int{20, 60, 150} {
		b.Run(fmt.Sprintf("revisions=%d", revisions), func(b *testing.B) {
			r, tip := buildFileHistory(b, revisions)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lines, err := Annotate(r, Options{FilePath: "file.txt", StartRID: tip})
				if err != nil {
					b.Fatalf("Annotate: %v", err)
				}
				if len(lines) == 0 {
					b.Fatal("Annotate returned no lines")
				}
			}
		})
	}
}

// buildFileHistory commits `revisions` versions of one file, each a small edit
// off the last, and returns the repo and the tip checkin rid. Checkin deltifies
// the file blobs into a backward chain (newest whole, older revisions deltas
// against it) on its own, matching a real repository's on-disk shape.
func buildFileHistory(b *testing.B, revisions int) (*repo.Repo, libfossil.FslID) {
	b.Helper()
	if revisions < 2 {
		panic("buildFileHistory: revisions must be >= 2")
	}

	path := filepath.Join(b.TempDir(), "hist.fossil")
	r, err := repo.Create(path, "bench", simio.CryptoRand{}, "")
	if err != nil {
		b.Fatalf("repo.Create: %v", err)
	}
	b.Cleanup(func() { r.Close() })

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var parent, tip libfossil.FslID
	for k := 0; k < revisions; k++ {
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "file.txt", Content: benchRevision(k)}},
			Comment: fmt.Sprintf("rev %d", k),
			User:    "bench",
			Parent:  parent,
			Time:    base.Add(time.Duration(k) * time.Hour),
		})
		if err != nil {
			b.Fatalf("checkin rev %d: %v", k, err)
		}
		parent = rid
		tip = rid
	}
	return r, tip
}

// benchRevision returns revision k's content: a deterministic ~400-line file
// whose lines drift with k so consecutive revisions differ by a handful of
// lines -- the shape content_deltify sees, and enough change per step that the
// deltifier keeps each revision as a delta rather than a whole copy.
func benchRevision(k int) []byte {
	const lines = 400
	buf := make([]byte, 0, lines*66)
	for i := 0; i < lines; i++ {
		token := i
		if i%11 == 0 {
			token = i + k
		}
		buf = append(buf, fmt.Sprintf(
			"line %04d token %08d lorem ipsum dolor sit amet consectetur\n",
			i, token)...)
	}
	return buf
}
