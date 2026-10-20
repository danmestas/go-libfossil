package libfossil_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/testutil"
)

// A sparse checkout stays a checkout fossil can open safely
// (draft-fossil-sparse-cone-00 §3.1): the cone lives in a vvar fossil
// ignores, and vfile keeps a row for every file. fossil therefore sees the
// uncovered files as MISSING and refuses to commit -- it cannot commit them
// away -- and its revert brings them back, after which go-libfossil still
// works within the cone and ignores the restored files.
func TestSparseCheckoutUnderFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	_, repoPath, _ := newFossilCheckout(t)
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tip, _, err := r.Commit(libfossil.CommitOpts{Comment: "tree", User: "u", Files: []libfossil.FileToCommit{
		{Name: "a.txt", Content: []byte("one\n")}, {Name: "b.txt", Content: []byte("two\n")},
		{Name: "src/s.txt", Content: []byte("src\n")}, {Name: "tools/t.txt", Content: []byte("tools\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cone, err := libfossil.NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "sparse")
	co, err := r.CreateCheckout(dir, libfossil.CheckoutCreateOpts{Cone: cone})
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(tip, libfossil.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	co.Close()
	writeFile(t, filepath.Join(dir, "src/s.txt"), "edited by fossil\n")
	leaves := strings.TrimSpace(fossilRun(t, bin, dir, "leaves"))

	status := fossilRun(t, bin, dir, "status")
	for _, want := range []string{"MISSING    a.txt", "MISSING    tools/t.txt", "EDITED     src/s.txt"} {
		if !strings.Contains(status, want) {
			t.Fatalf("fossil status does not show %q:\n%s", want, status)
		}
	}
	cmd := exec.Command(bin, "commit", "-m", "from fossil", "--no-warnings")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("fossil committed a sparse checkout:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "no such file") {
		t.Fatalf("fossil refused the commit, but not for the missing files:\n%s", out)
	}
	if got := strings.TrimSpace(fossilRun(t, bin, dir, "leaves")); got != leaves {
		t.Fatalf("a refused fossil commit changed the leaves:\n%s\nthen\n%s", leaves, got)
	}

	// fossil's revert densifies the tree; go-libfossil keeps to its cone.
	fossilRun(t, bin, dir, "revert", "a.txt", "b.txt", "tools/t.txt")
	if _, err := os.Stat(filepath.Join(dir, "tools/t.txt")); err != nil {
		t.Fatalf("fossil revert did not restore an uncovered file: %v", err)
	}
	co, err = r.OpenCheckout(dir, libfossil.CheckoutOpenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	changes, err := co.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Name != "src/s.txt" || changes[0].Change != "modified" {
		t.Fatalf("go-libfossil status after fossil: %+v", changes)
	}
	rid, _, err := co.Checkin(libfossil.CheckoutCommitOpts{Message: "sparse", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := r.ListFiles(rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("the sparse commit lists %d files, want 4", len(files))
	}
}

// Renames across a sparse checkout's cone, with fossil's dense update as the
// oracle: after go-libfossil updates a sparse checkout, the files its cone
// covers are the ones fossil's update leaves, and none other is on disk.
// fossil's mv records each rename: one outside the cone, one into it, and an
// unedited one out of it.
func TestSparseUpdateFollowsRenamesLikeFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	_, repoPath, ck := newFossilCheckout(t)
	for name, body := range map[string]string{
		"src/s.txt": "s\n", "src/out.txt": "out\n", "tools/t.txt": "t\n", "docs/d.txt": "d\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ck, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(ck, name), body)
	}
	fossilRun(t, bin, ck, "add", "src", "tools", "docs")
	fossilRun(t, bin, ck, "commit", "--hash", "--no-warnings", "-m", "A")
	fossilRun(t, bin, ck, "mv", "--hard", "tools/t.txt", "tools/t2.txt")
	fossilRun(t, bin, ck, "mv", "--hard", "docs/d.txt", "src/d.txt")
	fossilRun(t, bin, ck, "mv", "--hard", "src/out.txt", "tools/out.txt")
	fossilRun(t, bin, ck, "commit", "--hash", "--no-warnings", "-m", "B")
	fossilRun(t, bin, ck, "close")
	hashOf := func(comment string) string {
		out := fossilRun(t, bin, ck, "sql", "-R", repoPath,
			"SELECT uuid FROM blob JOIN event ON objid=rid WHERE comment='"+comment+"'")
		return strings.Trim(strings.TrimSpace(out), "'")
	}
	a, b := hashOf("A"), hashOf("B")

	fossilDir := t.TempDir()
	fossilRun(t, bin, fossilDir, "open", repoPath, a)
	fossilRun(t, bin, fossilDir, "update", b)

	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ridOf := func(uuid string) int64 {
		rid, err := r.ResolveVersion(uuid)
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	cone, err := libfossil.NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	goDir := filepath.Join(t.TempDir(), "sparse")
	co, err := r.CreateCheckout(goDir, libfossil.CheckoutCreateOpts{Cone: cone})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if err := co.Extract(ridOf(a), libfossil.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(libfossil.UpdateOpts{TargetRID: ridOf(b)}); err != nil {
		t.Fatalf("Update across renames: %v", err)
	}
	want := map[string]string{}
	for name, body := range treeFiles(t, fossilDir) {
		if cone.Covers(filepath.ToSlash(name)) {
			want[name] = body
		}
	}
	got := treeFiles(t, goDir)
	if len(got) != len(want) {
		t.Fatalf("sparse checkout holds %v, fossil's covered files are %v", got, want)
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s = %q, fossil %q", name, got[name], body)
		}
	}
}
