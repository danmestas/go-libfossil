package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/testutil"
)

// exitNotFound is the documented exit status for a version, path or history
// that does not exist.
const exitNotFound = 2

var uuidRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// runCLIStdin is runCLI with stdin supplied, for the commands that read the
// content to commit from it.
func runCLIStdin(t *testing.T, stdin string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "LIBFOSSIL_TEST_MAIN=1")
	cmd.Stdin = strings.NewReader(stdin)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	switch e := err.(type) {
	case nil:
		exitCode = 0
	case *exec.ExitError:
		exitCode = e.ExitCode()
	default:
		t.Fatalf("running subprocess: %v", err)
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func newSnapshotRepo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.fossil")
	stdout, stderr, code := runCLI(t, "repo", "new", path, "--user", "tester")
	if code != 0 {
		t.Fatalf("repo new exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	return path
}

// snapshot commits content at name through stdin and returns the printed uuid.
func snapshot(t *testing.T, repoPath, name, content string) string {
	t.Helper()
	stdout, stderr, code := runCLIStdin(t, content,
		"-R", repoPath, "repo", "snapshot", "-m", "snap "+name, "--user", "tester",
		"--path", name, "--stdin")
	if code != 0 {
		t.Fatalf("snapshot %s exited %d\nstdout:\n%s\nstderr:\n%s", name, code, stdout, stderr)
	}
	uuid := strings.TrimSpace(stdout)
	if !uuidRE.MatchString(uuid) {
		t.Fatalf("snapshot %s printed %q, want a single uuid", name, stdout)
	}
	return uuid
}

type logRow struct {
	UUID     string `json:"uuid"`
	Time     string `json:"time"`
	User     string `json:"user"`
	Comment  string `json:"comment"`
	Action   string `json:"action"`
	FileUUID string `json:"file_uuid"`
	PrevName string `json:"prev_name"`
}

func pathLog(t *testing.T, repoPath string, args ...string) []logRow {
	t.Helper()
	full := append([]string{"-R", repoPath, "repo", "log", "--json"}, args...)
	stdout, stderr, code := runCLI(t, full...)
	if code != 0 {
		t.Fatalf("repo log exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	var rows []logRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("repo log --json is not a JSON array: %v\n%s", err, stdout)
	}
	return rows
}

func TestSnapshotCommitsBytesWithoutCheckout(t *testing.T) {
	repoPath := newSnapshotRepo(t)

	uuid := snapshot(t, repoPath, "notes/x.md", "hello\n")

	stdout, stderr, code := runCLI(t, "-R", repoPath, "repo", "cat", "--at", uuid, "notes/x.md")
	if code != 0 || stdout != "hello\n" {
		t.Fatalf("cat exited %d, stdout %q, stderr %q; want hello", code, stdout, stderr)
	}
}

func TestSnapshotIdenticalBytesIsNoOp(t *testing.T) {
	repoPath := newSnapshotRepo(t)

	first := snapshot(t, repoPath, "x.md", "same\n")
	again := snapshot(t, repoPath, "x.md", "same\n")
	if again != first {
		t.Fatalf("no-op snapshot printed %s, want the existing tip %s", again, first)
	}
	if rows := pathLog(t, repoPath, "--path", "x.md"); len(rows) != 1 {
		t.Fatalf("log has %d rows after a no-op snapshot, want 1", len(rows))
	}

	stdout, stderr, code := runCLIStdin(t, "same\n",
		"-R", repoPath, "repo", "snapshot", "-m", "m", "--path", "x.md", "--stdin", "--json")
	if code != 0 {
		t.Fatalf("snapshot --json exited %d: %s", code, stderr)
	}
	var res struct {
		UUID    string `json:"uuid"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("snapshot --json output %q: %v", stdout, err)
	}
	if res.UUID != first || res.Changed {
		t.Fatalf("snapshot --json = %+v, want uuid %s changed=false", res, first)
	}

	second := snapshot(t, repoPath, "x.md", "different\n")
	if second == first {
		t.Fatalf("changed bytes produced no new check-in")
	}
}

func TestSnapshotReadsPathFromDiskWithoutStdin(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.md"), []byte("from disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-R", repoPath, "repo", "snapshot", "-m", "m", "--path", "disk.md")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LIBFOSSIL_TEST_MAIN=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("snapshot from disk: %v\n%s", err, out)
	}

	stdout, _, code := runCLI(t, "-R", repoPath, "repo", "cat", "--at", "tip", "disk.md")
	if code != 0 || stdout != "from disk\n" {
		t.Fatalf("cat exited %d, stdout %q; want the disk bytes", code, stdout)
	}
}

// TestSnapshotRejectsBadPaths checks that a --path fossil would not accept
// as a repository file name fails at exit 1 with a message saying why, and
// commits nothing: not a simple pathname, a control byte, or one of the names
// fossil reserves (file.c file_is_reserved_name).
func TestSnapshotRejectsBadPaths(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	for _, tc := range []struct{ path, why string }{
		{"../x.md", "clean relative path"},
		{"/abs.md", "clean relative path"},
		{"a/../../x", "clean relative path"},
		{"", "clean relative path"},
		{"a//b", "clean relative path"},
		{"./x", "clean relative path"},
		{"a/", "clean relative path"},
		{`a\b.md`, "clean relative path"},
		{"a\tb.md", "control character"},
		{"a\nb.md", "control character"},
		{"x\x1f.md", "control character"},
		{"\x7f", ""}, // DEL is not below 0x20: accepted
		{"_FOSSIL_", "reserved"},
		{"sub/_fossil_", "reserved"},
		{".fslckout", "reserved"},
		{"sub/.FSLCKOUT", "reserved"},
		{".fslckout-journal", "reserved"},
		{"_FOSSIL_-wal", "reserved"},
		{"sub/.fslckout-shm", "reserved"},
		{"my_FOSSIL_", ""}, // a tail match without a separator is a plain name
	} {
		stdout, stderr, code := runCLIStdin(t, "x",
			"-R", repoPath, "repo", "snapshot", "-m", "m", "--path", tc.path, "--stdin")
		if tc.why == "" {
			if code != 0 {
				t.Errorf("snapshot --path %q exited %d, want 0: %s", tc.path, code, stderr)
			}
			continue
		}
		if code != 1 || stdout != "" || !strings.Contains(stderr, tc.why) {
			t.Errorf("snapshot --path %q: exit %d, stdout %q, stderr %q; want exit 1 naming %q",
				tc.path, code, stdout, stderr, tc.why)
		}
	}
}

// TestSnapshotRefusesRepoWithoutTrunk checks that a snapshot into a
// repository that has check-ins but none its main branch names fails at exit
// 1 and commits nothing, rather than starting a second, unrelated root.
func TestSnapshotRefusesRepoWithoutTrunk(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	snapshot(t, repoPath, "a.md", "one\n")
	if _, stderr, code := runCLI(t, "-R", repoPath, "repo", "config", "set", "main-branch", "develop"); code != 0 {
		t.Fatalf("config set main-branch exited %d: %s", code, stderr)
	}

	stdout, stderr, code := runCLIStdin(t, "two\n",
		"-R", repoPath, "repo", "snapshot", "-m", "m", "--user", "tester", "--path", "a.md", "--stdin")
	if code != 1 || stdout != "" || !strings.Contains(stderr, `"develop"`) {
		t.Errorf("snapshot with no develop check-in: exit %d, stdout %q, stderr %q; want exit 1 naming the branch",
			code, stdout, stderr)
	}

	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var checkins int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&checkins); err != nil {
		t.Fatal(err)
	}
	if checkins != 1 {
		t.Errorf("repository has %d check-ins after the refused snapshot, want 1", checkins)
	}
}

func TestSnapshotKeepsOtherFilesOnTrunk(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	snapshot(t, repoPath, "a.md", "a\n")
	snapshot(t, repoPath, "b.md", "b\n")

	stdout, _, code := runCLI(t, "-R", repoPath, "repo", "ls", "tip")
	if code != 0 || strings.Join(strings.Fields(stdout), " ") != "a.md b.md" {
		t.Fatalf("ls tip = %q (exit %d), want a.md and b.md", stdout, code)
	}
	trunk, _, _ := runCLI(t, "-R", repoPath, "repo", "resolve", "trunk")
	tip, _, _ := runCLI(t, "-R", repoPath, "repo", "resolve", "tip")
	if trunk != tip || !strings.Contains(trunk, "uuid: ") {
		t.Fatalf("trunk %q is not the tip %q", trunk, tip)
	}
}

func TestLogPathListsOnlyCheckinsThatChangedIt(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	a1 := snapshot(t, repoPath, "a.md", "one\n")
	snapshot(t, repoPath, "b.md", "other\n")
	a2 := snapshot(t, repoPath, "a.md", "two\n")
	a3 := snapshot(t, repoPath, "a.md", "three\n")

	rows := pathLog(t, repoPath, "--path", "a.md")
	if len(rows) != 3 {
		t.Fatalf("log --path a.md has %d rows, want 3: %+v", len(rows), rows)
	}
	wantUUID := []string{a3, a2, a1}
	wantAction := []string{"modified", "modified", "added"}
	timeRE := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	for i, r := range rows {
		if r.UUID != wantUUID[i] || r.Action != wantAction[i] {
			t.Errorf("row %d = %s %s, want %s %s", i, r.UUID, r.Action, wantUUID[i], wantAction[i])
		}
		if r.User != "tester" || r.Comment != "snap a.md" {
			t.Errorf("row %d user/comment = %q/%q", i, r.User, r.Comment)
		}
		if !uuidRE.MatchString(r.FileUUID) {
			t.Errorf("row %d file_uuid = %q", i, r.FileUUID)
		}
		if !timeRE.MatchString(r.Time) {
			t.Errorf("row %d time = %q, want RFC 3339 UTC with milliseconds", i, r.Time)
		}
	}

	if got := pathLog(t, repoPath, "--path", "a.md", "-n", "2"); len(got) != 2 || got[0].UUID != a3 {
		t.Errorf("log -n 2 = %+v, want the two newest", got)
	}
	if got := pathLog(t, repoPath, "--path", "b.md"); len(got) != 1 {
		t.Errorf("log --path b.md has %d rows, want 1", len(got))
	}
}

// TestLogTimeIsTheDCard checks that each log row's time is its check-in's D
// card to the millisecond. Converting the stored Julian day by truncation put
// most rows 1ms early; a dozen check-ins make that near certain to show.
func TestLogTimeIsTheDCard(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	for i := 0; i < 12; i++ {
		snapshot(t, repoPath, "a.md", fmt.Sprintf("v%d\n", i))
	}
	rows := pathLog(t, repoPath, "--path", "a.md")
	if len(rows) != 12 {
		t.Fatalf("log has %d rows, want 12", len(rows))
	}
	for i, r := range rows {
		if want := dCard(t, repoPath, r.UUID) + "Z"; r.Time != want {
			t.Errorf("row %d time = %s, its D card says %s", i, r.Time, want)
		}
	}
}

// dCard returns the D card of check-in uuid, as the manifest records it.
func dCard(t *testing.T, repoPath, uuid string) string {
	t.Helper()
	stdout, stderr, code := runCLI(t, "-R", repoPath, "repo", "cat", uuid)
	if code != 0 {
		t.Fatalf("repo cat %s exited %d: %s", uuid, code, stderr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if d, ok := strings.CutPrefix(line, "D "); ok {
			return d
		}
	}
	t.Fatalf("check-in %s has no D card:\n%s", uuid, stdout)
	return ""
}

func TestLogPathTextOutput(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	uuid := snapshot(t, repoPath, "a.md", "one\n")

	stdout, stderr, code := runCLI(t, "-R", repoPath, "repo", "log", "--path", "a.md")
	if code != 0 {
		t.Fatalf("log exited %d: %s", code, stderr)
	}
	if !strings.HasPrefix(stdout, uuid[:10]) || !strings.Contains(stdout, "snap a.md") {
		t.Fatalf("log text = %q, want the short uuid and comment", stdout)
	}
}

func TestLogUnknownPathExitsNotFound(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	snapshot(t, repoPath, "a.md", "one\n")

	stdout, _, code := runCLI(t, "-R", repoPath, "repo", "log", "--json", "--path", "missing.md")
	if code != exitNotFound || stdout != "" {
		t.Fatalf("log of unknown path: exit %d, stdout %q; want exit %d and no stdout", code, stdout, exitNotFound)
	}
}

func TestCatAtVersions(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	v1 := snapshot(t, repoPath, "a.md", "one\n")
	v2 := snapshot(t, repoPath, "a.md", "two\n")

	for _, tc := range []struct{ at, want string }{
		{v1, "one\n"}, {v2, "two\n"}, {"tip", "two\n"}, {v1[:12], "one\n"}, {"trunk", "two\n"},
	} {
		stdout, stderr, code := runCLI(t, "-R", repoPath, "repo", "cat", "--at", tc.at, "a.md")
		if code != 0 || stdout != tc.want {
			t.Errorf("cat --at %s: exit %d stdout %q stderr %q, want %q", tc.at, code, stdout, stderr, tc.want)
		}
	}
}

func TestCatAtMissingExitsNotFound(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	v1 := snapshot(t, repoPath, "a.md", "one\n")

	if _, _, code := runCLI(t, "-R", repoPath, "repo", "cat", "--at", v1, "nope.md"); code != exitNotFound {
		t.Errorf("cat of a missing path exited %d, want %d", code, exitNotFound)
	}
	if _, _, code := runCLI(t, "-R", repoPath, "repo", "cat", "--at", "deadbeefdead", "a.md"); code != exitNotFound {
		t.Errorf("cat at a missing version exited %d, want %d", code, exitNotFound)
	}
}

func TestCatArtifactStillWorks(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	uuid := snapshot(t, repoPath, "a.md", "one\n")

	stdout, _, code := runCLI(t, "-R", repoPath, "repo", "cat", uuid)
	if code != 0 || !strings.Contains(stdout, "F a.md") {
		t.Fatalf("cat of a check-in artifact: exit %d stdout %q, want its manifest", code, stdout)
	}
}

func TestDiffJSONBetweenVersions(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	v1 := snapshot(t, repoPath, "a.md", "one\n")
	v2 := snapshot(t, repoPath, "a.md", "two\n")

	stdout, stderr, code := runCLI(t, "-R", repoPath, "repo", "diff", "--json", v1, v2, "a.md")
	if code != 0 {
		t.Fatalf("diff exited %d: %s", code, stderr)
	}
	var entries []struct {
		Name    string `json:"name"`
		Unified string `json:"unified"`
	}
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Fatalf("diff --json is not a JSON array: %v\n%s", err, stdout)
	}
	if len(entries) != 1 || entries[0].Name != "a.md" ||
		!strings.Contains(entries[0].Unified, "-one") || !strings.Contains(entries[0].Unified, "+two") {
		t.Fatalf("diff --json = %+v, want a.md one -> two", entries)
	}

	stdout, _, code = runCLI(t, "-R", repoPath, "repo", "diff", "--json", v2, v2, "a.md")
	if code != 0 || strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("diff of identical versions: exit %d stdout %q, want []", code, stdout)
	}

	stdout, _, code = runCLI(t, "-R", repoPath, "repo", "diff", v1, v2, "a.md")
	if code != 0 || !strings.Contains(stdout, "+two") {
		t.Fatalf("diff text: exit %d stdout %q", code, stdout)
	}
}

// TestDiffPathMatchesFossil checks diff A B PATH against fossil diff --from A
// --to B PATH on the same repository. PATH names a file, or a directory
// holding files, in either version; one in neither is an error ("not found",
// exit 2 here), not an empty diff. An unchanged file is an empty diff.
func TestDiffPathMatchesFossil(t *testing.T) {
	fossilBin := testutil.RequireFossilBin(t)
	repoPath := newSnapshotRepo(t)
	v1 := snapshot(t, repoPath, "notes/a.md", "one\n")
	snapshot(t, repoPath, "keep.md", "same\n")
	v2 := snapshot(t, repoPath, "notes/a.md", "two\n")

	for _, tc := range []struct {
		path    string
		found   bool
		changed bool
	}{
		{"notes/a.md", true, true},
		{"notes", true, true},
		{"keep.md", true, false},
		{"nosuch.md", false, false},
		{"note", false, false}, // a name prefix that is not a directory
	} {
		fossilOut, ferr := exec.Command(fossilBin, "diff", "-R", repoPath,
			"--from", v1, "--to", v2, tc.path).CombinedOutput()
		if (ferr == nil) != tc.found {
			t.Fatalf("fossil diff %s: err %v, the case expects found=%v\n%s", tc.path, ferr, tc.found, fossilOut)
		}

		stdout, stderr, code := runCLI(t, "-R", repoPath, "repo", "diff", "--json", v1, v2, tc.path)
		if !tc.found {
			if code != exitNotFound || stdout != "" || !strings.Contains(stderr, "not found") {
				t.Errorf("diff %s: exit %d, stdout %q, stderr %q; fossil fails (%s), want exit %d naming it not found",
					tc.path, code, stdout, stderr, strings.TrimSpace(string(fossilOut)), exitNotFound)
			}
			continue
		}
		var entries []struct {
			Name string `json:"name"`
		}
		if code != 0 || json.Unmarshal([]byte(stdout), &entries) != nil {
			t.Errorf("diff %s: exit %d, stdout %q, stderr %q; fossil succeeds", tc.path, code, stdout, stderr)
			continue
		}
		if got := len(entries) == 1 && entries[0].Name == "notes/a.md"; got != tc.changed {
			t.Errorf("diff %s = %+v, want changed=%v", tc.path, entries, tc.changed)
		}
		if fossilChanged := strings.Contains(string(fossilOut), "+two"); fossilChanged != tc.changed {
			t.Errorf("fossil diff %s changed=%v, the case expects %v", tc.path, fossilChanged, tc.changed)
		}
	}
}

func TestDiffMissingVersionExitsNotFound(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	v1 := snapshot(t, repoPath, "a.md", "one\n")

	if _, _, code := runCLI(t, "-R", repoPath, "repo", "diff", "--json", v1, "deadbeefdead", "a.md"); code != exitNotFound {
		t.Errorf("diff at a missing version exited %d, want %d", code, exitNotFound)
	}
}

func TestDiffJSONNeedsTwoVersions(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	if _, _, code := runCLI(t, "-R", repoPath, "repo", "diff", "--json"); code != 1 {
		t.Errorf("diff --json with no versions exited %d, want 1 (the parser accepts it; the command refuses it)", code)
	}
}

// exitUsage is the status the command-line parser exits with on a usage
// error, as cli-embedding.md documents.
const exitUsage = 80

// TestUsageErrorsExit80 pins the documented usage-error status, measured on
// the binary itself: `go run` reports any non-zero status as 1.
func TestUsageErrorsExit80(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	for _, args := range [][]string{
		{"repo", "log", "--path", "a.md", "--bogus"}, // unknown flag
		{"repo", "log"}, // missing required flag
		{"repo", "snapshot", "--path", "a.md", "--stdin"}, // missing required -m
		{"repo", "nosuch"}, // unknown subcommand
		{"repo", "cat"},    // missing argument
	} {
		stdout, stderr, code := runCLI(t, append([]string{"-R", repoPath}, args...)...)
		if code != exitUsage || stdout != "" {
			t.Errorf("%v: exit %d, stdout %q; want exit %d and no stdout\nstderr: %s",
				args, code, stdout, exitUsage, stderr)
		}
	}
}

// TestSnapshotConcurrentProcesses runs two processes committing to one
// repository at once. Every commit must land, on one linear trunk whose tip
// holds every file (no commit built on a stale tip loses another's file), and
// the repository must pass canonical fossil's integrity check.
func TestSnapshotConcurrentProcesses(t *testing.T) {
	fossilBin := testutil.RequireFossilBin(t)
	repoPath := newSnapshotRepo(t)

	const procs, perProc = 2, 8
	var wg sync.WaitGroup
	failures := make(chan string, procs*perProc)
	for p := 0; p < procs; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProc; i++ {
				name := fmt.Sprintf("p%d/note%d.md", p, i)
				_, stderr, code := runCLIStdin(t, fmt.Sprintf("body %d %d\n", p, i),
					"-R", repoPath, "repo", "snapshot", "-m", "snap "+name,
					"--user", "tester", "--path", name, "--stdin")
				if code != 0 {
					failures <- fmt.Sprintf("%s exited %d: %s", name, code, stderr)
				}
			}
		}(p)
	}
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Error(f)
	}

	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rid, err := r.ResolveVersion("trunk")
	if err != nil {
		t.Fatal(err)
	}
	files, err := r.ListFiles(rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != procs*perProc {
		t.Errorf("trunk tip holds %d files, want %d (a commit built on a stale tip dropped files)", len(files), procs*perProc)
	}
	var leaves int
	if err := r.DB().QueryRow(
		"SELECT count(*) FROM leaf WHERE rid IN (SELECT objid FROM event WHERE type='ci')",
	).Scan(&leaves); err != nil {
		t.Fatal(err)
	}
	if leaves != 1 {
		t.Errorf("repository has %d leaves, want 1 (the trunk forked)", leaves)
	}
	if err := r.Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}

	out, err := exec.Command(fossilBin, "test-integrity", repoPath).CombinedOutput()
	if err != nil {
		t.Errorf("fossil test-integrity: %v\n%s", err, out)
	}
}

// TestLogPathFollowsRename checks that a path's history reports the check-in
// that renamed it, with the old name.
func TestLogPathFollowsRename(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	snapshot(t, repoPath, "a.md", "one\n")

	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := r.ResolveVersion("tip")
	if err != nil {
		t.Fatal(err)
	}
	_, renameUUID, err := manifest.Checkin(r.Inner(), manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "b.md", Content: []byte("one\n"), OldName: "a.md"}},
		Comment: "rename",
		User:    "tester",
		Parent:  fsltype.FslID(parent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	rows := pathLog(t, repoPath, "--path", "b.md")
	if len(rows) == 0 || rows[0].UUID != renameUUID || rows[0].Action != "renamed" || rows[0].PrevName != "a.md" {
		t.Fatalf("log --path b.md = %+v, want the rename from a.md first", rows)
	}
}
