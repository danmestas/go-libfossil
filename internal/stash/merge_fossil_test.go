//go:build !js

package stash_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/stash"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// mergeFixture is one repository with two checkouts of trunk, each holding
// the same pending merge of branch br and the same local edit: fossil
// stashes in ckF, go-libfossil in ckG.
type mergeFixture struct {
	bin, repoPath, ckF, ckG string
	env                     *simio.Env
}

func writeIn(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newMergeFixture builds the fixture. On br, b.txt is replaced (merged in
// as a clean take, UPDATED_BY_MERGE) and m.txt's second line edited; trunk
// edits m.txt's last line, so m.txt merges three ways. With addOnBranch, br
// also adds c.txt (ADDED_BY_MERGE).
func newMergeFixture(t *testing.T, addOnBranch bool) mergeFixture {
	t.Helper()
	f := mergeFixture{bin: testutil.RequireFossilBin(t)}
	f.env = &simio.Env{
		Clock:   simio.NewSimClockAt(time.Now().Add(time.Hour)),
		Rand:    simio.CryptoRand{},
		Storage: simio.OSStorage{},
	}
	dir := t.TempDir()
	f.repoPath = filepath.Join(dir, "r.fossil")
	f.ckF, f.ckG = filepath.Join(dir, "ckf"), filepath.Join(dir, "ckg")
	for _, ck := range []string{f.ckF, f.ckG} {
		if err := os.Mkdir(ck, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fossilIn(t, f.bin, dir, "init", f.repoPath)
	fossilIn(t, f.bin, f.ckF, "open", f.repoPath)
	writeIn(t, f.ckF, "a.txt", "a\n")
	writeIn(t, f.ckF, "b.txt", "b\n")
	writeIn(t, f.ckF, "m.txt", "1\n2\n3\n4\n5\n")
	fossilIn(t, f.bin, f.ckF, "add", "a.txt", "b.txt", "m.txt")
	fossilIn(t, f.bin, f.ckF, "commit", "-m", "base", "--no-warnings")
	fossilIn(t, f.bin, f.ckF, "branch", "new", "br", "trunk")
	fossilIn(t, f.bin, f.ckF, "update", "br")
	writeIn(t, f.ckF, "b.txt", "branch b\n")
	writeIn(t, f.ckF, "m.txt", "1\nBR\n3\n4\n5\n")
	if addOnBranch {
		writeIn(t, f.ckF, "c.txt", "added on br\n")
		fossilIn(t, f.bin, f.ckF, "add", "c.txt")
	}
	fossilIn(t, f.bin, f.ckF, "commit", "-m", "on br", "--no-warnings")
	fossilIn(t, f.bin, f.ckF, "update", "trunk")
	writeIn(t, f.ckF, "m.txt", "1\n2\n3\n4\nTRUNK\n")
	fossilIn(t, f.bin, f.ckF, "commit", "-m", "on trunk", "--no-warnings")
	fossilIn(t, f.bin, f.ckG, "open", f.repoPath, "trunk")
	for _, ck := range []string{f.ckF, f.ckG} {
		fossilIn(t, f.bin, ck, "merge", "br")
		writeIn(t, ck, "a.txt", "local edit\n")
	}
	return f
}

// query renders rows of a checkout database query as text, one row a line.
func query(t *testing.T, ck, q string) string {
	t.Helper()
	name, err := checkout.FindCheckoutDB(simio.OSStorage{}, ck, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := libdb.OpenSQL(name, libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			if i > 0 {
				b.WriteByte('|')
			}
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			fmt.Fprint(&b, v)
		}
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const (
	// A file's vmerge row names it by vfile id, which differs between
	// checkouts; it is shown by pathname.
	vmergeRows = "SELECT CASE WHEN id<=0 THEN id ELSE (SELECT pathname FROM vfile WHERE vfile.id=vmerge.id) END," +
		" merge, mhash FROM vmerge ORDER BY 1, 2"
	vfileRows = "SELECT pathname, rid, mrid, chnged, deleted FROM vfile ORDER BY pathname"
	stashRows = "SELECT stashid, CAST(isAdded AS INT), CAST(isRemoved AS INT), hash, origname, newname" +
		" FROM stashfile ORDER BY stashid, newname"
)

// worktree renders the checkout's tracked-or-fixture files as text.
func worktree(t *testing.T, ck string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "m.txt"} {
		data, err := os.ReadFile(filepath.Join(ck, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(&b, "%s: (none)\n", name)
		case err != nil:
			t.Fatal(err)
		default:
			fmt.Fprintf(&b, "%s: %q\n", name, data)
		}
	}
	return b.String()
}

// artifactOf is the hash fossil resolves name to.
func artifactOf(t *testing.T, bin, ck, name string) string {
	t.Helper()
	for _, line := range strings.Split(fossilIn(t, bin, ck, "whatis", name), "\n") {
		if hash, ok := strings.CutPrefix(line, "artifact:"); ok {
			return strings.TrimSpace(hash)
		}
	}
	t.Fatalf("fossil whatis %s: no artifact", name)
	return ""
}

var stashDate = regexp.MustCompile(` on \d{4}-\d\d-\d\d \d\d:\d\d:\d\d`)

// stashList is fossil's stash list for ck, without its times.
func stashList(t *testing.T, f mergeFixture, ck string) string {
	t.Helper()
	return stashDate.ReplaceAllString(fossilIn(t, f.bin, ck, "stash", "list"), "")
}

func requireSame(t *testing.T, what, fossil, golib string) {
	t.Helper()
	if fossil != golib {
		t.Fatalf("%s differs\nfossil:\n%s\ngo-libfossil:\n%s", what, fossil, golib)
	}
}

// requireSameCheckouts compares what fossil and go-libfossil left in their
// checkouts: vmerge, vfile, the stash, its listing and what fossil's scan
// reports, and with files the working files.
func requireSameCheckouts(t *testing.T, f mergeFixture, files bool) {
	t.Helper()
	for _, q := range []string{vmergeRows, vfileRows, stashRows} {
		requireSame(t, q, query(t, f.ckF, q), query(t, f.ckG, q))
	}
	requireSame(t, "stash list", stashList(t, f, f.ckF), stashList(t, f, f.ckG))
	if files {
		requireSame(t, "working files", worktree(t, f.ckF), worktree(t, f.ckG))
		requireSame(t, "fossil changes",
			fossilIn(t, f.bin, f.ckF, "changes"), fossilIn(t, f.bin, f.ckG, "changes"))
	}
}

// pCard is the P card of ck's checked-out version, as fossil writes it.
func pCard(t *testing.T, f mergeFixture, ck string) string {
	t.Helper()
	for _, line := range strings.Split(fossilIn(t, f.bin, ck, "artifact", "current"), "\n") {
		if strings.HasPrefix(line, "P ") {
			return line
		}
	}
	t.Fatalf("%s: no P card", ck)
	return ""
}

// Fossil stashes with a merge pending: it stashes each changed row against
// the content last written for it (mrid) and reverts just those files, so
// the merge stays pending (MERGED_WITH). The pop restores the files without
// touching vmerge, and the next commit records the merge parent.
func TestStashPendingMergeMatchesFossil(t *testing.T) {
	f := newMergeFixture(t, false)
	fossilIn(t, f.bin, f.ckF, "stash", "save", "-m", "merged")
	withDBs(t, f.ckG, f.repoPath, f.env, func(co *checkout.Checkout, ckout, repoDB *sql.DB) {
		if err := stash.Save(co, ckout, repoDB, f.env, f.ckG, "merged"); err != nil {
			t.Fatal(err)
		}
	})
	if query(t, f.ckG, vmergeRows) == "" {
		t.Fatal("save dropped the pending merge")
	}
	requireSameCheckouts(t, f, true)

	fossilIn(t, f.bin, f.ckF, "stash", "pop")
	withDBs(t, f.ckG, f.repoPath, f.env, func(co *checkout.Checkout, ckout, repoDB *sql.DB) {
		if _, err := stash.Pop(ckout, repoDB, f.env, f.ckG); err != nil {
			t.Fatal(err)
		}
	})
	for _, q := range []string{vmergeRows, stashRows} {
		requireSame(t, "after pop: "+q, query(t, f.ckF, q), query(t, f.ckG, q))
	}
	fossilIn(t, f.bin, f.ckF, "commit", "-m", "fossil popped", "--no-warnings")
	fossilIn(t, f.bin, f.ckG, "commit", "-m", "go popped", "--no-warnings", "--allow-fork")
	requireSame(t, "P card", pCard(t, f, f.ckF), pCard(t, f, f.ckG))
	if p := pCard(t, f, f.ckG); len(strings.Fields(p)) != 3 {
		t.Fatalf("commit after pop = %q, want the merge parent", p)
	}
}

// A file the merge added is not in the checked-out version, so the stash
// reverts it by deleting it, and its pop fails as fossil's does: the file is
// missing, so nothing is restored and the stash and merge are kept.
func TestStashMergeAddedFileMatchesFossil(t *testing.T) {
	f := newMergeFixture(t, true)
	fossilIn(t, f.bin, f.ckF, "stash", "save", "-m", "merged")
	withDBs(t, f.ckG, f.repoPath, f.env, func(co *checkout.Checkout, ckout, repoDB *sql.DB) {
		if err := stash.Save(co, ckout, repoDB, f.env, f.ckG, "merged"); err != nil {
			t.Fatal(err)
		}
	})
	requireSameCheckouts(t, f, true)

	cmd := exec.Command(f.bin, "stash", "pop")
	cmd.Dir = f.ckF
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "no such file") {
		t.Fatalf("fossil stash pop = %v\n%s", err, out)
	}
	withDBs(t, f.ckG, f.repoPath, f.env, func(co *checkout.Checkout, ckout, repoDB *sql.DB) {
		_, err := stash.Pop(ckout, repoDB, f.env, f.ckG)
		if err == nil || !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("stash.Pop = %v, want fossil's missing-file failure", err)
		}
	})
	requireSameCheckouts(t, f, true)
}

// A sparse checkout records a merge outside its cone without writing it
// (sparse_pending). Fossil has no such record; stashing an edit inside the
// cone must leave it, and vmerge, as they are, write nothing outside the
// cone, and the commit after the pop must still carry the merge.
func TestStashKeepsSparseRecordOnlyMerge(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	env := &simio.Env{
		Clock:   simio.NewSimClockAt(time.Now().Add(time.Hour)),
		Rand:    simio.CryptoRand{},
		Storage: simio.OSStorage{},
	}
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "r.fossil")
	ck := filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilIn(t, bin, dir, "init", repoPath)
	fossilIn(t, bin, ck, "open", repoPath)
	for _, sub := range []string{"src", "tools"} {
		if err := os.Mkdir(filepath.Join(ck, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeIn(t, ck, "src/a.c", "a\n")
	writeIn(t, ck, "tools/t.c", "t\n")
	fossilIn(t, bin, ck, "add", "src/a.c", "tools/t.c")
	fossilIn(t, bin, ck, "commit", "-m", "base", "--no-warnings")
	fossilIn(t, bin, ck, "branch", "new", "br", "trunk")
	fossilIn(t, bin, ck, "update", "br")
	writeIn(t, ck, "tools/t.c", "t on br\n")
	fossilIn(t, bin, ck, "commit", "-m", "on br", "--no-warnings")
	trunkUUID, brUUID := artifactOf(t, bin, ck, "trunk"), artifactOf(t, bin, ck, "br")
	fossilIn(t, bin, ck, "close")
	if err := os.RemoveAll(ck); err != nil {
		t.Fatal(err)
	}

	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var trunk, br libfossil.FslID
	for uuid, rid := range map[string]*libfossil.FslID{trunkUUID: &trunk, brUUID: &br} {
		if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(rid); err != nil {
			t.Fatal(err)
		}
	}
	src, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	co, err := checkout.Create(r, ck, checkout.CreateOpts{Env: env, Cone: src})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if err := co.Extract(trunk, checkout.ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(checkout.MergeOpts{Version: br}); err != nil {
		t.Fatal(err)
	}
	writeIn(t, ck, "src/a.c", "local edit\n")

	ckName, err := checkout.FindCheckoutDB(simio.OSStorage{}, ck, false)
	if err != nil {
		t.Fatal(err)
	}
	ckout, err := libdb.OpenSQL(ckName, libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ckout.Close()
	pending := "SELECT pathname, rid, mrid, chnged, deleted FROM sparse_pending ORDER BY pathname"
	before := query(t, ck, pending) + query(t, ck, vmergeRows)
	if !strings.Contains(before, "tools/t.c") {
		t.Fatalf("merge recorded no out-of-cone outcome:\n%s", before)
	}
	if err := stash.Save(co, ckout, r.DB().SqlDB(), env, ck, "sparse"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(ck, "src/a.c")); err != nil || string(got) != "a\n" {
		t.Fatalf("src/a.c after save = %q, %v", got, err)
	}
	requireSame(t, "record-only merge after save", before, query(t, ck, pending)+query(t, ck, vmergeRows))
	if _, err := stash.Pop(ckout, r.DB().SqlDB(), env, ck); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(ck, "src/a.c")); err != nil || string(got) != "local edit\n" {
		t.Fatalf("src/a.c after pop = %q, %v", got, err)
	}
	requireSame(t, "record-only merge after pop", before, query(t, ck, pending)+query(t, ck, vmergeRows))
	if _, err := os.Lstat(filepath.Join(ck, "tools")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stash wrote outside the cone: %v", err)
	}

	rid, _, err := co.Commit(checkout.CommitOpts{Message: "after pop", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := manifest.GetManifest(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.P) != 2 || d.P[1] != brUUID {
		t.Fatalf("commit after pop: P %q, want the merge parent %s", d.P, brUUID)
	}
}
