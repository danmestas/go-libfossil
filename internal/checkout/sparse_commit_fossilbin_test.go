package checkout

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

// partialCommitHistory has fossil commit c1 (src/a.c, src/b.c, tools/t.c),
// then two children of it: remote edits tools/t.c, target edits src/a.c.
func partialCommitHistory(t *testing.T, bin string) (repoPath string, v map[string]string) {
	t.Helper()
	dir := t.TempDir()
	repoPath = filepath.Join(dir, "r.fossil")
	ck := filepath.Join(dir, "ck")
	runFossilIn(t, bin, dir, "init", repoPath)
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, ck, "open", repoPath)
	write := func(name, body string) {
		t.Helper()
		full := filepath.Join(ck, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/a.c", "a\n")
	write("src/b.c", "b\n")
	write("tools/t.c", "tools\n")
	runFossilIn(t, bin, ck, "add", "src", "tools")
	runFossilIn(t, bin, ck, "commit", "-m", "c1", "--date-override", "2030-01-02 03:04:05")
	write("tools/t.c", "merged\n")
	runFossilIn(t, bin, ck, "commit", "-m", "remote", "--branch", "r", "--date-override", "2030-01-02 03:05:05")
	runFossilIn(t, bin, ck, "update", "trunk")
	write("src/a.c", "target\n")
	runFossilIn(t, bin, ck, "commit", "-m", "target", "--date-override", "2030-01-02 03:06:05")
	runFossilIn(t, bin, ck, "close")
	v = map[string]string{}
	for _, name := range []string{"c1", "remote", "target"} {
		v[name] = strings.TrimSpace(runFossilIn(t, bin, dir, "sql", "-R", repoPath,
			"SELECT uuid FROM blob JOIN event ON rid=objid WHERE comment='"+name+"'"))
		v[name] = strings.Trim(v[name], "'")
	}
	return repoPath, v
}

// fossilFails runs fossil, which must fail, and returns its output.
func fossilFails(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("fossil %s succeeded; want a refusal\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

// checkinFiles lists the check-in's files as name to hash and permission.
func checkinFiles(t *testing.T, r *repo.Repo, rid libfossil.FslID) map[string]string {
	t.Helper()
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Name] = f.UUID + " " + f.Perm
	}
	return out
}

func sameFiles(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d files, fossil records %d: %v vs %v", what, len(got), len(want), got, want)
	}
	for name, w := range want {
		if got[name] != w {
			t.Fatalf("%s: %s is %q, fossil records %q", what, name, got[name], w)
		}
	}
}

// TestPendingMergeAcrossUpdateMatchesFossil merges remote, updates to
// target and tries a partial commit, with fossil, a dense checkout and a
// sparse one covering src. Each keeps the merge pending across the update
// and refuses the partial commit; the full commit records remote as a merge
// parent.
func TestPendingMergeAcrossUpdateMatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	repoPath, v := partialCommitHistory(t, bin)
	ck := t.TempDir()
	runFossilIn(t, bin, ck, "open", repoPath, v["c1"])
	runFossilIn(t, bin, ck, "merge", v["remote"])
	runFossilIn(t, bin, ck, "update", v["target"])
	fossilMerges := strings.TrimSpace(runFossilIn(t, bin, ck, "sql", "SELECT count(*) FROM localdb.vmerge"))
	if err := os.WriteFile(filepath.Join(ck, "src", "b.c"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := fossilFails(t, bin, ck, "commit", "-m", "partial", "src/b.c"); !strings.Contains(out, "cannot do a partial commit of a merge") {
		t.Fatalf("fossil's partial commit: %s", out)
	}
	runFossilIn(t, bin, ck, "close", "--force")

	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []*cone.Cone{nil, src} {
		co, err := Create(r, t.TempDir(), CreateOpts{Cone: c})
		if err != nil {
			t.Fatal(err)
		}
		defer co.Close()
		if err := co.Extract(ridOf(t, r, v["c1"]), ExtractOpts{Force: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := co.Merge(MergeOpts{Version: ridOf(t, r, v["remote"])}); err != nil {
			t.Fatal(err)
		}
		if _, err := co.Update(UpdateOpts{TargetRID: ridOf(t, r, v["target"])}); err != nil {
			t.Fatal(err)
		}
		var merges string
		if err := co.db.QueryRow("SELECT CAST(count(*) AS TEXT) FROM vmerge").Scan(&merges); err != nil {
			t.Fatal(err)
		}
		if merges != fossilMerges {
			t.Fatalf("sparse=%v: %s vmerge rows after update, fossil keeps %s", c != nil, merges, fossilMerges)
		}
		writeFile(t, co, "src/b.c", "edited\n")
		if err := co.Enqueue(EnqueueOpts{Paths: []string{"src/b.c"}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := co.Commit(CommitOpts{Message: "partial", User: "test"}); !errors.Is(err, errPartialMergeCommit) {
			t.Fatalf("sparse=%v: partial commit of a merge = %v", c != nil, err)
		}
		if err := co.DiscardQueue(); err != nil {
			t.Fatal(err)
		}
		rid, _, err := co.Commit(CommitOpts{Message: "full", User: "test", Time: time.Date(2030, 1, 3, 0, i, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		d, err := manifest.GetManifest(r, rid)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.P) != 2 || d.P[0] != v["target"] || d.P[1] != v["remote"] {
			t.Fatalf("sparse=%v: P card %q, want target and the merge parent remote", c != nil, d.P)
		}
	}
}

// TestPartialCommitLeavesChangePendingMatchesFossil commits src/b.c alone
// while tools/t.c is changed too: fossil and a dense checkout with tools/t.c
// edited on disk, and a sparse checkout covering src with the same change
// recorded outside the cone. Each partial commit records only src/b.c, and
// the next full commit records tools/t.c.
func TestPartialCommitLeavesChangePendingMatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	repoPath, v := partialCommitHistory(t, bin)
	ck := t.TempDir()
	runFossilIn(t, bin, ck, "open", repoPath, v["c1"])
	for name, body := range map[string]string{"src/b.c": "edited\n", "tools/t.c": "merged\n"} {
		if err := os.WriteFile(filepath.Join(ck, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runFossilIn(t, bin, ck, "commit", "-m", "fossil partial", "--allow-fork", "--date-override", "2030-01-02 04:00:00", "src/b.c")
	if out := runFossilIn(t, bin, ck, "changes"); !strings.Contains(out, "tools/t.c") {
		t.Fatalf("fossil's partial commit took tools/t.c: %q", out)
	}
	runFossilIn(t, bin, ck, "commit", "-m", "fossil full", "--date-override", "2030-01-02 05:00:00")
	runFossilIn(t, bin, ck, "close")

	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	byComment := func(comment string) libfossil.FslID {
		var rid int64
		if err := r.DB().QueryRow("SELECT objid FROM event WHERE comment=?", comment).Scan(&rid); err != nil {
			t.Fatal(err)
		}
		return libfossil.FslID(rid)
	}
	want := map[string]map[string]string{
		"partial": checkinFiles(t, r, byComment("fossil partial")),
		"full":    checkinFiles(t, r, byComment("fossil full")),
	}
	src, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []*cone.Cone{nil, src} {
		co, err := Create(r, t.TempDir(), CreateOpts{Cone: c})
		if err != nil {
			t.Fatal(err)
		}
		defer co.Close()
		if err := co.Extract(ridOf(t, r, v["c1"]), ExtractOpts{Force: true}); err != nil {
			t.Fatal(err)
		}
		if c == nil {
			writeFile(t, co, "tools/t.c", "merged\n")
		} else {
			// The outcome an earlier sparse update left without its merge.
			if _, err := co.Merge(MergeOpts{Version: ridOf(t, r, v["remote"])}); err != nil {
				t.Fatal(err)
			}
			if _, err := co.db.Exec("DELETE FROM vmerge"); err != nil {
				t.Fatal(err)
			}
		}
		writeFile(t, co, "src/b.c", "edited\n")
		if err := co.Enqueue(EnqueueOpts{Paths: []string{"src/b.c"}}); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2030, 1, 3, 0, i, 0, 0, time.UTC)
		rid, _, err := co.Commit(CommitOpts{Message: "partial", User: "test", Time: at})
		if err != nil {
			t.Fatal(err)
		}
		sameFiles(t, "partial", checkinFiles(t, r, rid), want["partial"])
		if rid, _, err = co.Commit(CommitOpts{Message: "full", User: "test", Time: at.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		sameFiles(t, "full", checkinFiles(t, r, rid), want["full"])
	}
}
