//go:build unix

package checkout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// execMergeRepo commits with fossil, in ck, files under base, then on branch
// br sets the executable bit of each of exec, and on trunk edits trunk.txt,
// so merging br into trunk changes only those bits.
func execMergeRepo(t *testing.T, bin, dir string, files []string, exec []string) string {
	t.Helper()
	repoPath := filepath.Join(dir, "r.fossil")
	ck := filepath.Join(dir, "build")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, dir, "init", repoPath)
	runFossilIn(t, bin, ck, "open", repoPath)
	for _, name := range append(files, "trunk.txt") {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ck, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ck, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runFossilIn(t, bin, ck, append([]string{"add"}, append(files, "trunk.txt")...)...)
	runFossilIn(t, bin, ck, "commit", "-m", "base", "--no-warnings")
	runFossilIn(t, bin, ck, "branch", "new", "br", "trunk")
	runFossilIn(t, bin, ck, "update", "br")
	for _, name := range exec {
		if err := os.Chmod(filepath.Join(ck, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runFossilIn(t, bin, ck, "commit", "-m", "exec on br", "--no-warnings")
	runFossilIn(t, bin, ck, "update", "trunk")
	if err := os.WriteFile(filepath.Join(ck, "trunk.txt"), []byte("trunk edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, ck, "commit", "-m", "on trunk", "--no-warnings")
	runFossilIn(t, bin, ck, "close")
	return repoPath
}

// execState renders name's mode on disk and the checkout's vfile and vmerge.
func execState(t *testing.T, ck, name string) string {
	t.Helper()
	var b strings.Builder
	info, err := os.Stat(filepath.Join(ck, name))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "%s %v\n", name, info.Mode().Perm())
	db, err := libdb.OpenSQL(filepath.Join(ck, ".fslckout"), libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT pathname, rid=mrid, chnged, CAST(isexe AS INT) FROM vfile ORDER BY pathname")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var same, chnged, isexe int64
		if err := rows.Scan(&path, &same, &chnged, &isexe); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s|%d|%d|%d\n", path, same, chnged, isexe)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var merges int
	if err := db.QueryRow("SELECT count(*) FROM vmerge").Scan(&merges); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "vmerge %d\n", merges)
	return b.String()
}

// Fossil's revert writes a file with the reverted-to version's permission and
// records it in vfile.isexe, so reverting a file whose only change is the
// executable bit a pending merge set takes the bit back off. Reverting
// everything selects only changed rows, which such a file is not, so fossil
// leaves its bit and drops the merge.
func TestRevertMergedExecBitMatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	for _, paths := range [][]string{{"a.sh"}, nil} {
		t.Run(fmt.Sprintf("paths=%q", paths), func(t *testing.T) {
			dir := t.TempDir()
			repoPath := execMergeRepo(t, bin, dir, []string{"a.sh"}, []string{"a.sh"})
			ckF, ckG := filepath.Join(dir, "ckf"), filepath.Join(dir, "ckg")
			for _, ck := range []string{ckF, ckG} {
				if err := os.Mkdir(ck, 0o755); err != nil {
					t.Fatal(err)
				}
				runFossilIn(t, bin, ck, "open", repoPath)
				runFossilIn(t, bin, ck, "merge", "br")
			}
			if got := execState(t, ckG, "a.sh"); !strings.Contains(got, "a.sh -rwxr-xr-x\na.sh|1|0|1\n") {
				t.Fatalf("merge did not set a.sh's bit alone:\n%s", got)
			}
			runFossilIn(t, bin, ckF, append([]string{"revert"}, paths...)...)
			r, err := repo.Open(repoPath)
			if err != nil {
				t.Fatal(err)
			}
			co, err := Open(r, ckG, OpenOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if err := co.Revert(RevertOpts{Paths: paths}); err != nil {
				t.Fatal(err)
			}
			co.Close()
			r.Close()
			fossil, golib := execState(t, ckF, "a.sh"), execState(t, ckG, "a.sh")
			if fossil != golib {
				t.Fatalf("after revert\nfossil:\n%s\ngo-libfossil:\n%s", fossil, golib)
			}
		})
	}
}

// In a sparse checkout the same merge sets the bit of a file inside the cone
// in vfile and records it for a file outside (sparse_pending). Reverting
// the file inside takes the bit back off, as fossil's revert does; reverting
// the one outside drops its record and writes nothing.
func TestRevertMergedExecBitSparse(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	files := []string{"src/x.sh", "tools/t.sh"}
	repoPath := execMergeRepo(t, bin, dir, files, files)
	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var trunk, br libfossil.FslID
	for name, rid := range map[string]*libfossil.FslID{"trunk": &trunk, "br": &br} {
		if err := r.DB().QueryRow(
			`SELECT rid FROM tagxref JOIN tag USING(tagid)
			  WHERE tagname='sym-'||? AND tagtype>0 ORDER BY mtime DESC LIMIT 1`, name,
		).Scan(rid); err != nil {
			t.Fatal(err)
		}
	}
	src, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ck := filepath.Join(dir, "ck")
	env := &simio.Env{
		Clock:   simio.NewSimClockAt(time.Now().Add(time.Hour)),
		Rand:    simio.CryptoRand{},
		Storage: simio.OSStorage{},
	}
	co, err := Create(r, ck, CreateOpts{Env: env, Cone: src})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if err := co.Extract(trunk, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: br}); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := co.db.QueryRow(
		"SELECT count(*) FROM sparse_pending WHERE pathname='tools/t.sh' AND isexe",
	).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("merge recorded %d out-of-cone bits, %v", pending, err)
	}
	if err := co.Revert(RevertOpts{Paths: files}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(ck, "src/x.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("src/x.sh after revert: mode %v, want -rw-r--r--", got)
	}
	var state string
	if err := co.db.QueryRow(
		`SELECT group_concat(pathname||'|'||chnged||'|'||CAST(isexe AS INT), ' ') FROM
		   (SELECT pathname, chnged, isexe FROM vfile ORDER BY pathname)`,
	).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if want := "src/x.sh|0|0"; state != want {
		t.Fatalf("vfile after revert = %q, want %q", state, want)
	}
	if err := co.db.QueryRow("SELECT count(*) FROM sparse_pending").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("revert left %d out-of-cone records, %v", pending, err)
	}
	if _, err := os.Lstat(filepath.Join(ck, "tools")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revert wrote outside the cone: %v", err)
	}
}
