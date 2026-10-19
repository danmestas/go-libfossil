//go:build unix

package stash_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/stash"
	"github.com/danmestas/go-libfossil/simio"
)

// newLinkCheckout commits plain.txt, other.txt and link, a symlink to
// plain.txt, with allow-symlinks on, and checks them out on disk.
func newLinkCheckout(t *testing.T) *simCheckout {
	t.Helper()
	r, err := repo.CreateWithEnv(filepath.Join(t.TempDir(), "r.fossil"), "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "plain.txt", Content: []byte("plain\n")},
			{Name: "other.txt", Content: []byte("other\n")},
			{Name: "link", Content: []byte("plain.txt"), Perm: "l"},
		},
		Comment: "initial", User: "test", Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := simio.RealEnv()
	dir := t.TempDir()
	co, err := checkout.Create(r, dir, checkout.CreateOpts{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(rid, checkout.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	ckout, err := libdb.OpenSQL(filepath.Join(dir, checkout.PreferredDBName()), libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ckout.Close() })
	return &simCheckout{co: co, ckout: ckout, repoDB: r.DB().SqlDB(), env: env, dir: dir}
}

func wantLink(t *testing.T, dir, name, target string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(dir, name))
	if err != nil || got != target {
		t.Fatalf("%s: Readlink = %q, %v; want a symlink to %s", name, got, err, target)
	}
}

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

// A repointed symlink is stashed as the link it is, as fossil's stash reads
// it with RepoFILE and records file_islink; the stash's revert makes it a
// link to its committed target again, and Pop repoints it, each time
// leaving what the link points to alone.
func TestStashRepointedSymlink(t *testing.T) {
	s := newLinkCheckout(t)
	wantLink(t, s.dir, "link", "plain.txt")
	if err := os.Remove(filepath.Join(s.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.txt", filepath.Join(s.dir, "link")); err != nil {
		t.Fatal(err)
	}

	if err := stash.Save(s.co, s.ckout, s.repoDB, s.env, s.dir, "repoint"); err != nil {
		t.Fatal(err)
	}
	var isLink int64
	if err := s.ckout.QueryRow("SELECT isLink FROM stashfile WHERE newname='link'").Scan(&isLink); err != nil {
		t.Fatal(err)
	}
	if isLink != 1 {
		t.Errorf("stashfile.isLink of link = %d, want 1", isLink)
	}
	wantLink(t, s.dir, "link", "plain.txt")
	wantFile(t, s.dir, "plain.txt", "plain\n")
	wantFile(t, s.dir, "other.txt", "other\n")
	if got := s.changes(t); len(got) != 0 {
		t.Fatalf("changes after stash = %v, want nothing", got)
	}

	if err := stash.Pop(s.ckout, s.repoDB, s.env, s.dir); err != nil {
		t.Fatal(err)
	}
	wantLink(t, s.dir, "link", "other.txt")
	wantFile(t, s.dir, "plain.txt", "plain\n")
	wantFile(t, s.dir, "other.txt", "other\n")
	if got := s.changes(t); len(got) != 1 || got["link"] != checkout.ChangeModified {
		t.Fatalf("changes after pop = %v, want link modified", got)
	}
}
