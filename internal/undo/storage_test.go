//go:build !js

package undo_test

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/internal/undo"
	"github.com/danmestas/go-libfossil/simio"
)

// simCheckout is a checkout whose files live in a MemStorage under a clock
// well after every MemStorage write stamp; only the databases are on disk.
type simCheckout struct {
	co    *checkout.Checkout
	ckout *sql.DB
	env   *simio.Env
	dir   string
}

// newSimCheckout commits a.txt and b.txt, extracts them and scans once, so
// vfile records each file's mtime.
func newSimCheckout(t *testing.T) *simCheckout {
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
		Clock:   simio.NewSimClockAt(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)),
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
	return &simCheckout{co: co, ckout: ckout, env: env, dir: dir}
}

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

func (s *simCheckout) row(t *testing.T, name string) (chnged, mtime int64) {
	t.Helper()
	if err := s.ckout.QueryRow(
		"SELECT chnged, CAST(coalesce(mtime,0) AS INTEGER) FROM vfile WHERE pathname=?", name,
	).Scan(&chnged, &mtime); err != nil {
		t.Fatal(err)
	}
	return chnged, mtime
}

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

// TestUndoRedoThroughStorage: undo and redo read and write the checkout's
// storage, and each written file's row gets the written mtime, since the row
// it restores vouched for that content when it was saved.
func TestUndoRedoThroughStorage(t *testing.T) {
	s := newSimCheckout(t)
	if err := undo.Save(s.ckout, s.env, s.dir, nil); err != nil {
		t.Fatal(err)
	}
	s.write(t, "a.txt", "HELLO world\n")
	if got := s.changes(t); got["a.txt"] != checkout.ChangeModified {
		t.Fatalf("after edit: changes = %v", got)
	}

	if _, err := undo.Undo(s.ckout, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.read(t, "a.txt"); got != "hello world\n" {
		t.Fatalf("a.txt after undo = %q", got)
	}
	if chnged, mtime := s.row(t, "a.txt"); chnged != 0 || mtime == 0 || mtime != s.diskMTime(t, "a.txt") {
		t.Fatalf("a.txt row after undo: chnged=%d mtime=%d, disk %d", chnged, mtime, s.diskMTime(t, "a.txt"))
	}
	if got := s.changes(t); len(got) != 0 {
		t.Fatalf("after undo: changes = %v", got)
	}

	if _, err := undo.Redo(s.ckout, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.read(t, "a.txt"); got != "HELLO world\n" {
		t.Fatalf("a.txt after redo = %q", got)
	}
	if chnged, mtime := s.row(t, "a.txt"); chnged != 1 || mtime != s.diskMTime(t, "a.txt") {
		t.Fatalf("a.txt row after redo: chnged=%d mtime=%d, disk %d", chnged, mtime, s.diskMTime(t, "a.txt"))
	}
	if got := s.changes(t); len(got) != 1 || got["a.txt"] != checkout.ChangeModified {
		t.Fatalf("after redo: changes = %v", got)
	}
}

// TestUndoOfUnscannedEditRecordsNothing: a row saved while its file had an
// edit no scan had seen does not vouch for the saved content, so undo
// restores the file without recording its mtime and the edit is still
// reported, even one of the same size.
func TestUndoOfUnscannedEditRecordsNothing(t *testing.T) {
	s := newSimCheckout(t)
	s.write(t, "a.txt", "HELLO world\n")
	if err := undo.Save(s.ckout, s.env, s.dir, nil); err != nil {
		t.Fatal(err)
	}
	s.write(t, "a.txt", "hello WORLD\n")

	if _, err := undo.Undo(s.ckout, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.read(t, "a.txt"); got != "HELLO world\n" {
		t.Fatalf("a.txt after undo = %q", got)
	}
	if chnged, mtime := s.row(t, "a.txt"); chnged != 0 || mtime != 0 {
		t.Fatalf("a.txt row after undo: chnged=%d mtime=%d, want 0 0", chnged, mtime)
	}
	if got := s.changes(t); got["a.txt"] != checkout.ChangeModified {
		t.Fatalf("after undo: changes = %v", got)
	}
}

// TestUndoRestoresDeletedFile: a file deleted after the save is written back
// through storage, and one created after it is removed from storage.
func TestUndoRestoresDeletedFile(t *testing.T) {
	s := newSimCheckout(t)
	if err := undo.Save(s.ckout, s.env, s.dir, []string{"b.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := s.env.Storage.Remove(filepath.Join(s.dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := undo.Undo(s.ckout, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.read(t, "b.txt"); got != "bee\n" {
		t.Fatalf("b.txt after undo = %q", got)
	}
	if _, err := undo.Redo(s.ckout, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.read(t, "b.txt"); ok {
		t.Fatal("b.txt still in storage after redo")
	}
	if got := s.changes(t); got["b.txt"] != checkout.ChangeMissing {
		t.Fatalf("after redo: changes = %v", got)
	}
}

// captureStdout runs fn and returns what it wrote to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestUndoRedoReturnChanges: undo and redo return what they did to each
// file, in the order fossil undoes them, and print nothing.
func TestUndoRedoReturnChanges(t *testing.T) {
	s := newSimCheckout(t)
	if err := undo.Save(s.ckout, s.env, s.dir, nil); err != nil {
		t.Fatal(err)
	}
	s.write(t, "a.txt", "HELLO world\n")
	if err := s.env.Storage.Remove(filepath.Join(s.dir, "b.txt")); err != nil {
		t.Fatal(err)
	}

	var undone, redone []undo.Change
	out := captureStdout(t, func() {
		var err error
		if undone, err = undo.Undo(s.ckout, s.env, s.dir); err != nil {
			t.Fatal(err)
		}
		if redone, err = undo.Redo(s.ckout, s.env, s.dir); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Errorf("undo and redo printed %q", out)
	}
	wantUndo := []undo.Change{{Pathname: "a.txt", Action: undo.Restored}, {Pathname: "b.txt", Action: undo.Created}}
	if !reflect.DeepEqual(undone, wantUndo) {
		t.Errorf("undo changes = %v, want %v", undone, wantUndo)
	}
	wantRedo := []undo.Change{{Pathname: "a.txt", Action: undo.Restored}, {Pathname: "b.txt", Action: undo.Deleted}}
	if !reflect.DeepEqual(redone, wantRedo) {
		t.Errorf("redo changes = %v, want %v", redone, wantRedo)
	}
}
