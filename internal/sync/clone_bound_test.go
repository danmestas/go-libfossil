package sync_test

import (
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestCloneSelfRoundTripLargeArtifactAfterFiller is the #109 regression test:
// a large artifact must clone even when ordinary filler precedes it in the
// same round, because the true ceiling on a round is the client's decode bound
// minus whatever the round already accumulated -- not the bound alone.
//
// Method: for each filler size, a fresh source holds one incompressible filler
// blob (stored first, so it takes the lower rid and the clone sends it ahead of
// the artifact) followed by the 58.7 MB artifact measured in the issue. The
// repository is served and cloned by this implementation over real HTTP, so the
// client decodes every round through xfer.MaxDecompressedBytes; a round that
// carried filler plus the whole artifact would exceed that bound and fail the
// clone. cloneSelfAndCompare requires artifact parity.
//
// The three filler sizes are exactly the issue's: each is under the round
// budget, so before the fix the filler did not exhaust the budget and the
// artifact rode into the same round. The non-monotonic 17 MB case that passed
// (by exhausting the budget and pushing the artifact into its own round) is not
// retested here -- the point is that clonability no longer depends on incidental
// filler size at all.
func TestCloneSelfRoundTripLargeArtifactAfterFiller(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a 58.7 MB artifact behind filler at three sizes")
	}
	// The exact artifact size from issue #109.
	const artifactBytes = 58720256
	if artifactBytes >= xfer.MaxDecompressedBytes {
		t.Fatalf("artifact of %d bytes cannot fit under the decode bound of %d when sent alone",
			artifactBytes, xfer.MaxDecompressedBytes)
	}

	for _, fillerBytes := range []int{8 << 20, 12 << 20, 15 << 20} {
		t.Run(fmt.Sprintf("filler-%dMiB", fillerBytes>>20), func(t *testing.T) {
			// The filler must stay under the batch budget so it does not force a
			// round of its own; only then does the artifact ride into the same
			// round, which is the #109 condition. (filler + artifact + per-card
			// overhead exceeds the decode bound at each of these sizes -- proven
			// by the clone itself, which is the real assertion here.)
			if fillerBytes >= sync.DefaultCloneBatchBytes {
				t.Fatalf("filler %d must stay under the batch budget %d to reproduce #109",
					fillerBytes, sync.DefaultCloneBatchBytes)
			}

			dir := t.TempDir()
			srcRepo := newSelfRoundTripRepo(t, dir)
			defer srcRepo.Close()

			// Store filler first so it takes the lower rid; the clone iterates by
			// rid, so this places the artifact behind accumulated round bytes.
			if _, _, err := blob.Store(srcRepo.DB(), incompressibleBytes(2, fillerBytes)); err != nil {
				t.Fatalf("store filler: %v", err)
			}
			if _, _, err := blob.Store(srcRepo.DB(), incompressibleBytes(3, artifactBytes)); err != nil {
				t.Fatalf("store artifact: %v", err)
			}

			cloneSelfAndCompare(t, srcRepo, dir)
		})
	}
}
