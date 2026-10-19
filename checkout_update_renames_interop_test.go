package libfossil_test

// Interop tests for Checkout.Update across renames (#445): the same local
// changes, updated by fossil 2.28 and by go-libfossil, must leave the same
// files and the same change status.

import (
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// renamesInteropRepo commits, with fossil, A and two children of it:
//
//	A: f.txt, g.txt, h.txt, k.txt
//	B: f.txt renamed f2.txt and edited; g.txt renamed g2.txt unchanged;
//	   k.txt edited
//	C: h.txt renamed h2.txt and edited
//
// It returns the binary, the repository and the hashes of A, B and C.
func renamesInteropRepo(t *testing.T) (bin, repoPath string, v map[string]string) {
	t.Helper()
	bin, repoPath, ck := newFossilCheckout(t)
	write := func(name, body string) { writeFile(t, filepath.Join(ck, name), body) }
	write("f.txt", "a\nb\nc\n")
	write("g.txt", "x\ny\nz\n")
	write("h.txt", "1\n2\n3\n")
	write("k.txt", "k1\nk2\nk3\n")
	fossilRun(t, bin, ck, "add", "f.txt", "g.txt", "h.txt", "k.txt")
	fossilRun(t, bin, ck, "commit", "--hash", "-m", "A")
	fossilRun(t, bin, ck, "mv", "--hard", "f.txt", "f2.txt")
	write("f2.txt", "A\nb\nc\n")
	fossilRun(t, bin, ck, "mv", "--hard", "g.txt", "g2.txt")
	write("k.txt", "K1\nk2\nk3\n")
	fossilRun(t, bin, ck, "commit", "--hash", "-m", "B")
	v = map[string]string{}
	v["A"] = versionHash(t, bin, ck, repoPath, "A")
	v["B"] = versionHash(t, bin, ck, repoPath, "B")
	fossilRun(t, bin, ck, "update", v["A"])
	fossilRun(t, bin, ck, "mv", "--hard", "h.txt", "h2.txt")
	write("h2.txt", "one\n2\n3\n")
	fossilRun(t, bin, ck, "commit", "--hash", "--allow-fork", "-m", "C")
	fossilRun(t, bin, ck, "close")
	v["C"] = versionHash(t, bin, ck, repoPath, "C")
	return bin, repoPath, v
}

func versionHash(t *testing.T, bin, dir, repoPath, comment string) string {
	t.Helper()
	out := fossilRun(t, bin, dir, "sql", "-R", repoPath,
		"SELECT uuid FROM blob JOIN event ON objid=rid WHERE comment='"+comment+"'")
	return strings.Trim(strings.TrimSpace(out), "'")
}

// From A to B: a local edit to f.txt (renamed and edited by B) is merged
// into f2.txt; a local edit to g.txt (only renamed by B) stays put; a local
// rename of k.txt (edited by B) is dropped, as fossil drops it.
func TestUpdateFollowsTargetRenamesLikeFossil(t *testing.T) {
	bin, repoPath, v := renamesInteropRepo(t)
	edit := func(dir string) {
		for name, body := range map[string]string{"f.txt": "a\nb\nC\n", "g.txt": "x\ny\nZ\n"} {
			writeFile(t, filepath.Join(dir, name), body)
			setMTime(t, filepath.Join(dir, name), agedTime)
		}
	}

	fossilDir := t.TempDir()
	fossilRun(t, bin, fossilDir, "open", repoPath, v["A"])
	edit(fossilDir)
	fossilRun(t, bin, fossilDir, "mv", "--hard", "k.txt", "k3.txt")
	fossilRun(t, bin, fossilDir, "update", v["B"])

	ck, goDir, ridOf := goCheckoutAt(t, bin, repoPath, v["A"])
	edit(goDir)
	if err := ck.Rename("k.txt", "k3.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := ck.Update(libfossil.UpdateOpts{TargetRID: ridOf(v["B"])}); err != nil {
		t.Fatal(err)
	}
	assertSameAfterUpdate(t, bin, repoPath, fossilDir, goDir, ck)
}

// From C to its sibling B: the path runs up through A, so C's rename of
// h.txt to h2.txt is followed backwards and a local edit to h2.txt is
// merged into h.txt.
func TestUpdateFollowsRenamesBackLikeFossil(t *testing.T) {
	bin, repoPath, v := renamesInteropRepo(t)
	edit := func(dir string) {
		writeFile(t, filepath.Join(dir, "h2.txt"), "one\n2\nthree\n")
		setMTime(t, filepath.Join(dir, "h2.txt"), agedTime)
	}

	fossilDir := t.TempDir()
	fossilRun(t, bin, fossilDir, "open", repoPath, v["C"])
	edit(fossilDir)
	fossilRun(t, bin, fossilDir, "update", v["B"])

	ck, goDir, ridOf := goCheckoutAt(t, bin, repoPath, v["C"])
	edit(goDir)
	if _, err := ck.Update(libfossil.UpdateOpts{TargetRID: ridOf(v["B"])}); err != nil {
		t.Fatal(err)
	}
	assertSameAfterUpdate(t, bin, repoPath, fossilDir, goDir, ck)
}
