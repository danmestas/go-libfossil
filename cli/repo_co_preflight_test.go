package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// A late missing payload or overwrite refusal must not leave an earlier file
// exported. Existing destination bytes survive even when --force was requested.
func TestRepoCoPreflightPreservesDestination(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "missing"}[missing], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "repo.fossil")
			r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = r.Commit(libfossil.CommitOpts{User: "u", Files: []libfossil.FileToCommit{
				{Name: "a.txt", Content: []byte("new a")},
				{Name: "z.txt", Content: []byte("new z")},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if missing {
				_, err = r.Inner().DB().Exec(`UPDATE blob SET size=-1, content=NULL
					WHERE rid IN (SELECT fid FROM mlink WHERE fnid IN
					(SELECT fnid FROM filename WHERE name='z.txt'))`)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "out")
			if err := os.Mkdir(out, 0o755); err != nil {
				t.Fatal(err)
			}
			keep := "z.txt"
			if missing {
				keep = "a.txt"
			}
			if err := os.WriteFile(filepath.Join(out, keep), []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			err = (&RepoCoCmd{Dir: out, Force: missing}).Run(&Globals{Repo: path})
			if err == nil {
				t.Fatal("export unexpectedly succeeded")
			}
			if missing && !strings.Contains(err.Error(), "required file payload") {
				t.Fatalf("missing payload diagnostic: %v", err)
			}
			got, readErr := os.ReadFile(filepath.Join(out, keep))
			if readErr != nil || string(got) != "original" {
				t.Fatalf("destination changed: %q, %v", got, readErr)
			}
			entries, err := os.ReadDir(out)
			if err != nil || len(entries) != 1 {
				t.Fatalf("partial export left destination entries: %d, %v", len(entries), err)
			}
		})
	}
}
