//go:build !js

package stash_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/stash"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

// simCheckout is a checkout whose files live in a MemStorage and whose
// clock is simulated; only the databases are on disk.
type simCheckout struct {
	co     *checkout.Checkout
	ckout  *sql.DB
	repoDB *sql.DB
	env    *simio.Env
	dir    string
}

// newSimCheckout commits a.txt and b.txt, extracts them into a MemStorage
// checkout with the clock at now, and scans once so vfile records each
// file's mtime.
func newSimCheckout(t *testing.T, now time.Time) *simCheckout {
	t.Helper()
	r, err := repo.CreateWithEnv(filepath.Join(t.TempDir(), "r.fossil"), "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("hello world\n")},
			{Name: "b.txt", Content: []byte("bee\n")},
		},
		Comment: "initial",
		User:    "test",
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &simio.Env{
		Clock:   simio.NewSimClockAt(now),
		Rand:    simio.NewSeededRand(1),
		Storage: simio.NewMemStorage(),
	}
	dir := t.TempDir()
	co, err := checkout.Create(r, dir, checkout.CreateOpts{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(rid, checkout.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}
	ckout, err := libdb.OpenSQL(filepath.Join(dir, checkout.PreferredDBName()), libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ckout.Close() })
	return &simCheckout{co: co, ckout: ckout, repoDB: r.DB().SqlDB(), env: env, dir: dir}
}

// later is a clock well after every MemStorage write stamp, so each write's
// mtime is recordable.
var later = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func (s *simCheckout) write(t *testing.T, name, body string) {
	t.Helper()
	if err := s.env.Storage.WriteFile(filepath.Join(s.dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (s *simCheckout) read(t *testing.T, name string) (string, bool) {
	t.Helper()
	data, err := s.env.Storage.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return "", false
	}
	return string(data), true
}

func (s *simCheckout) diskMTime(t *testing.T, name string) int64 {
	t.Helper()
	info, err := s.env.Storage.Stat(filepath.Join(s.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().Unix()
}

// row returns name's vfile chnged and mtime; ok is false when it has no row.
func (s *simCheckout) row(t *testing.T, name string) (chnged, mtime int64, ok bool) {
	t.Helper()
	err := s.ckout.QueryRow(
		"SELECT chnged, CAST(coalesce(mtime,0) AS INTEGER) FROM vfile WHERE pathname=?", name,
	).Scan(&chnged, &mtime)
	if err == sql.ErrNoRows {
		return 0, 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return chnged, mtime, true
}

// changes runs a status scan and returns what it reports, by name.
func (s *simCheckout) changes(t *testing.T) map[string]checkout.FileChange {
	t.Helper()
	vid, _, err := s.co.Version()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]checkout.FileChange{}
	if err := s.co.VisitChanges(vid, true, func(e checkout.ChangeEntry) error {
		out[e.Name] = e.Change
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (s *simCheckout) setMTime(t *testing.T, name string, secs int64) {
	t.Helper()
	when := time.Unix(secs, 0)
	if err := s.env.Storage.Chtimes(filepath.Join(s.dir, name), when, when); err != nil {
		t.Fatal(err)
	}
}

// TestSaveRevertsThroughStorage: stash save reads the edit from, and writes
// the baseline back to, the checkout's storage, and records the written
// file's mtime on its now unchanged row, as fossil's revert does.
func TestSaveRevertsThroughStorage(t *testing.T) {
	s := newSimCheckout(t, later)
	s.write(t, "a.txt", "HELLO world\n")
	if got := s.changes(t); got["a.txt"] != checkout.ChangeModified {
		t.Fatalf("before save: changes = %v", got)
	}

	if err := stash.Save(s.ckout, s.repoDB, s.env, s.dir, "edit"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.read(t, "a.txt"); got != "hello world\n" {
		t.Fatalf("a.txt after save = %q", got)
	}
	chnged, mtime, _ := s.row(t, "a.txt")
	if chnged != 0 || mtime != s.diskMTime(t, "a.txt") || mtime == 0 {
		t.Fatalf("a.txt row after save: chnged=%d mtime=%d, disk mtime %d",
			chnged, mtime, s.diskMTime(t, "a.txt"))
	}
	if got := s.changes(t); len(got) != 0 {
		t.Fatalf("after save: changes = %v", got)
	}
}

// TestPopRestoresThroughStorage: pop writes the stashed edit back through
// the checkout's storage and leaves the row saying what the scan would:
// changed, with the written file's mtime.
func TestPopRestoresThroughStorage(t *testing.T) {
	s := newSimCheckout(t, later)
	s.write(t, "a.txt", "HELLO world\n")
	s.changes(t) // stash save stashes what the last scan found
	if err := stash.Save(s.ckout, s.repoDB, s.env, s.dir, "edit"); err != nil {
		t.Fatal(err)
	}
	if err := stash.Pop(s.ckout, s.repoDB, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.read(t, "a.txt"); got != "HELLO world\n" {
		t.Fatalf("a.txt after pop = %q", got)
	}
	chnged, mtime, _ := s.row(t, "a.txt")
	if chnged != 1 || mtime != s.diskMTime(t, "a.txt") {
		t.Fatalf("a.txt row after pop: chnged=%d mtime=%d, disk mtime %d",
			chnged, mtime, s.diskMTime(t, "a.txt"))
	}
	if got := s.changes(t); len(got) != 1 || got["a.txt"] != checkout.ChangeModified {
		t.Fatalf("after pop: changes = %v", got)
	}
}

// TestSameSizePopInSaveSecondDetected: a pop whose write lands on the very
// mtime the save recorded, with an edit of the same size, is still reported.
// Fossil's apply leaves the reverted row (chnged=0, that mtime) in place, so
// its next scan trusts it and misses the edit.
func TestSameSizePopInSaveSecondDetected(t *testing.T) {
	s := newSimCheckout(t, later)
	s.write(t, "a.txt", "HELLO world\n")
	s.changes(t) // stash save stashes what the last scan found
	if err := stash.Save(s.ckout, s.repoDB, s.env, s.dir, "edit"); err != nil {
		t.Fatal(err)
	}
	_, saved, _ := s.row(t, "a.txt")
	if err := stash.Pop(s.ckout, s.repoDB, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	s.setMTime(t, "a.txt", saved)
	if got := s.changes(t); got["a.txt"] != checkout.ChangeModified {
		t.Fatalf("same-size pop in the save's second: changes = %v", got)
	}
}

// TestSaveInWriteSecondRecordsNothing: a revert written in the clock's
// current second is recorded as 0, so a same-size edit in that second, which
// keeps the mtime, is still found.
func TestSaveInWriteSecondRecordsNothing(t *testing.T) {
	s := newSimCheckout(t, time.Unix(0, 0))
	s.write(t, "a.txt", "HELLO world\n")
	s.changes(t) // stash save stashes what the last scan found
	if err := stash.Save(s.ckout, s.repoDB, s.env, s.dir, "edit"); err != nil {
		t.Fatal(err)
	}
	if _, mtime, _ := s.row(t, "a.txt"); mtime != 0 {
		t.Fatalf("a.txt mtime recorded in the write's second = %d, want 0", mtime)
	}
	reverted := s.diskMTime(t, "a.txt")
	s.write(t, "a.txt", "hello WORLD\n")
	s.setMTime(t, "a.txt", reverted)
	if got := s.changes(t); got["a.txt"] != checkout.ChangeModified {
		t.Fatalf("same-second same-size edit: changes = %v", got)
	}
}

// TestApplyReaddsAddedFile: apply writes an added file back through storage
// and adds it again, as fossil's stash_apply does (stash_add_files_in_sfile);
// the stash keeps the entry.
func TestApplyReaddsAddedFile(t *testing.T) {
	s := newSimCheckout(t, later)
	s.write(t, "c.txt", "new file\n")
	if _, err := s.co.Manage(checkout.ManageOpts{Paths: []string{"c.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := stash.Save(s.ckout, s.repoDB, s.env, s.dir, "add"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.read(t, "c.txt"); ok {
		t.Fatal("c.txt still in storage after save")
	}
	if _, _, ok := s.row(t, "c.txt"); ok {
		t.Fatal("c.txt still in vfile after save")
	}

	entries, err := stash.List(s.ckout)
	if err != nil || len(entries) != 1 {
		t.Fatalf("stash list = %v, %v", entries, err)
	}
	if err := stash.Apply(s.ckout, s.repoDB, s.env, s.dir, entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.read(t, "c.txt"); got != "new file\n" {
		t.Fatalf("c.txt after apply = %q", got)
	}
	if got := s.changes(t); len(got) != 1 || got["c.txt"] != checkout.ChangeAdded {
		t.Fatalf("after apply: changes = %v", got)
	}
	if entries, err := stash.List(s.ckout); err != nil || len(entries) != 1 {
		t.Fatalf("stash list after apply = %v, %v", entries, err)
	}
}

// TestSaveRestoresRemovedFile: a removed file comes back through storage on
// save and is gone from storage again after pop, its row left as fossil's
// apply leaves it (the file is missing, not removed).
func TestSaveRestoresRemovedFile(t *testing.T) {
	s := newSimCheckout(t, later)
	if err := s.env.Storage.Remove(filepath.Join(s.dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ckout.Exec("UPDATE vfile SET deleted=1 WHERE pathname='b.txt'"); err != nil {
		t.Fatal(err)
	}
	if err := stash.Save(s.ckout, s.repoDB, s.env, s.dir, "rm"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.read(t, "b.txt"); got != "bee\n" {
		t.Fatalf("b.txt after save = %q", got)
	}
	if chnged, mtime, _ := s.row(t, "b.txt"); chnged != 0 || mtime != s.diskMTime(t, "b.txt") {
		t.Fatalf("b.txt row after save: chnged=%d mtime=%d", chnged, mtime)
	}
	if err := stash.Pop(s.ckout, s.repoDB, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.read(t, "b.txt"); ok {
		t.Fatal("b.txt still in storage after pop")
	}
	if got := s.changes(t); got["b.txt"] != checkout.ChangeMissing {
		t.Fatalf("after pop: changes = %v", got)
	}
}
