package libfossil_test

// Interop test for Checkout.Update with uncommitted changes: the same edits
// made in a fossil checkout and in a go-libfossil checkout, each updated by
// its own tool, must leave the same files and the same change status (#443).

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// updateEditsRepo commits A and its child B with fossil and returns the
// binary, the repository and the two versions' hashes. Commits use --hash:
// the edits land within a second of the last scan, keeping their sizes.
func updateEditsRepo(t *testing.T) (bin, repoPath, a, b string) {
	t.Helper()
	bin, repoPath, ck := newFossilCheckout(t)
	write := func(name, body string) { writeFile(t, filepath.Join(ck, name), body) }
	write("merge.txt", "a\nb\nc\n")
	write("clash.txt", "x\n")
	write("gone.txt", "gone\n")
	write("plain.txt", "plain\n")
	write("same.txt", "same\n")
	fossilRun(t, bin, ck, "add", "merge.txt", "clash.txt", "gone.txt", "plain.txt", "same.txt")
	fossilRun(t, bin, ck, "commit", "--hash", "-m", "A")
	write("merge.txt", "A\nb\nc\n")
	write("clash.txt", "y\n")
	write("plain.txt", "plain, B\n")
	write("added.txt", "added by B\n")
	fossilRun(t, bin, ck, "add", "added.txt")
	fossilRun(t, bin, ck, "rm", "gone.txt")
	fossilRun(t, bin, ck, "commit", "--hash", "-m", "B")
	fossilRun(t, bin, ck, "close")
	a = strings.TrimSpace(fossilRun(t, bin, ck, "sql", "-R", repoPath,
		"SELECT uuid FROM blob JOIN event ON objid=rid WHERE comment='A'"))
	b = strings.TrimSpace(fossilRun(t, bin, ck, "sql", "-R", repoPath,
		"SELECT uuid FROM blob JOIN event ON objid=rid WHERE comment='B'"))
	return bin, repoPath, strings.Trim(a, "'"), strings.Trim(b, "'")
}

// localEdits makes the uncommitted changes both checkouts get on top of A:
// edits merge.txt where B did not, clashes with B in clash.txt, edits
// gone.txt which B deletes, and writes new.txt for adding. The edits keep
// their sizes, so each is dated agedTime: fossil would not see an edit dated
// the second it recorded when it wrote the file.
func localEdits(t *testing.T, dir string) {
	t.Helper()
	for name, body := range map[string]string{
		"merge.txt": "a\nb\nC\n", "clash.txt": "z\n",
		"gone.txt": "gone, edited\n", "new.txt": "new\n",
	} {
		writeFile(t, filepath.Join(dir, name), body)
		setMTime(t, filepath.Join(dir, name), agedTime)
	}
}

// treeFiles reads every file under dir but the checkout database and its
// journal files.
func treeFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, ".fslckout") || rel == "_FOSSIL_" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// goCheckoutAt opens a go-libfossil checkout of the version with hash
// uuid in a fresh directory, returning it, its directory and a resolver
// from hash to rid.
func goCheckoutAt(t *testing.T, bin, repoPath, uuid string) (*libfossil.Checkout, string, func(string) int64) {
	t.Helper()
	goDir := filepath.Join(t.TempDir(), "ck")
	if err := os.Mkdir(goDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ck, err := r.CreateCheckout(goDir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ck.Close() })
	ridOf := func(uuid string) int64 {
		out := fossilRun(t, bin, goDir, "sql", "-R", repoPath,
			"SELECT rid FROM blob WHERE uuid='"+uuid+"'")
		rid, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil {
			t.Fatalf("rid of %s: %q", uuid, out)
		}
		return rid
	}
	if err := ck.Extract(ridOf(uuid), libfossil.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	return ck, goDir, ridOf
}

// assertSameAfterUpdate compares the two updated checkouts: the files on
// disk, fossil's changes in each, and Status in each.
func assertSameAfterUpdate(
	t *testing.T, bin, repoPath, fossilDir, goDir string, ck *libfossil.Checkout,
) {
	t.Helper()
	want, got := treeFiles(t, fossilDir), treeFiles(t, goDir)
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s: go-libfossil left %q, fossil %q", name, got[name], body)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: go-libfossil left a file fossil does not", name)
		}
	}
	wantChanges := fossilRun(t, bin, fossilDir, "changes")
	if gotChanges := fossilRun(t, bin, goDir, "changes"); gotChanges != wantChanges {
		t.Errorf("fossil changes in the go-libfossil checkout:\n%s\nwant, as in fossil's:\n%s",
			gotChanges, wantChanges)
	}
	fossilCk := openLibfossilCheckout(t, repoPath, fossilDir)
	if g, w := strings.Join(statusLines(t, ck), ","), strings.Join(statusLines(t, fossilCk), ","); g != w {
		t.Errorf("Status = %q, in fossil's checkout %q", g, w)
	}
}

func TestUpdateWithLocalEditsMatchesFossil(t *testing.T) {
	bin, repoPath, a, b := updateEditsRepo(t)

	fossilDir := t.TempDir()
	fossilRun(t, bin, fossilDir, "open", repoPath, a)
	localEdits(t, fossilDir)
	fossilRun(t, bin, fossilDir, "add", "new.txt")
	fossilRun(t, bin, fossilDir, "rm", "same.txt")
	fossilRun(t, bin, fossilDir, "update", b)

	ck, goDir, ridOf := goCheckoutAt(t, bin, repoPath, a)
	localEdits(t, goDir)
	if _, err := ck.Add([]string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := ck.Remove([]string{"same.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err := ck.Update(libfossil.UpdateOpts{TargetRID: ridOf(b)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Conflicted, ",") != "clash.txt" {
		t.Errorf("Conflicted = %v, want [clash.txt]", res.Conflicted)
	}

	// The conflicted file holds markers in both. Their text differs: fossil
	// 2.28 adds line numbers and a suggested resolution, which
	// internal/merge does not write, so only the markers are compared.
	for _, dir := range []string{fossilDir, goDir} {
		path := filepath.Join(dir, "clash.txt")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), "<<<<<<< BEGIN MERGE CONFLICT") {
			t.Errorf("%s = %q, want conflict markers", path, data)
		}
		writeFile(t, path, "resolved\n")
	}
	assertSameAfterUpdate(t, bin, repoPath, fossilDir, goDir, ck)
}

// A file missing from disk is restored, and a file the target adds over an
// untracked one is written after the untracked one is copied to
// NAME-original.
func TestUpdateRestoresAndBacksUpLikeFossil(t *testing.T) {
	bin, repoPath, a, b := updateEditsRepo(t)
	prepare := func(dir string) {
		if err := os.Remove(filepath.Join(dir, "same.txt")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "added.txt"), "mine\n")
	}

	fossilDir := t.TempDir()
	fossilRun(t, bin, fossilDir, "open", repoPath, a)
	prepare(fossilDir)
	fossilRun(t, bin, fossilDir, "update", b)

	ck, goDir, ridOf := goCheckoutAt(t, bin, repoPath, a)
	prepare(goDir)
	if _, err := ck.Update(libfossil.UpdateOpts{TargetRID: ridOf(b)}); err != nil {
		t.Fatal(err)
	}
	assertSameAfterUpdate(t, bin, repoPath, fossilDir, goDir, ck)
}
