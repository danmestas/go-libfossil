package content

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestCorpusExpandSweep isolates the expansion half of a crosslink sweep: it
// expands every blob of a real repository in rid order, the order Crosslink
// visits them in, and reports throughput and cache behaviour. Skipped unless
// LIBFOSSIL_CORPUS names a repository.
func TestCorpusExpandSweep(t *testing.T) {
	corpus := os.Getenv("LIBFOSSIL_CORPUS")
	if corpus == "" {
		t.Skip("set LIBFOSSIL_CORPUS to a .fossil repository to run")
	}
	limit := 5000
	if s := os.Getenv("LIBFOSSIL_CORPUS_BLOBS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("LIBFOSSIL_CORPUS_BLOBS: %v", err)
		}
		limit = n
	}
	cacheBytes := int64(256 << 20)
	if s := os.Getenv("LIBFOSSIL_CORPUS_CACHE_MB"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("LIBFOSSIL_CORPUS_CACHE_MB: %v", err)
		}
		cacheBytes = int64(n) << 20
	}

	// Work on a copy: opening a repository read-write creates WAL sidecars
	// next to it, and the corpus is somebody's real repository.
	src, err := os.ReadFile(corpus)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	work := filepath.Join(t.TempDir(), "corpus.fossil")
	if err := os.WriteFile(work, src, 0o600); err != nil {
		t.Fatalf("write corpus copy: %v", err)
	}

	d, err := db.Open(work)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer d.Close()

	// Delta chains in a real repository run from a high-rid root down to
	// low-rid ancestors, so descending order approximates chain order and
	// ascending order — the order Crosslink sweeps in — is its reverse.
	order := "ASC"
	if os.Getenv("LIBFOSSIL_CORPUS_ORDER") == "desc" {
		order = "DESC"
	}
	rows, err := d.Query("SELECT rid FROM blob WHERE size>=0 ORDER BY rid "+order+" LIMIT ?", limit)
	if err != nil {
		t.Fatalf("query rids: %v", err)
	}
	var rids []libfossil.FslID
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			t.Fatalf("scan rid: %v", err)
		}
		rids = append(rids, rid)
	}
	rows.Close()

	var c *Cache // nil cache = no memoization, the pre-change behaviour
	if cacheBytes > 0 {
		c = NewCache(cacheBytes)
	}
	start := time.Now()
	failed := 0
	for _, rid := range rids {
		if _, err := c.Expand(d, rid); err != nil {
			failed++
		}
	}
	elapsed := time.Since(start)
	s := c.Stats()
	t.Logf("expanded %d blobs in %s (%.1f blobs/sec), %d failed; cache %d MiB: hits=%d misses=%d entries=%d size=%dMiB",
		len(rids), elapsed.Round(time.Millisecond), float64(len(rids))/elapsed.Seconds(), failed,
		cacheBytes>>20, s.Hits, s.Misses, s.Entries, s.Size>>20)
}
