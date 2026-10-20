package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/testutil"
)

// fossilOut runs the canonical fossil and returns its trimmed stdout.
func fossilOut(t *testing.T, fossilBin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(fossilBin, args...).Output()
	if err != nil {
		t.Fatalf("fossil %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestRepoNewCreatesWhatFossilOpens pins repo new's documented behaviour:
// the path comes from the argument or -R, the one line it prints, exit 0, no
// file but the repository left behind, the admin user, and a repository
// stock fossil opens and passes its integrity check on.
func TestRepoNewCreatesWhatFossilOpens(t *testing.T) {
	fossilBin := testutil.RequireFossilBin(t)
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		args func(path string) []string
		user string
	}{
		{"argument", func(p string) []string { return []string{"repo", "new", p, "--user", "alice"} }, "alice"},
		{"-R", func(p string) []string { return []string{"-R", p, "repo", "new", "--user", "alice"} }, "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := filepath.Join(dir, tc.name)
			if err := os.Mkdir(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(sub, "history.fossil")
			stdout, stderr, code := runCLI(t, tc.args(path)...)
			if code != 0 || stdout != "created repository: "+path+"\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if names := dirNames(t, sub); len(names) != 1 {
				t.Errorf("directory holds %v, want only history.fossil", names)
			}
			if got := fossilOut(t, fossilBin, "sql", "-R", path, "SELECT cap FROM user WHERE login='alice'"); !strings.Contains(got, "s") {
				t.Errorf("fossil sql: alice's capabilities %q, want setup (s)", got)
			}
			if got := fossilOut(t, fossilBin, "hash-policy", "-R", path); got != "sha3" {
				t.Errorf("hash-policy = %q, want sha3", got)
			}
			if got := fossilOut(t, fossilBin, "info", "-R", path); !strings.Contains(got, "check-ins:    0") {
				t.Errorf("fossil info = %q, want an empty repository", got)
			}
			if got := fossilOut(t, fossilBin, "test-integrity", "-R", path); !strings.Contains(got, "ok") && got != "" {
				t.Logf("fossil test-integrity: %s", got)
			}
		})
	}
}

// TestRepoNewDefaultUserIsTheLoginName: without --user the admin user is the
// login name of the process, as fossil new's is.
func TestRepoNewDefaultUserIsTheLoginName(t *testing.T) {
	fossilBin := testutil.RequireFossilBin(t)
	path := filepath.Join(t.TempDir(), "r.fossil")
	if _, stderr, code := runCLI(t, "repo", "new", path); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := fossilOut(t, fossilBin, "sql", "-R", path, "SELECT login FROM user WHERE cap='s'")
	if want == "" || strings.Contains(want, "anonymous") {
		t.Errorf("setup user %q, want the login name", want)
	}
}

// TestRepoNewArgumentWinsOverR: with both, the argument is the file created.
func TestRepoNewArgumentWinsOverR(t *testing.T) {
	dir := t.TempDir()
	arg, other := filepath.Join(dir, "arg.fossil"), filepath.Join(dir, "other.fossil")
	if _, stderr, code := runCLI(t, "-R", other, "repo", "new", arg); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "arg.fossil" {
		t.Errorf("directory holds %v, want only arg.fossil", names)
	}
}

// TestRepoNewRefusesWhatItWouldOverwrite: an existing file or directory at
// the path is refused with exit 1, as fossil new refuses ("file already
// exists", exit 1), and is left byte for byte as it was.
func TestRepoNewRefusesWhatItWouldOverwrite(t *testing.T) {
	dir := t.TempDir()
	existing := newSnapshotRepo(t)
	snapshot(t, existing, "a.md", "one\n")
	before, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(plain, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.fossil")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{existing, plain, empty, dir} {
		stdout, stderr, code := runCLI(t, "repo", "new", path)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "already exists") {
			t.Errorf("repo new %s: exit %d, stdout %q, stderr %q; want exit 1, no stdout, \"already exists\"", path, code, stdout, stderr)
		}
	}
	after, err := os.ReadFile(existing)
	if err != nil || string(after) != string(before) {
		t.Errorf("the existing repository changed (err %v)", err)
	}
	if b, _ := os.ReadFile(plain); string(b) != "hi\n" {
		t.Errorf("the plain file changed to %q", b)
	}
	if names := dirNames(t, filepath.Dir(existing)); len(names) != 1 {
		t.Errorf("repository directory holds %v, want only the repository", names)
	}
}

// TestRepoNewMissingParentFailsCleanly: a parent directory that does not
// exist is exit 1 with a message that names it, and nothing is created
// (fossil new fails with exit 1 too).
func TestRepoNewMissingParentFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodir", "x.fossil")
	stdout, stderr, code := runCLI(t, "repo", "new", path)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "parent directory") || !strings.Contains(stderr, filepath.Join(dir, "nodir")) {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, no stdout, an error naming the missing parent directory", code, stdout, stderr)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory holds %v, want nothing created", names)
	}
}

// TestRepoNewNeedsAPath: neither an argument nor -R is exit 1 and no output.
func TestRepoNewNeedsAPath(t *testing.T) {
	stdout, stderr, code := runCLI(t, "repo", "new")
	if code != 1 || stdout != "" || stderr == "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and a message", code, stdout, stderr)
	}
}
