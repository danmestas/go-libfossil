package verify_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/simio"
)

// BenchmarkRebuildDeepHistory measures verify.Rebuild over a repository whose
// files carry long, deltified histories -- the whole-repository-sweep case
// issue #151 routes through content.Cache.
//
// Methodology: commit `revisions` versions of two ~26 KB files, each changing a
// strided subset of lines off the previous, so Checkin deltifies every older
// file blob into a backward chain against its successor. Rebuild then sweeps
// every blob more than once -- checkBlobs, rebuildManifests, and two passes in
// rebuildTags -- each of which would, uncached, replay every blob's whole delta
// chain from its whole tip: O(revisions^2) per sweep, times the sweep count.
// The shared cache walks each chain once for the entire rebuild.
func BenchmarkRebuildDeepHistory(b *testing.B) {
	for _, revisions := range []int{20, 60, 150} {
		b.Run(fmt.Sprintf("revisions=%d", revisions), func(b *testing.B) {
			r := buildRebuildHistory(b, revisions)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := verify.Rebuild(r); err != nil {
					b.Fatalf("Rebuild: %v", err)
				}
			}
		})
	}
}

func buildRebuildHistory(b *testing.B, revisions int) *repo.Repo {
	b.Helper()
	if revisions < 2 {
		panic("buildRebuildHistory: revisions must be >= 2")
	}

	path := filepath.Join(b.TempDir(), "hist.fossil")
	r, err := repo.Create(path, "bench", simio.CryptoRand{}, "")
	if err != nil {
		b.Fatalf("repo.Create: %v", err)
	}
	b.Cleanup(func() { r.Close() })

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var parent libfossil.FslID
	for k := 0; k < revisions; k++ {
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: []manifest.File{
				{Name: "a.txt", Content: rebuildRevision(k, 0)},
				{Name: "b.txt", Content: rebuildRevision(k, 7)},
			},
			Comment: fmt.Sprintf("rev %d", k),
			User:    "bench",
			Parent:  parent,
			Time:    base.Add(time.Duration(k) * time.Hour),
		})
		if err != nil {
			b.Fatalf("checkin rev %d: %v", k, err)
		}
		parent = rid
	}
	return r
}

// rebuildRevision returns revision k's content for a file seeded by `salt`, so
// the two files in a commit differ from each other while each still drifts by a
// handful of lines per revision.
func rebuildRevision(k, salt int) []byte {
	const lines = 400
	buf := make([]byte, 0, lines*66)
	for i := 0; i < lines; i++ {
		token := i + salt
		if i%11 == 0 {
			token = i + salt + k
		}
		buf = append(buf, fmt.Sprintf(
			"line %04d token %08d lorem ipsum dolor sit amet consectetur\n",
			i, token)...)
	}
	return buf
}
