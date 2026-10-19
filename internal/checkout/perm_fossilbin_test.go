//go:build unix

package checkout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

// fossilLabel is the label fossil's changes command prints for each change.
var fossilLabel = map[FileChange]string{
	ChangeAdded: "ADDED", ChangeRemoved: "DELETED", ChangeMissing: "MISSING",
	ChangeRenamed: "RENAMED", ChangeModified: "EDITED",
	ChangeExecutable: "EXECUTABLE", ChangeSymlink: "SYMLINK",
	ChangeUnexec: "UNEXEC", ChangeUnlink: "UNLINK",
}

// permHistory commits, with fossil and allow-symlinks set to allow, a plain
// file, an executable, a symlink and two more plain files, and returns the
// repository.
func permHistory(t *testing.T, bin, allow string) string {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "r.fossil")
	ck := filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, dir, "init", repoPath)
	runFossilIn(t, bin, ck, "open", repoPath)
	runFossilIn(t, bin, ck, "settings", "allow-symlinks", "on")
	for name, body := range map[string]string{
		"plain.txt": "plain\n", "run.sh": "#!/bin/sh\n", "gone.txt": "gone\n", "tolink.txt": "t\n",
	} {
		if err := os.WriteFile(filepath.Join(ck, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(ck, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("plain.txt", filepath.Join(ck, "link")); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, ck, "add", ".")
	runFossilIn(t, bin, ck, "commit", "-m", "v1")
	runFossilIn(t, bin, ck, "settings", "allow-symlinks", allow)
	runFossilIn(t, bin, ck, "close", "--force")
	return repoPath
}

// permEdits makes every kind of change the scan's permission half sees, and
// a missing file, in the checkout at ck.
func permEdits(t *testing.T, ck string) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Chmod(filepath.Join(ck, "plain.txt"), 0o755))
	must(os.Chmod(filepath.Join(ck, "run.sh"), 0o644))
	must(os.Remove(filepath.Join(ck, "gone.txt")))
	must(os.Remove(filepath.Join(ck, "tolink.txt")))
	must(os.Symlink("run.sh", filepath.Join(ck, "tolink.txt")))
	must(os.Remove(filepath.Join(ck, "link")))
	must(os.WriteFile(filepath.Join(ck, "link"), []byte("plain.txt"), 0o644))
}

// fossilChanges parses `fossil changes` into name → label.
func fossilChanges(t *testing.T, bin, ck string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(runFossilIn(t, bin, ck, "changes"), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			out[f[1]] = f[0]
		}
	}
	return out
}

// goChanges opens the fossil checkout at ck and reports its status as
// fossil's labels.
func goChanges(t *testing.T, repoPath, ck string) map[string]string {
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
	out := map[string]string{}
	for name, change := range statusOf(t, co) {
		out[name] = fossilLabel[change]
	}
	return out
}

func sameLabels(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %v, fossil %v", what, got, want)
		return
	}
	for name, label := range want {
		if got[name] != label {
			t.Errorf("%s: %s %q, fossil %q", what, name, got[name], label)
		}
	}
}

// The same edits on the same checkout are reported identically by fossil's
// changes command and go-libfossil's status, whichever scans first, with
// allow-symlinks on and off.
func TestModeChangesMatchFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	for _, allow := range []string{"on", "off"} {
		repoPath := permHistory(t, bin, allow)

		fossilFirst := t.TempDir()
		runFossilIn(t, bin, fossilFirst, "open", repoPath)
		permEdits(t, fossilFirst)
		want := fossilChanges(t, bin, fossilFirst)
		sameLabels(t, "allow-symlinks "+allow+", go after fossil",
			goChanges(t, repoPath, fossilFirst), want)

		goFirst := t.TempDir()
		runFossilIn(t, bin, goFirst, "open", repoPath)
		permEdits(t, goFirst)
		got := goChanges(t, repoPath, goFirst)
		sameLabels(t, "allow-symlinks "+allow+", go before fossil", got, want)
		sameLabels(t, "allow-symlinks "+allow+", fossil after go",
			fossilChanges(t, bin, goFirst), want)
		if len(want) < 5 {
			t.Errorf("allow-symlinks %s: fossil reports %v; the test's premise is wrong", allow, want)
		}
	}
}

// fCards returns the F cards of the check-in uuid, by file name.
func fCards(t *testing.T, bin, repoPath, uuid string) map[string]string {
	t.Helper()
	out := map[string]string{}
	art := runFossilIn(t, bin, filepath.Dir(repoPath), "artifact", "-R", repoPath, uuid)
	for _, line := range strings.Split(art, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == "F" {
			out[f[1]] = line
		}
	}
	return out
}

// A commit carrying exec and symlink changes records the same F cards
// whether fossil or go-libfossil makes it.
func TestModeCommitMatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	repoPath := permHistory(t, bin, "on")
	goRepo := filepath.Join(t.TempDir(), "g.fossil")
	data, err := os.ReadFile(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goRepo, data, 0o644); err != nil {
		t.Fatal(err)
	}

	edit := func(ck string) {
		permEdits(t, ck)
		if err := os.WriteFile(filepath.Join(ck, "gone.txt"), []byte("gone\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fossilCk := t.TempDir()
	runFossilIn(t, bin, fossilCk, "open", repoPath)
	edit(fossilCk)
	runFossilIn(t, bin, fossilCk, "commit", "-m", "modes")
	tip := strings.Fields(runFossilIn(t, bin, fossilCk, "sql",
		"SELECT uuid FROM blob WHERE rid=(SELECT value FROM vvar WHERE name='checkout')"))
	want := fCards(t, bin, repoPath, tip[0])

	goCk := t.TempDir()
	runFossilIn(t, bin, goCk, "open", goRepo)
	edit(goCk)
	r, err := repo.Open(goRepo)
	if err != nil {
		t.Fatal(err)
	}
	co, err := Open(r, goCk, OpenOpts{})
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	_, uuid, err := co.Commit(CommitOpts{Message: "modes", User: "test"})
	co.Close()
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := fCards(t, bin, goRepo, uuid)
	if len(got) != len(want) {
		t.Fatalf("F cards %v, fossil %v", got, want)
	}
	for name, card := range want {
		if got[name] != card {
			t.Errorf("%s: %q, fossil %q", name, got[name], card)
		}
	}
	if out := fossilChanges(t, bin, goCk); len(out) != 0 {
		t.Errorf("fossil changes after go commit: %v, want nothing", out)
	}
}

// A fresh checkout of a symlink entry, written without Force, looks the same
// to go-libfossil as fossil's open does to fossil: clean with allow-symlinks
// on, where both write a symlink, and UNLINK with it off, where both write a
// plain file holding the target and keep islink.
func TestFreshSymlinkCheckoutMatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	for _, allow := range []string{"on", "off"} {
		repoPath := permHistory(t, bin, allow)
		fossilCk := t.TempDir()
		runFossilIn(t, bin, fossilCk, "open", repoPath)
		want := fossilChanges(t, bin, fossilCk)
		runFossilIn(t, bin, fossilCk, "close", "--force")

		r, err := repo.Open(repoPath)
		if err != nil {
			t.Fatal(err)
		}
		co, rid := createdCheckout(t, r)
		if err := co.Extract(rid, ExtractOpts{}); err != nil {
			r.Close()
			t.Fatalf("allow-symlinks %s: Extract without Force: %v", allow, err)
		}
		got := map[string]string{}
		for name, change := range statusOf(t, co) {
			got[name] = fossilLabel[change]
		}
		co.Close()
		r.Close()
		sameLabels(t, "allow-symlinks "+allow+", fresh checkout", got, want)
		if allow == "off" && want["link"] != "UNLINK" {
			t.Errorf("fossil reports %v with allow-symlinks off; the test's premise is wrong", want)
		}
	}
}
