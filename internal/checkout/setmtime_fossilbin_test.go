package checkout

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

func runFossilIn(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// setmtimeHistory commits three versions with fossil at fixed times, one on
// a whole second: v1 adds a, b, c and e; v2 edits a; v3 edits b and adds d.
// It returns the repository and the three versions' hashes.
func setmtimeHistory(t *testing.T, bin string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "r.fossil")
	ck := filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossilIn(t, bin, dir, "init", repoPath)
	runFossilIn(t, bin, ck, "open", repoPath)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(ck, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "a1\n")
	write("b.txt", "b1\n")
	write("c.txt", "c1\n")
	write("e.txt", "e1\n")
	runFossilIn(t, bin, ck, "add", "a.txt", "b.txt", "c.txt", "e.txt")
	runFossilIn(t, bin, ck, "commit", "-m", "v1", "--date-override", "2030-01-02 03:04:05.000")
	write("a.txt", "a2, longer\n")
	runFossilIn(t, bin, ck, "commit", "-m", "v2", "--date-override", "2030-01-02 03:05:06.789")
	write("b.txt", "b3, longer\n")
	write("d.txt", "d3\n")
	runFossilIn(t, bin, ck, "add", "d.txt")
	runFossilIn(t, bin, ck, "commit", "-m", "v3", "--date-override", "2030-01-02 03:06:07.500")
	runFossilIn(t, bin, ck, "close")
	out := runFossilIn(t, bin, dir, "sql", "-R", repoPath,
		"SELECT uuid FROM blob JOIN event ON rid=objid WHERE type='ci'"+
			" AND comment GLOB 'v[123]' ORDER BY event.mtime")
	versions := strings.Fields(out)
	if len(versions) != 3 {
		t.Fatalf("want 3 versions, got %q", out)
	}
	return repoPath, versions
}

// fileTimes returns each named file's mtime in nanoseconds since 1970.
func fileTimes(t *testing.T, dir string, names []string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, n := range names {
		info, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		out[n] = info.ModTime().UnixNano()
	}
	return out
}

// sentinel is the mtime given to a locally edited file, which --setmtime
// must leave alone.
var sentinel = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

func editLocally(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, sentinel, sentinel); err != nil {
		t.Fatal(err)
	}
}

func ridOf(t *testing.T, r *repo.Repo, uuid string) libfossil.FslID {
	t.Helper()
	var rid int64
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return libfossil.FslID(rid)
}

// TestSetMTimeMatchesFossil checks out the newest version, and updates to it
// from the oldest with a local edit pending, with fossil (--setmtime) and with
// go-libfossil (SetMTime). Fossil gives each unedited file the time of the
// newest check-in in the version's primary ancestry that recorded its content,
// in whole seconds, and leaves an edited file alone; both must agree.
func TestSetMTimeMatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	repoPath, v := setmtimeHistory(t, bin)
	names := []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}

	fossilOpen := t.TempDir()
	runFossilIn(t, bin, fossilOpen, "open", repoPath, v[2], "--setmtime")
	fossilUpdate := t.TempDir()
	runFossilIn(t, bin, fossilUpdate, "open", repoPath, v[0])
	editLocally(t, filepath.Join(fossilUpdate, "c.txt"))
	runFossilIn(t, bin, fossilUpdate, "update", v[2], "--setmtime")
	want := map[string]map[string]int64{
		"extract": fileTimes(t, fossilOpen, names),
		"update":  fileTimes(t, fossilUpdate, names),
	}
	runFossilIn(t, bin, fossilOpen, "close")
	runFossilIn(t, bin, fossilUpdate, "close", "--force")
	if want["update"]["c.txt"] != sentinel.UnixNano() {
		t.Fatalf("fossil changed the edited file's mtime; the test's premise is wrong")
	}

	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := map[string]map[string]int64{}

	goExtract := t.TempDir()
	co, err := Create(r, goExtract, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(ridOf(t, r, v[2]), ExtractOpts{SetMTime: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	co.Close()
	got["extract"] = fileTimes(t, goExtract, names)

	goUpdate := t.TempDir()
	co, err = Create(r, goUpdate, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if err := co.Extract(ridOf(t, r, v[0]), ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	editLocally(t, filepath.Join(goUpdate, "c.txt"))
	if _, err := co.Update(UpdateOpts{TargetRID: ridOf(t, r, v[2]), SetMTime: true}); err != nil {
		t.Fatal(err)
	}
	got["update"] = fileTimes(t, goUpdate, names)

	for _, op := range []string{"extract", "update"} {
		for _, n := range names {
			if got[op][n] != want[op][n] {
				t.Errorf("%s %s: mtime %s, fossil sets %s", op, n,
					time.Unix(0, got[op][n]).UTC(), time.Unix(0, want[op][n]).UTC())
			}
		}
	}
}
