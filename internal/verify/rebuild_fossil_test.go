package verify_test

// verify.Rebuild against tables the fossil binary wrote (#258): a repository
// made entirely by fossil is copied and rebuilt, and every event, plink and
// tagxref row must come back with fossil's values, times bit for bit.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/branch"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

func fossilCmd(t *testing.T, bin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fossil %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// fossilHistory commits a few versions with the fossil binary, on trunk and
// a branch, and tags one with a value and one without.
func fossilHistory(t *testing.T) string {
	t.Helper()
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "f.fossil")
	ck := filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilCmd(t, bin, dir, "init", repoPath)
	fossilCmd(t, bin, ck, "open", repoPath)
	path := filepath.Join(ck, "f.txt")
	mtime := time.Unix(1_700_000_000, 0)
	for v := 0; v < 4; v++ {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("version %d\n", v)), 0o644); err != nil {
			t.Fatal(err)
		}
		mtime = mtime.Add(10 * time.Second)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		if v == 0 {
			fossilCmd(t, bin, ck, "add", "f.txt")
		}
		args := []string{"commit", "-m", fmt.Sprintf("v%d", v)}
		if v == 2 {
			args = append(args, "--branch", "feat")
		}
		fossilCmd(t, bin, ck, args...)
	}
	fossilCmd(t, bin, ck, "tag", "add", "release", "current")
	fossilCmd(t, bin, ck, "tag", "add", "note", "current", "with a value")
	return repoPath
}

// fossilTextTimesExact reports whether the fossil binary on this machine
// writes text times the way TimeToTextJulian computes them: its %.17g runs in
// long double, which is a 64-bit double on macOS arm64 and Windows but wider
// on Linux and Intel macOS, where the 16th digit can differ.
func fossilTextTimesExact() bool {
	return (runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") || runtime.GOOS == "windows"
}

// textTime renders a column fossil writes as SQL text: bit for bit where
// this machine's fossil computes it as TimeToTextJulian does, otherwise to
// the millisecond the value stands for.
func textTime(col string) string {
	if fossilTextTimesExact() {
		return "printf('%!.20g', " + col + ")"
	}
	return "CAST(round((" + col + " - 2440587.5) * 86400000) AS INTEGER)"
}

// derivedRows reads the derived tables #258 compares, in a stable order.
// Tag times are SQLite's julianday() on every platform, so they are compared
// bit for bit; see textTime for the others.
func derivedRows(t *testing.T, path string) map[string][]string {
	t.Helper()
	return queryRows(t, path, map[string]string{
		"event": "SELECT objid, type, " + textTime("mtime") + ", " + textTime("omtime") +
			", user, comment FROM event ORDER BY objid",
		"plink": "SELECT pid, cid, isprim, " + textTime("mtime") +
			" FROM plink ORDER BY cid, pid",
		"tagxref": `SELECT tagid, rid, tagtype, quote(value), printf('%!.20g', mtime)
			FROM tagxref ORDER BY tagid, rid`,
	})
}

// derivedTimes reads the times fossil derives from the artifacts into
// event, plink and tagxref, keyed by the row each belongs to.
func derivedTimes(t *testing.T, path string) map[string][]string {
	t.Helper()
	return queryRows(t, path, map[string]string{
		"event": "SELECT objid, type, " + textTime("mtime") + ", " + textTime("omtime") +
			" FROM event ORDER BY objid",
		"plink": "SELECT pid, cid, " + textTime("mtime") + " FROM plink ORDER BY cid, pid",
		"tagxref": `SELECT tagid, rid, srcid, origid, printf('%!.20g', mtime)
			FROM tagxref ORDER BY tagid, rid`,
	})
}

// queryRows runs each named query against the repository at path and
// returns its rows, each rendered as one string.
func queryRows(t *testing.T, path string, queries map[string]string) map[string][]string {
	t.Helper()
	d, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	}()
	out := map[string][]string{}
	for name, q := range queries {
		rows, err := d.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out[name] = append(out[name], fmt.Sprint(vals...))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildMatchesFossilDerivedValues(t *testing.T) {
	orig := fossilHistory(t)
	rebuilt := filepath.Join(t.TempDir(), "rebuilt.fossil")
	copyFile(t, orig, rebuilt)

	r, err := repo.Open(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify.Rebuild(r); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	want, got := derivedRows(t, orig), derivedRows(t, rebuilt)
	for table := range want {
		if !reflect.DeepEqual(got[table], want[table]) {
			t.Errorf("%s after Rebuild:\n  %s\nfossil wrote:\n  %s", table,
				strings.Join(got[table], "\n  "), strings.Join(want[table], "\n  "))
		}
	}
}

// goTimesHistory commits with go-libfossil at times given to the nanosecond
// and at the clock's, tags one check-in and branches from it.
func goTimesHistory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "g.fossil")
	r, err := repo.Create(path, "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	base := time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC)
	times := []time.Time{base.Add(789600 * time.Microsecond), base.Add(time.Minute + 789500*time.Microsecond), {}}
	var parent fsltype.FslID
	for i, when := range times {
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "f.txt", Content: []byte(fmt.Sprintf("v%d\n", i))}},
			Comment: fmt.Sprintf("v%d", i), User: "u", Parent: parent, Time: when,
		})
		if err != nil {
			t.Fatal(err)
		}
		parent = rid
	}
	if _, err := tag.AddTag(r, tag.TagOpts{TargetRID: parent, TagName: "sym-rel",
		TagType: tag.TagSingleton, User: "u", Time: base.Add(2*time.Minute + 123456789)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := branch.Create(r, branch.CreateOpts{Name: "feat", Parent: parent, User: "u"}); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFossilRebuildKeepsGoTimes rebuilds a repository go-libfossil wrote with
// the fossil binary: every event, plink and tagxref time fossil derives from
// the artifacts must be the one go-libfossil stored, bit for bit where this
// machine's fossil computes text times as go-libfossil does (textTime).
func TestFossilRebuildKeepsGoTimes(t *testing.T) {
	testutil.RequireFossilBin(t)
	path := goTimesHistory(t)
	stored := derivedTimes(t, path)
	if err := testutil.FossilRebuild(path); err != nil {
		t.Fatal(err)
	}
	rebuilt := derivedTimes(t, path)
	for table := range stored {
		if !reflect.DeepEqual(stored[table], rebuilt[table]) {
			t.Errorf("%s as go-libfossil stored it:\n  %s\nafter fossil rebuild:\n  %s", table,
				strings.Join(stored[table], "\n  "), strings.Join(rebuilt[table], "\n  "))
		}
	}
}
