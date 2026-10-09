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
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
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

// derivedRows reads the derived tables #258 compares, in a stable order.
func derivedRows(t *testing.T, path string) map[string][]string {
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
	queries := map[string]string{
		// Check-in events only: Rebuild does not yet recreate the events of
		// other artifacts (control artifacts, wiki, tickets, forum), a gap
		// apart from the values #258 is about.
		"event": `SELECT objid, type, printf('%!.20g', mtime), printf('%!.20g', omtime), user,
			comment FROM event WHERE type='ci' ORDER BY objid`,
		"plink": `SELECT pid, cid, isprim, printf('%!.20g', mtime) FROM plink ORDER BY cid, pid`,
		"tagxref": `SELECT tagid, rid, tagtype, quote(value), printf('%!.20g', mtime)
			FROM tagxref ORDER BY tagid, rid`,
	}
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
