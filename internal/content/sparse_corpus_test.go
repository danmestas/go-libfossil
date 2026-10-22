package content_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestCorpusSparseClassification classifies a real repository's phantoms
// under the store cone {src}, then under a content horizon at a recent trunk
// check-in, and reports wall time, reads and peak heap for each. It is
// skipped unless LIBFOSSIL_CORPUS names a repository; it works on a copy.
func TestCorpusSparseClassification(t *testing.T) {
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
	// Elide a few old file versions: some named only outside src/, and some
	// src/ versions no recent check-in names, for the horizon.
	for _, query := range []string{
		`SELECT b.uuid FROM mlink m JOIN filename f ON f.fnid=m.fnid JOIN blob b ON b.rid=m.fid
		 WHERE b.size>=0 AND f.name NOT LIKE 'src/%' ORDER BY m.mid LIMIT 5`,
		`SELECT b.uuid FROM mlink m JOIN filename f ON f.fnid=m.fnid JOIN blob b ON b.rid=m.fid
		 WHERE b.size>=0 AND f.name LIKE 'src/%' ORDER BY m.mid LIMIT 5`,
	} {
		var uuids []string
		rows, err := r.DB().Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var uuid string
			if err := rows.Scan(&uuid); err != nil {
				t.Fatal(err)
			}
			uuids = append(uuids, uuid)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, uuid := range uuids {
			hzPhantomize(t, r, uuid)
		}
	}
	path, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := corpusClassify(t, r, "cone {src}", path); a.Count(content.Elided) == 0 {
		t.Error("cone {src} elided nothing")
	}
	var c string
	if err := r.DB().QueryRow(`SELECT b.uuid FROM event e JOIN blob b ON b.rid=e.objid
		JOIN tagxref x ON x.rid=e.objid AND x.tagtype>0 AND x.value='trunk'
		JOIN tag g ON g.tagid=x.tagid AND g.tagname='branch'
		WHERE e.type='ci' ORDER BY e.mtime DESC LIMIT 1 OFFSET 20`).Scan(&c); err != nil {
		t.Fatalf("recent trunk check-in: %v", err)
	}
	h, err := cone.NewHorizon(c, cone.RuleDescendants, cone.DepthContent)
	if err != nil {
		t.Fatal(err)
	}
	both, err := cone.NewWithHorizon([]string{"src"}, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if a := corpusClassify(t, r, "cone {src} + horizon "+c[:10], both); a.Count(content.Elided) <= 5 {
		t.Error("the horizon elided no old src/ version")
	}
}

func corpusClassify(t *testing.T, r *repo.Repo, label string, c *cone.Cone) *content.Absences {
	t.Helper()
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	stop := corpusHeapPeak()
	start := time.Now()
	a, work, err := content.LoadAbsencesWork(context.Background(), r.DB())
	elapsed := time.Since(start)
	peak := stop()
	if err != nil {
		t.Fatalf("%s: %v (work %+v)", label, err, work)
	}
	t.Logf("%s: %v, peak heap %d MiB, elided %d transit %d shunned %d", label, elapsed,
		peak>>20, a.Count(content.Elided), a.Count(content.Transit), a.Count(content.Shunned))
	t.Logf("%s: work %+v", label, work)
	return a
}

// corpusHeapPeak samples the live heap until stopped, which returns the
// peak, a last reading included.
func corpusHeapPeak() func() uint64 {
	var mu sync.Mutex
	var max uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		mu.Lock()
		if m.HeapAlloc > max {
			max = m.HeapAlloc
		}
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
	stop := func() uint64 {
		sample()
		close(done)
		wg.Wait()
		return max
	}
	return stop
}

func corpusCopy(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
