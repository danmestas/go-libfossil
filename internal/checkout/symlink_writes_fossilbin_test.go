//go:build unix

package checkout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

// fossilRepointedLink commits with fossil, allow-symlinks on, version a,
// where link points to plain.txt, and its child b, where it points to
// elsewhere.txt, leaving b open at ck. It returns the repository and ck.
// The targets' names differ in length, so fossil's scan sees the repointed
// link within the second it was made.
func fossilRepointedLink(t *testing.T, bin string) (repoPath, ck string) {
	t.Helper()
	dir := t.TempDir()
	repoPath = filepath.Join(dir, "r.fossil")
	ck = filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, dir, "init", repoPath)
	runFossilIn(t, bin, ck, "open", repoPath)
	runFossilIn(t, bin, ck, "settings", "allow-symlinks", "on")
	for name, body := range map[string]string{"plain.txt": "plain\n", "elsewhere.txt": "elsewhere\n"} {
		if err := os.WriteFile(filepath.Join(ck, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(ck, "link")
	if err := os.Symlink("plain.txt", link); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, ck, "add", ".")
	runFossilIn(t, bin, ck, "commit", "-m", "a", "--tag", "va")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere.txt", link); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, ck, "commit", "-m", "b")
	return repoPath, ck
}

// goUpdate opens the checkout at ck and updates it to the version tagged
// tag.
func goUpdate(t *testing.T, repoPath, ck, tag string) {
	t.Helper()
	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var rid int64
	if err := r.DB().QueryRow(`SELECT rid FROM tagxref JOIN tag USING(tagid)
		WHERE tagname = 'sym-' || ? AND tagtype > 0`, tag).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	co, err := Open(r, ck, OpenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if _, err := co.Update(UpdateOpts{TargetRID: libfossil.FslID(rid)}); err != nil {
		t.Fatal(err)
	}
}

// goUndo opens the checkout at ck and undoes its last operation.
func goUndo(t *testing.T, repoPath, ck string) {
	t.Helper()
	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	co, err := Open(r, ck, OpenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
}

// Undo of an update that repointed a symlink leaves the same files whether
// fossil or go-libfossil updates and whether either undoes: fossil's undo
// reads the undo state go-libfossil saves, and go-libfossil's the state
// fossil saves, each restoring the link as a link.
func TestUndoSymlinkMatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	for _, tc := range []struct {
		name             string
		goUpdate, goUndo bool
	}{
		{"fossil update, fossil undo", false, false},
		{"go update, fossil undo", true, false},
		{"fossil update, go undo", false, true},
		{"go update, go undo", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoPath, ck := fossilRepointedLink(t, bin)
			if tc.goUpdate {
				goUpdate(t, repoPath, ck, "va")
			} else {
				runFossilIn(t, bin, ck, "update", "va")
			}
			wantLink(t, ck, "link", "plain.txt")
			if tc.goUndo {
				goUndo(t, repoPath, ck)
			} else {
				runFossilIn(t, bin, ck, "undo")
			}
			wantLink(t, ck, "link", "elsewhere.txt")
			wantFile(t, ck, "plain.txt", "plain\n")
			wantFile(t, ck, "elsewhere.txt", "elsewhere\n")
			if out := strings.TrimSpace(runFossilIn(t, bin, ck, "changes")); out != "" {
				t.Errorf("fossil changes after undo = %q, want nothing", out)
			}
			runFossilIn(t, bin, ck, "close", "--force")
		})
	}
}
