package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// repo verify reports elided artifacts apart from damage and from transit
// phantoms, and calls a store complete within its cone healthy
// (sparse-cone §5.2). Method: commit inside and outside a cone, narrow the
// store and compact it, then verify.
func TestRepoVerifyReportsElided(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{User: "u", Comment: "one", Files: []libfossil.FileToCommit{
		{Name: "src/a.txt", Content: []byte("inside\n")},
		{Name: "zzz/b.txt", Content: []byte("outside\n")},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Inner().DB().Exec("DELETE FROM unsent"); err != nil {
		t.Fatal(err)
	}
	c, err := libfossil.NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetStoreCone(c); err != nil {
		t.Fatal(err)
	}
	if n, err := r.CompactStore(context.Background()); err != nil || n != 1 {
		t.Fatalf("compact = %d, %v; want 1", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := requiredHTTPStdout(t, func() error {
		runErr = (&RepoVerifyCmd{}).Run(&Globals{Repo: path})
		return nil
	})
	if runErr != nil {
		t.Fatalf("verify of a store complete within its cone: %v\n%s", runErr, out)
	}
	if !strings.Contains(out, "0 transit phantoms, 1 elided\n") || !strings.HasSuffix(out, "repository integrity verified\n") {
		t.Fatalf("verify output = %q", out)
	}
}
