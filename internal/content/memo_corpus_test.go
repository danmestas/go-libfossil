package content_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestCorpusMemoTrees reports what the classification memo costs on a real
// repository (draft-fossil-horizon-00 §6.2): content horizons 1000 and 2000
// check-ins back, each cold (a fresh memo) and warm (after the other), with
// the heap the memo keeps, the bytes it charges for trees, and the peak
// in-use heap, each inside a read transaction as an origin's request reads.
// It is skipped unless LIBFOSSIL_CORPUS names a repository; it
// works on a copy.
func TestCorpusMemoTrees(t *testing.T) {
	corpus := os.Getenv("LIBFOSSIL_CORPUS")
	if corpus == "" {
		t.Skip("set LIBFOSSIL_CORPUS to a .fossil repository to run")
	}
	work := filepath.Join(t.TempDir(), "corpus.fossil")
	if err := corpusCopy(work, corpus); err != nil {
		t.Fatalf("copy corpus: %v", err)
	}
	r, err := repo.Open(work)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	var tip string
	if err := r.DB().QueryRow(`SELECT b.uuid FROM event e JOIN blob b ON b.rid=e.objid
		WHERE e.type='ci' ORDER BY e.mtime DESC LIMIT 1`).Scan(&tip); err != nil {
		t.Fatal(err)
	}
	horizons := map[int]cone.Horizon{}
	for _, back := range []int{1000, 2000} {
		h, err := cone.NewHorizon(corpusAncestor(t, r, tip, back), cone.RuleDescendants, cone.DepthContent)
		if err != nil {
			t.Fatal(err)
		}
		horizons[back] = h
	}
	for _, order := range [][2]int{{1000, 2000}, {2000, 1000}} {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		stop := corpusInUsePeak()
		memo := content.NewMemo()
		var took [2]time.Duration
		for i, back := range order {
			start := time.Now()
			err := r.DB().WithReadTx(func(tx *db.Tx) error {
				v, err := memo.View(context.Background(), tx)
				if err != nil {
					return err
				}
				_, _, placed, err := v.HorizonReach(context.Background(), tx, horizons[back])
				if err == nil && !placed {
					t.Fatalf("horizon %d back: not placed", back)
				}
				v.Publish()
				return err
			})
			took[i] = time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
		}
		peak := stop()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		t.Logf("horizon %d back cold %s, then %d back warm %s; memo heap %d MiB, tree bytes %d MiB, peak in-use heap %d MiB",
			order[0], took[0], order[1], took[1], (int64(after.HeapInuse)-int64(before.HeapInuse))>>20,
			memo.TreeBytes()>>20, peak>>20)
		runtime.KeepAlive(memo)
	}
}

// corpusInUsePeak samples HeapInuse until stopped, which returns the peak.
func corpusInUsePeak() func() uint64 {
	var mu sync.Mutex
	var peak uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		mu.Lock()
		peak = max(peak, m.HeapInuse)
		mu.Unlock()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				sample()
			}
		}
	}()
	return func() uint64 {
		sample()
		close(done)
		wg.Wait()
		return peak
	}
}

// corpusAncestor walks n primary parents back from uuid, stopping at a root.
func corpusAncestor(t *testing.T, r *repo.Repo, uuid string, n int) string {
	t.Helper()
	for i := 0; i < n; i++ {
		var parent string
		err := r.DB().QueryRow(`SELECT p.uuid FROM plink JOIN blob c ON c.rid=plink.cid
			JOIN blob p ON p.rid=plink.pid WHERE c.uuid=? AND plink.isprim`, uuid).Scan(&parent)
		if err != nil {
			return uuid
		}
		uuid = parent
	}
	return uuid
}
