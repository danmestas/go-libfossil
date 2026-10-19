//go:build unix

package checkout

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// repointedLinkRepo commits, with allow-symlinks on, version a, where link
// points to plain.txt, and its child b, where link points to other.txt.
func repointedLinkRepo(t *testing.T) (r *repo.Repo, a, b libfossil.FslID) {
	t.Helper()
	r, err := repo.CreateWithEnv(filepath.Join(t.TempDir(), "l.fossil"), "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	files := func(target string) []manifest.File {
		return []manifest.File{
			{Name: "plain.txt", Content: []byte("plain\n")},
			{Name: "other.txt", Content: []byte("other\n")},
			{Name: "link", Content: []byte(target), Perm: "l"},
		}
	}
	a, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: files("plain.txt"), Comment: "a", User: "test",
		Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: files("other.txt"), Comment: "b", User: "test", Parent: a,
		Time: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, a, b
}

// wantLink fails unless name in co is a symlink to target.
func wantLink(t *testing.T, dir, name, target string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(dir, name))
	if err != nil || got != target {
		t.Fatalf("%s: Readlink = %q, %v; want a symlink to %s", name, got, err, target)
	}
}

// wantFile fails unless name in dir is a regular file holding body.
func wantFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s: %v, %v; want a regular file", name, info, err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != body {
		t.Fatalf("%s = %q, %v; want %q", name, data, err, body)
	}
}

// Undo of an Update that repointed a symlink puts the link back as a link,
// as fossil's undo_one deletes it and calls symlink_create, and leaves what
// either target points to alone. Redo repoints it again.
func TestUndoUpdateRestoresSymlink(t *testing.T) {
	r, a, b := repointedLinkRepo(t)
	co, _ := createdCheckout(t, r)
	if err := co.Extract(b, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	wantLink(t, co.dir, "link", "other.txt")
	if _, err := co.Update(UpdateOpts{TargetRID: a}); err != nil {
		t.Fatal(err)
	}
	wantLink(t, co.dir, "link", "plain.txt")
	var isLink int64
	if err := co.db.QueryRow("SELECT isLink FROM undo WHERE pathname='link'").Scan(&isLink); err != nil {
		t.Fatal(err)
	}
	if isLink != 1 {
		t.Errorf("undo.isLink of link = %d, want 1 as fossil's undo_save records it", isLink)
	}

	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	wantLink(t, co.dir, "link", "other.txt")
	wantFile(t, co.dir, "plain.txt", "plain\n")
	wantFile(t, co.dir, "other.txt", "other\n")
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status after undo = %v, want nothing", got)
	}

	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	wantLink(t, co.dir, "link", "plain.txt")
	wantFile(t, co.dir, "plain.txt", "plain\n")
	wantFile(t, co.dir, "other.txt", "other\n")
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status after redo = %v, want nothing", got)
	}
}

// Undo never writes through a link: one pointing out of the checkout is
// replaced, and the file outside is left alone.
func TestUndoDoesNotWriteThroughOutsideLink(t *testing.T) {
	r, a, b := repointedLinkRepo(t)
	co, _ := createdCheckout(t, r)
	if err := co.Extract(b, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(UpdateOpts{TargetRID: a}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replaceWithSymlink(t, co, "link", outside)
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	wantLink(t, co.dir, "link", "other.txt")
	wantFile(t, filepath.Dir(outside), "outside.txt", "outside\n")
}

// Revert of a repointed symlink makes it a link to its committed target
// again, as fossil's revert does, and leaves what it pointed to alone.
func TestRevertRestoresRepointedSymlink(t *testing.T) {
	r, a, _ := repointedLinkRepo(t)
	co, _ := createdCheckout(t, r)
	if err := co.Extract(a, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	replaceWithSymlink(t, co, "link", "other.txt")
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	wantLink(t, co.dir, "link", "plain.txt")
	wantFile(t, co.dir, "other.txt", "other\n")
	wantFile(t, co.dir, "plain.txt", "plain\n")
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status after revert = %v, want nothing", got)
	}
}
