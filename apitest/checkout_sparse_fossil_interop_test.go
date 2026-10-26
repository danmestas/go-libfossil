package apitest_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/testutil"
)

// Sparse metadata has its own marker: stock Fossil must refuse checkout
// operations rather than interpret a cone-only vfile as a complete tree.
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
	leaves := strings.TrimSpace(fossilRun(t, bin, dir, "leaves", "-R", repoPath))
	for _, args := range [][]string{
		{"status"},
		{"commit", "-m", "from fossil", "--no-warnings"},
		{"revert", "a.txt", "b.txt", "tools/t.txt"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(out), "within an open check-out") {
			t.Fatalf("fossil %s did not refuse private sparse metadata: %v: %s", args[0], err, out)
		}
	}
	if got := strings.TrimSpace(fossilRun(t, bin, dir, "leaves", "-R", repoPath)); got != leaves {
		t.Fatalf("refused fossil operations changed the leaves:\n%s\nthen\n%s", leaves, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "tools/t.txt")); !os.IsNotExist(err) {
		t.Fatalf("refused fossil revert materialized an uncovered file: %v", err)
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
