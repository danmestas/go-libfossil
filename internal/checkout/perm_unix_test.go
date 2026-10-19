//go:build unix

package checkout

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// permRepo is a repository whose one check-in holds a plain file, an
// executable and a symlink to the plain file.
func permRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.CreateWithEnv(filepath.Join(t.TempDir(), "p.fossil"), "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "plain.txt", Content: []byte("plain\n")},
			{Name: "run.sh", Content: []byte("#!/bin/sh\n"), Perm: "x"},
			{Name: "link", Content: []byte("plain.txt"), Perm: "l"},
		},
		Comment: "initial", User: "test", Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// permCheckout extracts r's tip to disk, with link a real symlink as fossil
// writes it while allow-symlinks is on, and scans once.
func permCheckout(t *testing.T, r *repo.Repo) *Checkout {
	t.Helper()
	co, err := Create(r, t.TempDir(), CreateOpts{})
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
	replaceWithSymlink(t, co, "link", "plain.txt")
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}
	return co
}

func replaceWithSymlink(t *testing.T, co *Checkout, name, target string) {
	t.Helper()
	p := filepath.Join(co.dir, name)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, co *Checkout, name string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(filepath.Join(co.dir, name), mode); err != nil {
		t.Fatal(err)
	}
}

func chngedOf(t *testing.T, co *Checkout, name string) int64 {
	t.Helper()
	var chnged int64
	if err := co.db.QueryRow("SELECT chnged FROM vfile WHERE pathname=?", name).
		Scan(&chnged); err != nil {
		t.Fatalf("chnged of %s: %v", name, err)
	}
	return chnged
}

// Checkout records a symlink's l permission in vfile.islink, as fossil's
// load_vfile_from_rid does.
func TestLoadVFileRecordsSymlink(t *testing.T) {
	co := permCheckout(t, permRepo(t))
	var isexe, islink int64
	if err := co.db.QueryRow("SELECT isexe, islink FROM vfile WHERE pathname='link'").
		Scan(&isexe, &islink); err != nil {
		t.Fatal(err)
	}
	if isexe != 0 || islink != 1 {
		t.Fatalf("link: isexe %d islink %d, want 0 and 1", isexe, islink)
	}
}

// A file missing from disk is marked changed and its mtime recorded as -1,
// as fossil's scan does.
func TestScanMarksMissingFileChanged(t *testing.T) {
	co := permCheckout(t, permRepo(t))
	if err := os.Remove(filepath.Join(co.dir, "plain.txt")); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}
	if got := chngedOf(t, co, "plain.txt"); got != 1 {
		t.Fatalf("missing file: chnged %d, want 1", got)
	}
	if got := vfileMTime(t, co, "plain.txt"); got != -1 {
		t.Fatalf("missing file: mtime %d, want -1", got)
	}
}

// With allow-symlinks off (fossil's default) a symlink is seen as what it
// points to, so a tracked symlink reads as a plain file: fossil reports it
// UNLINK on a fresh checkout.
func TestScanSymlinksOff(t *testing.T) {
	co := permCheckout(t, permRepo(t))
	got := statusOf(t, co)
	if len(got) != 1 || got["link"] != ChangeUnlink {
		t.Fatalf("status = %v, want link unlink", got)
	}
}

// The scan sets chnged 6 through 9 for executable and symlink changes, and
// back to 0 once they are undone.
func TestScanSeesModeChanges(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	co := permCheckout(t, r)
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("fresh checkout: status %v, want nothing", got)
	}
	chmod(t, co, "plain.txt", 0o755)
	chmod(t, co, "run.sh", 0o644)
	if err := os.Remove(filepath.Join(co.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(co.dir, "link"), []byte("plain.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := statusOf(t, co)
	want := map[string]FileChange{
		"plain.txt": ChangeExecutable, "run.sh": ChangeUnexec, "link": ChangeUnlink,
	}
	assertStatus(t, got, want)

	chmod(t, co, "plain.txt", 0o644)
	chmod(t, co, "run.sh", 0o755)
	replaceWithSymlink(t, co, "link", "plain.txt")
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("modes put back: status %v, want nothing", got)
	}
	replaceWithSymlink(t, co, "plain.txt", "run.sh")
	assertStatus(t, statusOf(t, co), map[string]FileChange{"plain.txt": ChangeSymlink})
}

// A storage without file modes, such as MemStorage, keeps the recorded
// permissions, as fossil on Windows does: an executable is not seen to lose
// its bit.
func TestScanWithoutModesKeepsPermissions(t *testing.T) {
	co, _ := memCheckout(t, permRepo(t), simio.RealClock{})
	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status = %v, want nothing", got)
	}
}

// Commit records the permission it finds on disk: x for an executable and,
// with allow-symlinks on, l for a symlink whose content is its target.
func TestCommitRecordsModeChanges(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	co := permCheckout(t, r)
	chmod(t, co, "plain.txt", 0o755)
	chmod(t, co, "run.sh", 0o644)
	replaceWithSymlink(t, co, "link", "run.sh")
	rid, _, err := co.Commit(CommitOpts{Message: "modes", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	perms := map[string]string{}
	for _, f := range files {
		perms[f.Name] = f.Perm
	}
	want := map[string]string{"plain.txt": "x", "run.sh": "", "link": "l"}
	for name, perm := range want {
		if perms[name] != perm {
			t.Errorf("%s: perm %q, want %q", name, perms[name], perm)
		}
	}
	if got := readCommitted(t, r, rid, "link"); got != "run.sh" {
		t.Errorf("link content %q, want its target run.sh", got)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("after commit: status %v, want nothing", got)
	}
}

func readCommitted(t *testing.T, r *repo.Repo, rid libfossil.FslID, name string) string {
	t.Helper()
	data, err := content.Expand(r.DB(), fileRIDByName(t, r, rid, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertStatus(t *testing.T, got, want map[string]FileChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("status = %v, want %v", got, want)
	}
	for name, change := range want {
		if got[name] != change {
			t.Errorf("%s: %v, want %v", name, got[name], change)
		}
	}
}
