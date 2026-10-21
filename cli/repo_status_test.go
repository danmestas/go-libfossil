package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"
)

// status reports a checkout's changes against the version it has checked out,
// from inside the checkout with the default --dir ".". Two faults it had:
// the walk skipped its own root, named ".", as a hidden directory, so every
// file read MISSING; and it compared the disk with tip, so a checkout of an
// older version read as edited. Method: commit two versions, check out the
// first, run status clean, then edit a file and run it again.
func TestRepoStatusReportsCheckoutChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repo.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := r.Commit(libfossil.CommitOpts{User: "u", Comment: "one", Files: []libfossil.FileToCommit{
		{Name: "a.txt", Content: []byte("a\n")},
		{Name: "src/b.txt", Content: []byte("b\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{User: "u", Comment: "two", ParentID: first,
		Files: []libfossil.FileToCommit{
			{Name: "a.txt", Content: []byte("a\n")},
			{Name: "src/b.txt", Content: []byte("b at tip\n")},
		}}); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	co, err := r.CreateCheckout(work, libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(first, libfossil.ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := co.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	status := func() string {
		return captureStdout(t, func() error {
			return (&cli.RepoStatusCmd{Dir: "."}).Run(&cli.Globals{Repo: path})
		})
	}
	if out := status(); out != "" {
		t.Fatalf("status of a clean checkout of the first version = %q, want none", out)
	}
	if err := os.WriteFile(filepath.Join(work, "src", "b.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, want := status(), "EDITED   src/b.txt\n"; out != want {
		t.Fatalf("status after an edit = %q, want %q", out, want)
	}
}
