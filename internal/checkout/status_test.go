package checkout

import (
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

func TestHasChangesClean(t *testing.T) {
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

	has, err := co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("clean checkout should not have changes")
	}
}

func TestHasChangesModified(t *testing.T) {
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

	// Modify file and scan
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}

	has, err := co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("should have changes after modification")
	}
}

func TestVisitChanges(t *testing.T) {
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

	// Modify a file
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// VisitChanges with scan=true should detect the change
	var entries []ChangeEntry
	err = co.VisitChanges(rid, true, func(e ChangeEntry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 change, got %d", len(entries))
	}
	if entries[0].Name != "hello.txt" {
		t.Fatalf("changed file = %q, want hello.txt", entries[0].Name)
	}
	if entries[0].Change != ChangeModified {
		t.Fatalf("change type = %d, want ChangeModified", entries[0].Change)
	}
}

func TestVisitChangesNoScan(t *testing.T) {
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

	// Modify a file but don't scan
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// VisitChanges with scan=false should NOT detect the change
	var entries []ChangeEntry
	err = co.VisitChanges(rid, false, func(e ChangeEntry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 changes without scan, got %d", len(entries))
	}
}

func TestVisitChangesMultiple(t *testing.T) {
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

	// Modify multiple files
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/checkout/README.md", []byte("changed2"), 0644); err != nil {
		t.Fatal(err)
	}

	// VisitChanges should detect both changes
	var entries []ChangeEntry
	err = co.VisitChanges(rid, true, func(e ChangeEntry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(entries))
	}

	// Check that both files are present
	found := make(map[string]bool)
	for _, e := range entries {
		if e.Change != ChangeModified {
			t.Errorf("file %s: expected ChangeModified, got %d", e.Name, e.Change)
		}
		found[e.Name] = true
	}
	if !found["hello.txt"] || !found["README.md"] {
		t.Fatal("expected hello.txt and README.md to be in changes")
	}
}

// TestVisitChangesFossilRowShapes pins how row shapes the fossil binary writes
// are classified (#228). It writes three rows straight into vfile, then scans:
//   - new.txt: a fossil add (rid=0, chnged left 0) with the file on disk; the
//     scan promotes it to chnged=1 and it reports as added.
//   - gone.txt: a row with no artifact and no file on disk, the shape
//     LoadVFile writes for content missing from the repo; not a change.
//   - hello.txt: untouched, with origname equal to pathname; not a rename.
func TestVisitChangesFossilRowShapes(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	if err := mem.WriteFile("/checkout/new.txt", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"UPDATE vfile SET origname = pathname WHERE vid = ?",
		"INSERT INTO vfile(vid, pathname, rid, mrid, chnged) VALUES(?, 'new.txt', 0, 0, 0)",
		"INSERT INTO vfile(vid, pathname, rid, mrid, chnged) VALUES(?, 'gone.txt', 0, 0, 0)",
	} {
		if _, err := co.db.Exec(stmt, int64(rid)); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	err = co.VisitChanges(rid, true, func(e ChangeEntry) error {
		got = append(got, e.Name)
		if e.Change != ChangeAdded {
			t.Errorf("%s: change = %v, want ChangeAdded", e.Name, e.Change)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "new.txt" {
		t.Fatalf("visited %q, want only new.txt", got)
	}
}

// TestNextChangedState pins the chnged transitions against fossil's
// vfile_check_signature, for a file present on disk.
func TestNextChangedState(t *testing.T) {
	cases := []struct {
		chnged      int64
		hasBaseline bool
		differs     bool
		want        int64
	}{
		{0, false, false, 1}, // fossil add: always a change
		{1, false, false, 1},
		{3, false, false, 3}, // added by merge: kept
		{0, true, false, 0},
		{0, true, true, 1},
		{1, true, false, 0}, // edit reverted by hand
		{1, true, true, 1},
		{2, true, false, 2}, // updated by merge: kept while untouched
		{2, true, true, 1},  // ...and edited on top
		{4, true, true, 1},
		{5, true, true, 5}, // merge conflict: kept
		{5, true, false, 5},
	}
	for _, tc := range cases {
		got := nextChangedState(tc.chnged, tc.hasBaseline, tc.differs)
		if got != tc.want {
			t.Errorf("nextChangedState(%d, %v, %v) = %d, want %d",
				tc.chnged, tc.hasBaseline, tc.differs, got, tc.want)
		}
	}
}
