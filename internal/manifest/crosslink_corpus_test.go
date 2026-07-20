package manifest

import (
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/repo"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// derivedTables are the tables Crosslink is responsible for populating.
// Emptying them turns a fully-built repository back into the state a
// freshly-transferred clone is in right before crosslinking starts.
var derivedTables = []string{
	"event", "plink", "leaf", "mlink", "tagxref", "forumpost",
	"attachment", "backlink", "cherrypick", "ticket", "ticketchng",
}

// TestCorpusCrosslinkThroughput measures Crosslink throughput against a real
// Fossil repository. It is skipped unless LIBFOSSIL_CORPUS names one.
//
//	LIBFOSSIL_CORPUS=/path/to/fossil.fossil \
//	LIBFOSSIL_CORPUS_EVENTS=2000 \
//	LIBFOSSIL_CORPUS_CPUPROFILE=/tmp/cross.prof \
//	go test ./internal/manifest/ -run TestCorpusCrosslinkThroughput -v -timeout 0
//
// The reported figure is wall time to write the first N event rows, so runs
// before and after a change compare the same amount of work.
func TestCorpusCrosslinkThroughput(t *testing.T) {
	corpus := os.Getenv("LIBFOSSIL_CORPUS")
	if corpus == "" {
		t.Skip("set LIBFOSSIL_CORPUS to a .fossil repository to run")
	}
	target := 2000
	if s := os.Getenv("LIBFOSSIL_CORPUS_EVENTS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("LIBFOSSIL_CORPUS_EVENTS: %v", err)
		}
		target = n
	}

	// LIBFOSSIL_CORPUS_OUT keeps the crosslinked result somewhere a real
	// fossil binary can be pointed at afterwards.
	work := os.Getenv("LIBFOSSIL_CORPUS_OUT")
	if work == "" {
		work = filepath.Join(t.TempDir(), "corpus.fossil")
	}
	if err := os.Remove(work); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear previous output: %v", err)
	}
	src, err := os.ReadFile(corpus)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if err := os.WriteFile(work, src, 0o600); err != nil {
		t.Fatalf("write corpus copy: %v", err)
	}

	prep, err := db.Open(work)
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	for _, tbl := range derivedTables {
		if _, err := prep.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if _, err := prep.Exec("DELETE FROM tag WHERE tagid>11"); err != nil {
		t.Fatalf("clear tag: %v", err)
	}
	var blobs int
	if err := prep.QueryRow("SELECT count(*) FROM blob WHERE size>=0").Scan(&blobs); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if err := prep.Close(); err != nil {
		t.Fatalf("close prep: %v", err)
	}

	r, err := repo.Open(work)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	// No deferred r.Close here. Reaching the event target returns while the
	// sweep is still running, and closing the handle underneath it is a
	// use-after-close on a connection Crosslink is mid-query on. The sweep
	// goroutine owns r and closes it when Crosslink returns; nothing else
	// touches that handle. watch is a separate connection the test owns.
	watch, err := db.Open(work)
	if err != nil {
		t.Fatalf("open watcher: %v", err)
	}
	defer watch.Close()

	if p := os.Getenv("LIBFOSSIL_CORPUS_CPUPROFILE"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatalf("create cpuprofile: %v", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			t.Fatalf("start cpuprofile: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	t.Logf("corpus %s: %d content blobs, waiting for %d event rows", corpus, blobs, target)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		defer r.Close()
		_, err := Crosslink(r)
		done <- err
	}()

	last := 0
	lastLog := start
	for {
		select {
		case err := <-done:
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Crosslink: %v (after %s)", err, elapsed)
			}
			var n int
			watch.QueryRow("SELECT count(*) FROM event").Scan(&n)
			t.Logf("FULL SWEEP: %d events in %s (%.1f events/sec)",
				n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
			return
		case <-time.After(250 * time.Millisecond):
		}
		var n int
		if err := watch.QueryRow("SELECT count(*) FROM event").Scan(&n); err != nil {
			continue
		}
		if n >= target {
			elapsed := time.Since(start)
			t.Logf("RESULT: %d events in %s (%.1f events/sec)",
				n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
			t.Log("sweep abandoned at the target and left running in the " +
				"background until the process exits; run this test on its own")
			return
		}
		if time.Since(lastLog) > 10*time.Second {
			t.Logf("progress: %d events after %s (%.1f events/sec)",
				n, time.Since(start).Round(time.Second), float64(n)/time.Since(start).Seconds())
			lastLog = time.Now()
			last = n
			_ = last
		}
	}
}
