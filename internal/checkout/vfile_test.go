package checkout

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

func TestLoadVFile(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	missing, err := co.LoadVFile(rid, true)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("expected 0 missing, got %d", missing)
	}

	// Verify 3 rows in vfile
	var count int
	co.db.QueryRow("SELECT count(*) FROM vfile WHERE vid=?", int64(rid)).Scan(&count)
	if count != 3 {
		t.Fatalf("expected 3 vfile rows, got %d", count)
	}

	// Verify pathnames
	rows, _ := co.db.Query("SELECT pathname FROM vfile WHERE vid=? ORDER BY pathname", int64(rid))
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		names = append(names, name)
	}
	expected := []string{"README.md", "hello.txt", "src/main.go"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d names, got %d", len(expected), len(names))
	}
	for i, n := range expected {
		if names[i] != n {
			t.Fatalf("vfile[%d] = %q, want %q", i, names[i], n)
		}
	}
}

func TestUnloadVFile(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	co.LoadVFile(rid, true)

	if err := co.UnloadVFile(rid); err != nil {
		t.Fatal(err)
	}

	var count int
	co.db.QueryRow("SELECT count(*) FROM vfile WHERE vid=?", int64(rid)).Scan(&count)
	if count != 0 {
		t.Fatalf("expected 0 vfile rows after unload, got %d", count)
	}
}

func TestLoadVFileClear(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()

	// Load vfile twice with different RIDs to test clear behavior
	// First load with clear=false
	missing, err := co.LoadVFile(rid, false)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("expected 0 missing, got %d", missing)
	}

	// Manually insert a row with a different vid to test clear
	_, err = co.db.Exec(`
		INSERT INTO vfile(vid, pathname, rid, mrid, mhash, isexe, islink)
		VALUES(999, 'dummy.txt', 0, 0, 'dummy', 0, 0)
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Verify we have rows for both vid=rid and vid=999
	var countAll int
	co.db.QueryRow("SELECT count(*) FROM vfile").Scan(&countAll)
	if countAll != 4 { // 3 for rid + 1 for dummy
		t.Fatalf("expected 4 total rows before clear, got %d", countAll)
	}

	// Load with clear=true should remove the dummy row
	_, err = co.LoadVFile(rid, true)
	if err != nil {
		t.Fatal(err)
	}

	// Verify only rid rows remain
	var countRid int
	co.db.QueryRow("SELECT count(*) FROM vfile WHERE vid=?", int64(rid)).Scan(&countRid)
	if countRid != 3 {
		t.Fatalf("expected 3 rows for vid=%d after clear, got %d", rid, countRid)
	}

	var countDummy int
	co.db.QueryRow("SELECT count(*) FROM vfile WHERE vid=999").Scan(&countDummy)
	if countDummy != 0 {
		t.Fatalf("expected 0 rows for vid=999 after clear, got %d", countDummy)
	}
}

func TestLoadVFileRIDAndMRIDSet(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	missing, err := co.LoadVFile(rid, true)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("expected 0 missing, got %d", missing)
	}

	// Verify that rid and mrid are both set and non-zero
	rows, _ := co.db.Query("SELECT pathname, rid, mrid FROM vfile WHERE vid=? ORDER BY pathname", int64(rid))
	defer rows.Close()
	for rows.Next() {
		var name string
		var ridVal, mridVal int64
		rows.Scan(&name, &ridVal, &mridVal)
		if ridVal == 0 {
			t.Errorf("file %s: rid is 0 (expected non-zero)", name)
		}
		if mridVal == 0 {
			t.Errorf("file %s: mrid is 0 (expected non-zero)", name)
		}
		if ridVal != mridVal {
			t.Errorf("file %s: rid=%d != mrid=%d (expected equal)", name, ridVal, mridVal)
		}
	}
}

func TestScanChangesModified(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()

	// Extract files to MemStorage
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Modify a file
	if err := mem.WriteFile("/checkout/hello.txt", []byte("modified content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Scan
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}

	// Check vfile.chnged for hello.txt
	var chnged int
	if err := co.db.QueryRow("SELECT chnged FROM vfile WHERE vid=? AND pathname='hello.txt'", int64(rid)).Scan(&chnged); err != nil {
		t.Fatal(err)
	}
	if chnged != 1 {
		t.Fatalf("hello.txt chnged = %d, want 1", chnged)
	}

	// Unmodified files should remain chnged=0
	if err := co.db.QueryRow("SELECT chnged FROM vfile WHERE vid=? AND pathname='README.md'", int64(rid)).Scan(&chnged); err != nil {
		t.Fatal(err)
	}
	if chnged != 0 {
		t.Fatalf("README.md chnged = %d, want 0", chnged)
	}
}

func TestScanChangesMissing(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Delete a file from MemStorage
	if err := mem.Remove("/checkout/hello.txt"); err != nil {
		t.Fatal(err)
	}

	// Scan should not error (missing files are noted, not fatal)
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}
}

func TestScanChangesNoHash(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{
		Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{},
	}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}

	// Modify a file on disk.
	if err := mem.WriteFile(
		"/checkout/hello.txt", []byte("changed"), 0644,
	); err != nil {
		t.Fatal(err)
	}

	// ScanChanges(0) — no ScanHash flag — should NOT detect content
	// changes (only mtime-based, which is not yet implemented, so
	// chnged should remain 0).
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}

	var chnged int
	err = co.db.QueryRow(
		"SELECT chnged FROM vfile WHERE vid=? AND pathname='hello.txt'",
		int64(rid),
	).Scan(&chnged)
	if err != nil {
		t.Fatal(err)
	}
	if chnged != 0 {
		t.Fatalf("without ScanHash, chnged = %d, want 0", chnged)
	}
}

func TestScanChangesObserver(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()

	var scanEnd *ScanEnd
	co.obs = &testObserver{
		onScanStarted: func(ctx context.Context) context.Context {
			return ctx
		},
		onScanCompleted: func(ctx context.Context, e ScanEnd) {
			scanEnd = &e
		},
	}
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}

	if scanEnd == nil {
		t.Fatal("observer ScanCompleted not called")
	}
	if scanEnd.FilesScanned != 3 {
		t.Fatalf("scanned = %d, want 3", scanEnd.FilesScanned)
	}
	if scanEnd.FilesChanged != 1 {
		t.Fatalf("changed = %d, want 1", scanEnd.FilesChanged)
	}
}
