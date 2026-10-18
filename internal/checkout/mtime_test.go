package checkout

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// past is a whole-second time well before any test runs; files given it look
// as fossil's checkouts look once their files have aged.
var past = time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)

// agedCheckout extracts the tip of r into a fresh directory, ages every
// file to past and scans once, so vfile.mtime holds each file's mtime.
func agedCheckout(t *testing.T, r *repo.Repo) *Checkout {
	t.Helper()
	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hello.txt", "src/main.go", "README.md"} {
		setFileTime(t, co, name, past)
	}
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}
	return co
}

func setFileTime(t *testing.T, co *Checkout, name string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(co.dir, name), when, when); err != nil {
		t.Fatal(err)
	}
}

// rewrite replaces name's content and then gives it the time when, so a test
// controls exactly what the scan sees.
func rewrite(t *testing.T, co *Checkout, name, body string, when time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(co.dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	setFileTime(t, co, name, when)
}

func vfileMTime(t *testing.T, co *Checkout, name string) int64 {
	t.Helper()
	vid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	var mtime int64
	if err := co.db.QueryRow(
		"SELECT coalesce(mtime, 0) FROM vfile WHERE vid=? AND pathname=?", int64(vid), name,
	).Scan(&mtime); err != nil {
		t.Fatalf("vfile.mtime of %s: %v", name, err)
	}
	return mtime
}

// statusOf runs a status scan the way fossil's changes command does and
// returns the names reported, by change.
func statusOf(t *testing.T, co *Checkout) map[string]FileChange {
	t.Helper()
	vid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]FileChange{}
	if err := co.VisitChanges(vid, true, func(e ChangeEntry) error {
		out[e.Name] = e.Change
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A scan records each file's mtime in whole seconds, as fossil's
// vfile_check_signature does.
func TestScanRecordsDiskMTime(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co := agedCheckout(t, r)

	for _, name := range []string{"hello.txt", "src/main.go", "README.md"} {
		if got := vfileMTime(t, co, name); got != past.Unix() {
			t.Errorf("vfile.mtime of %s = %d, want %d", name, got, past.Unix())
		}
	}
}

// With size and mtime as recorded, fossil does not hash the file, so an edit
// that keeps both is not seen; a hash scan (fossil's --hash) sees it.
func TestScanTrustsMatchingSizeAndMTime(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co := agedCheckout(t, r)

	rewrite(t, co, "hello.txt", "HELLO WORLD\n", past)
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status with mtime and size unchanged = %v, want nothing (fossil's fast path)", got)
	}
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}
	vid, _, _ := co.Version()
	var chnged int64
	if err := co.db.QueryRow("SELECT chnged FROM vfile WHERE vid=? AND pathname='hello.txt'",
		int64(vid)).Scan(&chnged); err != nil {
		t.Fatal(err)
	}
	if chnged != 1 {
		t.Fatalf("hash scan: chnged = %d, want 1", chnged)
	}
}

// A changed mtime makes the scan hash the file.
func TestScanHashesWhenMTimeDiffers(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co := agedCheckout(t, r)

	rewrite(t, co, "hello.txt", "HELLO WORLD\n", past.Add(time.Second))
	got := statusOf(t, co)
	if len(got) != 1 || got["hello.txt"] != ChangeModified {
		t.Fatalf("status = %v, want hello.txt modified", got)
	}
	if m := vfileMTime(t, co, "hello.txt"); m != past.Unix()+1 {
		t.Fatalf("vfile.mtime = %d, want %d", m, past.Unix()+1)
	}
}

// A changed size is an edit without hashing; an mtime put back changes nothing.
func TestScanSizeChangeIsAnEdit(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co := agedCheckout(t, r)

	rewrite(t, co, "README.md", "# Test, longer\n", past)
	got := statusOf(t, co)
	if len(got) != 1 || got["README.md"] != ChangeModified {
		t.Fatalf("status = %v, want README.md modified", got)
	}
}

// With the mtime-changes setting off, fossil hashes every file.
func TestScanMTimeChangesOff(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co := agedCheckout(t, r)
	if err := r.SetConfig("mtime-changes", "off"); err != nil {
		t.Fatal(err)
	}

	rewrite(t, co, "hello.txt", "HELLO WORLD\n", past)
	got := statusOf(t, co)
	if len(got) != 1 || got["hello.txt"] != ChangeModified {
		t.Fatalf("status with mtime-changes off = %v, want hello.txt modified", got)
	}
}

// An mtime not older than the scan's own second cannot vouch for the
// content: a write later in that second would keep it. Such a file is
// recorded with mtime 0, which fossil and go-libfossil both read as "hash
// it", so an edit in the same second is still seen.
func TestScanDoesNotRecordCurrentSecond(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co := agedCheckout(t, r)

	now := time.Now().Truncate(time.Second).Add(time.Second)
	rewrite(t, co, "hello.txt", "hello world\n", now)
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}
	if m := vfileMTime(t, co, "hello.txt"); m != 0 {
		t.Fatalf("vfile.mtime for a file of the current second = %d, want 0", m)
	}
	rewrite(t, co, "hello.txt", "HELLO WORLD\n", now)
	got := statusOf(t, co)
	if len(got) != 1 || got["hello.txt"] != ChangeModified {
		t.Fatalf("same-second edit: status = %v, want hello.txt modified", got)
	}
}

// fossil's vfile_check_signature, for a file present on disk.
func TestNextChangedState(t *testing.T) {
	cases := []struct {
		name                      string
		chnged                    int64
		baseline, sizeDiff, trust bool
		differs                   bool
		want                      int64
		wantHash                  bool
	}{
		{"unchanged, trusted", 0, true, false, true, true, 0, false},
		{"unchanged, mtime moved, same", 0, true, false, false, false, 0, true},
		{"unchanged, mtime moved, edited", 0, true, false, false, true, 1, true},
		{"unchanged, size moved", 0, true, true, true, false, 1, false},
		{"edited, same size, back to baseline", 1, true, false, true, false, 0, true},
		{"edited, same size, still edited", 1, true, false, true, true, 1, true},
		{"edited, size moved", 1, true, true, false, false, 1, false},
		{"merged, trusted", 2, true, false, true, true, 2, false},
		{"merged, mtime moved, edited", 2, true, false, false, true, 1, true},
		{"integrated, mtime moved, edited", 4, true, false, false, true, 1, true},
		{"merge-added, size moved", 3, true, true, false, false, 1, false},
		{"merge-added, same size", 3, true, false, false, true, 3, false},
		{"became exec, size moved", 6, true, true, false, false, 1, false},
		{"became exec, same size", 6, true, false, false, true, 6, false},
		{"integrate-added, same size", 5, true, false, false, true, 5, false},
		{"integrate-added, size moved", 5, true, true, false, false, 1, false},
		{"pending add", 0, false, true, false, false, 1, false},
		{"pending add, empty", 0, false, false, true, false, 1, false},
	}
	for _, tc := range cases {
		hashed := false
		differs := func() (bool, error) {
			hashed = true
			return tc.differs, nil
		}
		got, err := nextChangedState(tc.chnged, tc.baseline, tc.sizeDiff, tc.trust, differs)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want || hashed != tc.wantHash {
			t.Errorf("%s: chnged %d hashed %v, want %d hashed %v",
				tc.name, got, hashed, tc.want, tc.wantHash)
		}
	}
}

// Extract records the mtime of each file it writes, as fossil's
// vfile_to_disk does. MemStorage stamps writes with a clock that only moves
// forward from 1970, so every mtime is in the past and recordable.
func TestExtractRecordsMTime(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	rid, _, _ := co.Version()
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hello.txt", "src/main.go", "README.md"} {
		info, err := mem.Stat(filepath.Join("/checkout", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := vfileMTime(t, co, name); got == 0 || got != info.ModTime().Unix() {
			t.Errorf("vfile.mtime of %s = %d, want the file's %d", name, got, info.ModTime().Unix())
		}
	}
}

// A commit keeps the recorded mtime of every file it leaves matching its
// row, the committed ones included (fossil updates vfile in place). A file
// left out of a partial commit stays edited.
func TestCommitKeepsMTimes(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co := agedCheckout(t, r)

	edited := past.Add(time.Minute)
	rewrite(t, co, "hello.txt", "hello, committed\n", edited)
	rewrite(t, co, "README.md", "# Kept\n", edited)
	if err := co.Enqueue(EnqueueOpts{Paths: []string{"hello.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := co.Commit(CommitOpts{Message: "partial", User: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := vfileMTime(t, co, "hello.txt"); got != edited.Unix() {
		t.Errorf("committed file: vfile.mtime = %d, want %d", got, edited.Unix())
	}
	if got := vfileMTime(t, co, "src/main.go"); got != past.Unix() {
		t.Errorf("unchanged file: vfile.mtime = %d, want %d", got, past.Unix())
	}
	got := statusOf(t, co)
	if len(got) != 1 || got["README.md"] != ChangeModified {
		t.Fatalf("after partial commit: status = %v, want README.md modified", got)
	}
}

// Update carries an unchanged file's mtime to the new version's row, as
// fossil's update transfers it, and records the mtime of a file it writes.
func TestUpdateKeepsAndRecordsMTimes(t *testing.T) {
	r, rid1, rid2, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid1, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	kept := vfileMTime(t, co, "src/main.go")
	if kept == 0 {
		t.Fatal("extract recorded no mtime")
	}
	if _, err := co.Update(UpdateOpts{TargetRID: rid2}); err != nil {
		t.Fatal(err)
	}
	if got := vfileMTime(t, co, "src/main.go"); got != kept {
		t.Errorf("unchanged file: vfile.mtime = %d, want %d carried over", got, kept)
	}
	for _, name := range []string{"hello.txt", "new.txt"} {
		info, err := mem.Stat(filepath.Join("/checkout", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := vfileMTime(t, co, name); got != info.ModTime().Unix() {
			t.Errorf("written file %s: vfile.mtime = %d, want %d", name, got, info.ModTime().Unix())
		}
	}
}

// Revert records the mtime of the file it restores, as fossil's revert does.
func TestRevertRecordsMTime(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	rid, _, _ := co.Version()
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("/checkout", "hello.txt")
	if err := mem.WriteFile(path, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{Paths: []string{"hello.txt"}}); err != nil {
		t.Fatal(err)
	}
	info, err := mem.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := vfileMTime(t, co, "hello.txt"); got != info.ModTime().Unix() {
		t.Fatalf("vfile.mtime = %d, want the restored file's %d", got, info.ModTime().Unix())
	}
}

// Undoing a rename must not let the row vouch for content it never
// checked: the file moved back may be edited while keeping its mtime.
func TestRevertRenameKeepsEditVisible(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co := agedCheckout(t, r)

	if err := co.Rename(RenameOpts{From: "hello.txt", To: "greet.txt", DoFsMove: true}); err != nil {
		t.Fatal(err)
	}
	rewrite(t, co, "greet.txt", "HELLO WORLD\n", past.Add(time.Hour))
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}
	// Moved back by hand: a rename keeps the edited file's mtime.
	if _, err := co.RevertRename("greet.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(co.dir, "greet.txt"), filepath.Join(co.dir, "hello.txt")); err != nil {
		t.Fatal(err)
	}
	got := statusOf(t, co)
	if len(got) != 1 || got["hello.txt"] != ChangeModified {
		t.Fatalf("after undoing the rename: status = %v, want hello.txt modified", got)
	}
}

// --setmtime records the times it gives files, as fossil's
// CKSIG_SETMTIME scan does.
func TestSetMTimeRecordsMTime(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	rid, _, _ := co.Version()
	if err := co.Extract(rid, ExtractOpts{Force: true, SetMTime: true}); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	for _, name := range []string{"hello.txt", "src/main.go", "README.md"} {
		if got := vfileMTime(t, co, name); got != want {
			t.Errorf("vfile.mtime of %s = %d, want %d", name, got, want)
		}
	}
}

// A same-size edit in the second the file was scanned is committed. Fossil
// records that second and would commit nothing; go-libfossil records 0 and
// rehashes. The clock is fixed so the scan and the edit share a second.
func TestCommitSeesSameSecondEdit(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	rid, _, _ := co.Version()
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	second := past
	co.env = &simio.Env{Storage: simio.OSStorage{}, Clock: simio.NewSimClockAt(second), Rand: simio.CryptoRand{}}
	for _, name := range []string{"hello.txt", "src/main.go", "README.md"} {
		setFileTime(t, co, name, second)
	}
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}

	rewrite(t, co, "hello.txt", "HELLO WORLD\n", second)
	newRID, _, err := co.Commit(CommitOpts{Message: "same second", User: "test"})
	if err != nil {
		t.Fatalf("commit of a same-second edit: %v", err)
	}
	got, err := manifest.ListFiles(r, newRID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if f.Name != "hello.txt" {
			continue
		}
		if want := hash.ContentHash([]byte("HELLO WORLD\n"), f.UUID); f.UUID != want {
			t.Fatalf("committed hello.txt is %s, want the edit %s", f.UUID, want)
		}
		return
	}
	t.Fatal("hello.txt missing from the new check-in")
}
