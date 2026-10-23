package verify

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestCorpusCheckBlobs times the blob-integrity sweep Verify and Rebuild open
// with, against a real repository. Skipped unless LIBFOSSIL_CORPUS names one.
func TestCorpusCheckBlobs(t *testing.T) {
	r := openCorpusCopy(t)
	report := &Report{}
	start := time.Now()
	if err := checkBlobs(r, report); err != nil {
		t.Fatalf("checkBlobs: %v", err)
	}
	t.Logf("checked %d blobs in %s: %d ok, %d failed",
		report.BlobsChecked, time.Since(start).Round(time.Millisecond),
		report.BlobsOK, report.BlobsFailed)
	if !report.OK() {
		t.Errorf("issues: %d, first: %+v", len(report.Issues), report.Issues[0])
	}
}

// TestCorpusRebuild times a whole Rebuild of a real repository. Skipped unless
// LIBFOSSIL_CORPUS names one.
func TestCorpusRebuild(t *testing.T) {
	r := openCorpusCopy(t)
	peak := samplePeakHeap(t)
	report, err := Rebuild(r)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	t.Logf("rebuilt in %s: %d blobs checked, %d failed, peak heap %d MiB",
		report.Duration.Round(time.Millisecond), report.BlobsChecked, report.BlobsFailed,
		peak()>>20)
	if !report.OK() {
		t.Errorf("issues: %d, first: %+v", len(report.Issues), report.Issues[0])
	}
}

// TestCorpusRebuildKeepsAbsences rebuilds a real sparse repository and checks
// that every phantom classifies as it did before: a rebuild empties the
// tables classification reads, and must refill them. Skipped unless
// LIBFOSSIL_CORPUS names one.
func TestCorpusRebuildKeepsAbsences(t *testing.T) {
	r := openCorpusCopy(t)
	before := corpusAbsences(t, r)
	if _, err := Rebuild(r); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	after := corpusAbsences(t, r)
	// A rebuild links a reference to an artifact the store never held against
	// a new phantom, so the phantoms may grow; none may disappear or change.
	counts := map[string]int{}
	for rid, b := range before {
		if a, ok := after[rid]; !ok || a != b {
			t.Errorf("rid %d: %+v before, %+v after (held %v)", rid, b, a, ok)
		}
		counts[fmt.Sprintf("class=%d", b.class)]++
	}
	t.Logf("%d phantoms unchanged by rebuild (%d after): %v", len(before), len(after), counts)
}

type corpusAbsence struct {
	class          content.Absence
	below, unnamed bool
}

func corpusAbsences(t *testing.T, r *repo.Repo) map[int64]corpusAbsence {
	t.Helper()
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		t.Fatalf("LoadAbsences: %v", err)
	}
	rows, err := r.DB().Query("SELECT rid FROM blob WHERE size<0")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]corpusAbsence{}
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			t.Fatal(err)
		}
		id := libfossil.FslID(rid)
		out[rid] = corpusAbsence{a.Class(id), a.Below(id), a.Unnamed(id)}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestCorpusVerify times a whole Verify of a real repository and logs the
// issue count; a corpus can carry derived-table drift Verify rightly reports.
// Skipped unless LIBFOSSIL_CORPUS names one.
func TestCorpusVerify(t *testing.T) {
	r := openCorpusCopy(t)
	report, err := Verify(r)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	t.Logf("verified in %s: %d blobs checked, %d failed, %d issues",
		report.Duration.Round(time.Millisecond), report.BlobsChecked,
		report.BlobsFailed, len(report.Issues))
	for i, issue := range report.Issues {
		if i == 5 {
			break
		}
		t.Logf("issue: %+v", issue)
	}
}

// samplePeakHeap polls the Go heap until the test ends and returns a func
// reporting the highest HeapInuse seen, in bytes.
func samplePeakHeap(t *testing.T) func() uint64 {
	t.Helper()
	var peak atomic.Uint64
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	return peak.Load
}

// openCorpusCopy opens a private copy of the LIBFOSSIL_CORPUS repository, or
// skips the test when none is named.
func openCorpusCopy(t *testing.T) *repo.Repo {
	t.Helper()
	corpus := os.Getenv("LIBFOSSIL_CORPUS")
	if corpus == "" {
		t.Skip("set LIBFOSSIL_CORPUS to a .fossil repository to run")
	}

	// Work on a copy: opening a repository creates WAL sidecars next to it.
	// Stream it, so the copy does not count toward the sweep's peak RSS.
	work := filepath.Join(t.TempDir(), "corpus.fossil")
	if err := copyFile(work, corpus); err != nil {
		t.Fatalf("copy corpus: %v", err)
	}
	r, err := repo.Open(work)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func copyFile(dst, src string) error {
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
