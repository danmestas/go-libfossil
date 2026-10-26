package apitest_test

// Interop tests for vfile.mtime: fossil and go-libfossil record a checkout
// file's mtime the same way and trust it the same way, so each reads the
// other's checkout as it reads its own.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"
)

// agedTime is a whole second long past, given to checkout files so that
// their mtimes are recorded.
var agedTime = time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)

func setMTime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// vfileMTimes reads pathname → vfile.mtime from a checkout's database.
func vfileMTimes(t *testing.T, ckDir string) map[string]int64 {
	t.Helper()
	d, err := db.Open(filepath.Join(ckDir, ".fslckout"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query("SELECT pathname, coalesce(mtime, 0) FROM vfile")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var mtime int64
		if err := rows.Scan(&name, &mtime); err != nil {
			t.Fatal(err)
		}
		out[name] = mtime
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// fossilEdited lists the files fossil's changes command reports EDITED.
func fossilEdited(t *testing.T, bin, ckDir string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(fossilRun(t, bin, ckDir, "changes"), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "EDITED" {
			out = append(out, f[1])
		}
	}
	sort.Strings(out)
	return out
}

// libfossilEdited lists the files Status reports modified.
func libfossilEdited(t *testing.T, ck *libfossil.Checkout) []string {
	t.Helper()
	var out []string
	for _, line := range statusLines(t, ck) {
		if name, ok := strings.CutPrefix(line, "modified "); ok {
			out = append(out, name)
		}
	}
	return out
}

// editKeepingSignature gives a.txt new content of the same size and puts
// its recorded mtime back: a scan that trusts mtimes cannot see it. b.txt
// changes size, which any scan sees.
func editKeepingSignature(t *testing.T, ckDir string) {
	t.Helper()
	writeFile(t, filepath.Join(ckDir, "a.txt"), "ONE\n")
	setMTime(t, filepath.Join(ckDir, "a.txt"), agedTime)
	writeFile(t, filepath.Join(ckDir, "b.txt"), "two, longer\n")
	setMTime(t, filepath.Join(ckDir, "b.txt"), agedTime)
}

// assertBothSee checks that fossil and go-libfossil report want as edited.
func assertBothSee(t *testing.T, bin, ckDir string, ck *libfossil.Checkout, want string) {
	t.Helper()
	if got := strings.Join(libfossilEdited(t, ck), ","); got != want {
		t.Errorf("go-libfossil edited = %q, want %q", got, want)
	}
	if got := strings.Join(fossilEdited(t, bin, ckDir), ","); got != want {
		t.Errorf("fossil edited = %q, want %q", got, want)
	}
}

// A checkout fossil opened and scanned: go-libfossil trusts the mtimes
// fossil recorded exactly as fossil does, and both hash every file once
// mtime-changes is off.
func TestFossilRecordedMTimesTrustedLikeFossil(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	setMTime(t, filepath.Join(ckDir, "a.txt"), agedTime)
	setMTime(t, filepath.Join(ckDir, "b.txt"), agedTime)
	fossilRun(t, bin, ckDir, "changes")
	for name, m := range vfileMTimes(t, ckDir) {
		if m != agedTime.Unix() {
			t.Fatalf("fossil recorded %s mtime %d, want %d; the test's premise is wrong",
				name, m, agedTime.Unix())
		}
	}

	editKeepingSignature(t, ckDir)
	ck := openLibfossilCheckout(t, repoPath, ckDir)
	assertBothSee(t, bin, ckDir, ck, "b.txt")

	fossilRun(t, bin, ckDir, "settings", "mtime-changes", "off")
	assertBothSee(t, bin, ckDir, ck, "a.txt,b.txt")
}

// A checkout go-libfossil made and scanned: fossil finds the mtimes it would
// have recorded itself, leaves them as they are, and trusts them.
func TestLibfossilRecordedMTimesTrustedByFossil(t *testing.T) {
	bin, repoPath, fossilDir := newFossilCheckout(t)
	fossilRun(t, bin, fossilDir, "close")

	ckDir := filepath.Join(t.TempDir(), "ck")
	if err := os.Mkdir(ckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ck, err := r.CreateCheckout(ckDir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ck.Close() })
	rid, _, err := ck.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := ck.Extract(rid, libfossil.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	setMTime(t, filepath.Join(ckDir, "a.txt"), agedTime)
	setMTime(t, filepath.Join(ckDir, "b.txt"), agedTime)
	if got := statusLines(t, ck); len(got) != 0 {
		t.Fatalf("fresh checkout: Status = %q, want none", got)
	}

	recorded := vfileMTimes(t, ckDir)
	for name, m := range recorded {
		if m != agedTime.Unix() {
			t.Fatalf("go-libfossil recorded %s mtime %d, want %d", name, m, agedTime.Unix())
		}
	}
	fossilRun(t, bin, ckDir, "changes")
	for name, m := range vfileMTimes(t, ckDir) {
		if m != recorded[name] {
			t.Errorf("fossil rewrote %s mtime %d to %d", name, recorded[name], m)
		}
	}

	editKeepingSignature(t, ckDir)
	assertBothSee(t, bin, ckDir, ck, "b.txt")
}

// A file whose mtime is not older than the scan's second is recorded with
// mtime 0, so fossil reading the row hashes the file and sees a same-size
// edit that keeps the mtime; fossil's own row would have hidden it. A time
// ahead of the clock stands in for the current second, deterministically.
func TestFossilSeesEditBehindUnrecordedMTime(t *testing.T) {
	bin, repoPath, fossilDir := newFossilCheckout(t)
	fossilRun(t, bin, fossilDir, "close")

	ckDir := filepath.Join(t.TempDir(), "ck")
	if err := os.Mkdir(ckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ck, err := r.CreateCheckout(ckDir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ck.Close() })
	rid, _, err := ck.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := ck.Extract(rid, libfossil.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	current := time.Now().Truncate(time.Second).Add(time.Hour)
	setMTime(t, filepath.Join(ckDir, "a.txt"), current)
	setMTime(t, filepath.Join(ckDir, "b.txt"), agedTime)
	if got := statusLines(t, ck); len(got) != 0 {
		t.Fatalf("fresh checkout: Status = %q, want none", got)
	}
	if m := vfileMTimes(t, ckDir)["a.txt"]; m != 0 {
		t.Fatalf("a.txt recorded with mtime %d, want 0", m)
	}

	writeFile(t, filepath.Join(ckDir, "a.txt"), "ONE\n")
	setMTime(t, filepath.Join(ckDir, "a.txt"), current)
	if got := strings.Join(fossilEdited(t, bin, ckDir), ","); got != "a.txt" {
		t.Errorf("fossil edited = %q, want %q", got, "a.txt")
	}
	if got := strings.Join(libfossilEdited(t, ck), ","); got != "a.txt" {
		t.Errorf("go-libfossil edited = %q, want %q", got, "a.txt")
	}
}
