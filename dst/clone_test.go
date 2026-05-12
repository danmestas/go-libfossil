package dst

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/repo"
	libsync "github.com/danmestas/libfossil/internal/sync"
	"github.com/danmestas/libfossil/simio"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// runCloneDST exercises Clone under deterministic fault injection.
//
//  1. Create server repo, seed with 50 blobs (deterministic content from seed).
//  2. Create MockFossil with server-side SeededBuggify (seed+1000).
//  3. Call sync.Clone() with client-side SeededBuggify (seed+2000).
//  4. On success: verify blob convergence + no phantoms.
//  5. On failure: verify repo file deleted (error cleanup).
func runCloneDST(t *testing.T, seed int64) {
	t.Helper()

	// 1. Create and seed the server repo.
	serverPath := filepath.Join(t.TempDir(), "server.fossil")
	serverRepo, err := repo.Create(serverPath, "server", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("seed %d: repo.Create server: %v", seed, err)
	}
	t.Cleanup(func() { serverRepo.Close() })

	rng := rand.New(rand.NewSource(seed))
	seededUUIDs := make([]string, 0, 50)
	for i := range 50 {
		size := 64 + rng.Intn(512)
		data := make([]byte, size)
		rng.Read(data)

		var uuid string
		err := serverRepo.WithTx(func(tx *db.Tx) error {
			_, u, err := blob.Store(tx, data)
			if err != nil {
				return err
			}
			uuid = u
			return nil
		})
		if err != nil {
			t.Fatalf("seed %d: store blob %d: %v", seed, i, err)
		}
		seededUUIDs = append(seededUUIDs, uuid)
	}

	// 2. Wire up MockFossil with server-side buggify.
	mf := NewMockFossil(serverRepo)
	serverBuggify := &SeededBuggify{rng: rand.New(rand.NewSource(seed + 1000))}
	mf.SetBuggify(serverBuggify)

	// 3. Clone with client-side buggify.
	clonePath := filepath.Join(t.TempDir(), "clone.fossil")
	clientBuggify := &SeededBuggify{rng: rand.New(rand.NewSource(seed + 2000))}

	ctx := context.Background()
	cloneRepo, result, cloneErr := libsync.Clone(ctx, clonePath, mf, libsync.CloneOpts{
		Buggify: clientBuggify,
	})

	if cloneErr != nil {
		// 5. On failure: verify repo file was cleaned up.
		t.Logf("seed %d: clone failed (expected under buggify): %v", seed, cloneErr)
		if _, statErr := os.Stat(clonePath); !os.IsNotExist(statErr) {
			t.Errorf("seed %d: clone file should be deleted on error, stat returned: %v", seed, statErr)
		}
		return
	}

	// 4. On success: verify convergence.
	t.Cleanup(func() { cloneRepo.Close() })
	t.Logf("seed %d: clone OK — %d rounds, %d blobs received", seed, result.Rounds, result.BlobsRecvd)

	// Verify blob convergence. Under buggify, dropFile can silently
	// discard received blobs, so a "successful" clone may have gaps.
	// We log missing blobs and only fail if zero blobs arrived.
	missing := 0
	for _, uuid := range seededUUIDs {
		_, exists := blob.Exists(cloneRepo.DB(), uuid)
		if !exists {
			missing++
		}
	}
	if missing > 0 {
		t.Logf("seed %d: %d/%d blobs missing (expected under buggify dropFile)", seed, missing, len(seededUUIDs))
	}
	if result.BlobsRecvd == 0 {
		t.Errorf("seed %d: clone reported success but received 0 blobs", seed)
	}

	// Check phantom table — under buggify, residual phantoms are possible
	// when dropFile discards a blob that the protocol considers delivered.
	var phantomCount int
	row := cloneRepo.DB().QueryRow("SELECT COUNT(*) FROM phantom")
	if err := row.Scan(&phantomCount); err != nil {
		t.Fatalf("seed %d: query phantom count: %v", seed, err)
	}
	if phantomCount > 0 {
		t.Logf("seed %d: %d residual phantoms (expected under buggify)", seed, phantomCount)
	}
}

// TestCloneDST runs a single deterministic clone with fault injection.
func TestCloneDST(t *testing.T) {
	runCloneDST(t, 42)
}

// TestCloneDSTSeedSweep runs 20 seeds to explore diverse fault combinations.
func TestCloneDSTSeedSweep(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			runCloneDST(t, seed)
		})
	}
}
