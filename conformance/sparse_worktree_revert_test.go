package conformance

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/stash"
	"github.com/danmestas/go-libfossil/simio"
)

// §4.4 for revert and stash: a record-only change -- what a merge applies to
// an out-of-cone file over a dense store -- is reverted by name and by revert
// with no names exactly as fossil reverts the same change in a dense
// checkout. Stash keeps to the cone: it stashes the in-cone files of a
// pending merge and leaves the merge and its record-only changes pending,
// round-trips an in-cone edit as fossil does, and refuses to apply an
// out-of-cone file, naming the cone.

func scRevertStashCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.4.4.revert-record-only-named", Req: "SC-4.4-1"}, scRevertRecordOnlyNamed},
		{Case{ID: "sparsecone.4.4.revert-record-only-all", Req: "SC-4.4-1"}, scRevertRecordOnlyAll},
		{Case{ID: "sparsecone.4.4.stash-record-only", Req: "SC-4.4-1"}, scStashRecordOnly},
		{Case{ID: "sparsecone.4.4.stash-apply", Req: "SC-4.4-1"}, scStashApplySparse},
		{Case{ID: "sparsecone.4.4.stash-apply-outside", Req: "SC-4.4-1"}, scStashApplyOutside},
	}
}

// scRecordOnlyRemote is c1 changed outside scDefault by an edit, a deletion
// and an addition, and inside it by an edit.
func scRecordOnlyRemote() map[string]string {
	remote := make(map[string]string, len(scFiles)+1)
	for name, text := range scFiles {
		remote[name] = text
	}
	remote["docs/d.txt"] = "remote docs\n"
	remote["tools/new.txt"] = "remote addition\n"
	delete(remote, "tools/t.c")
	remote["src/a.c"] = "remote source\n"
	return remote
}

// scFossilDense runs steps with fossil in a dense checkout of c1 on a copy of
// f's repository, "$c1" and "$mid" standing for those versions, and returns
// the content of the check-in the steps made, nil when they made none. A
// step whose first argument is "!" must fail; "@write" name text writes a
// working file.
func scFossilDense(t *testing.T, f *scRepo, mid libfossil.FslID, steps [][]string) map[string]string {
	t.Helper()
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("fossil(1) is this case's oracle")
	}
	if err := f.r.Checkpoint(db.CheckpointTruncate); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dense.fossil")
	if err := copyFile(f.r.Path(), path); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{"$c1": scUUID(t, f.r, f.c1), "$mid": scUUID(t, f.r, mid)}
	dir := t.TempDir()
	scRCardRun(t, ref, dir, "open", path, names["$c1"])
	for _, step := range steps {
		args := make([]string, 0, len(step))
		for _, a := range step {
			if v, ok := names[a]; ok {
				a = v
			}
			args = append(args, a)
		}
		if args[0] == "@write" {
			scWrite(t, dir, args[1], args[2])
			continue
		}
		if args[0] == "!" {
			if out, err := ref.run(dir, args[1:]...); err == nil {
				t.Fatalf("fossil %v succeeded: %s", args[1:], out)
			}
			continue
		}
		scRCardRun(t, ref, dir, args...)
	}
	scRCardRun(t, ref, dir, "close", "--force")
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var tip libfossil.FslID
	if err := r.DB().QueryRow("SELECT objid FROM event WHERE type='ci' ORDER BY mtime DESC LIMIT 1").
		Scan(&tip); err != nil {
		t.Fatal(err)
	}
	if uuid := scUUID(t, r, tip); uuid == names["$c1"] || uuid == names["$mid"] {
		return nil
	}
	got, err := scContent(r, tip)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func scUUID(t *testing.T, r *repo.Repo, rid libfossil.FslID) string {
	t.Helper()
	var uuid string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	return uuid
}

func scRevertRecordOnlyNamed(t *testing.T) error {
	f := newSCRepo(t, false)
	mid := f.commit(t, scRecordOnlyRemote(), f.c1)
	named := []string{"docs/d.txt", "tools/t.c", "tools/new.txt"}
	want := scFossilDense(t, f, mid, [][]string{
		{"merge", "$mid"}, append([]string{"revert"}, named...),
		{"commit", "--nosync", "--no-warnings", "--allow-fork", "-m", "partly reverted merge"},
	})
	co := f.open(t, scDefault(t), f.c1)
	if _, err := co.Merge(checkout.MergeOpts{Version: mid}); err != nil {
		return err
	}
	if err := co.Revert(checkout.RevertOpts{Paths: named}); err != nil {
		return err
	}
	if got := scOnDisk(t, co.Dir()); !slices.Equal(got, scDefaultCovered) {
		return expectf("named revert of record-only changes materialized %q", got)
	}
	rid, _, err := co.Commit(checkout.CommitOpts{Message: "partly reverted merge", User: "u", Time: f.when.Add(time.Hour)})
	if err != nil {
		return err
	}
	got, err := scContent(f.r, rid)
	if err != nil {
		return err
	}
	return scSameContent("named revert of record-only changes", got, want)
}

func scRevertRecordOnlyAll(t *testing.T) error {
	f := newSCRepo(t, false)
	mid := f.commit(t, scRecordOnlyRemote(), f.c1)
	want := scFossilDense(t, f, mid, [][]string{
		{"merge", "$mid"}, {"revert"},
		{"!", "commit", "--nosync", "--no-warnings", "--allow-fork", "-m", "nothing"},
	})
	if want != nil {
		return expectf("fossil committed after revert-all: %q", want)
	}
	co := f.open(t, scDefault(t), f.c1)
	if _, err := co.Merge(checkout.MergeOpts{Version: mid}); err != nil {
		return err
	}
	if err := co.Revert(checkout.RevertOpts{}); err != nil {
		return err
	}
	_, _, err := co.Commit(checkout.CommitOpts{Message: "nothing", User: "u", Time: f.when.Add(time.Hour)})
	if !errors.Is(err, checkout.ErrNothingToCommit) {
		return expectf("commit after revert-all of a record-only merge = %v, want nothing to commit", err)
	}
	if got := scOnDisk(t, co.Dir()); !slices.Equal(got, scDefaultCovered) {
		return expectf("revert-all materialized %q", got)
	}
	return scSameDisk(t, co.Dir(), scFiles)
}

// scSameDisk checks every file on disk under dir holds want's content.
func scSameDisk(t *testing.T, dir string, want map[string]string) error {
	t.Helper()
	for _, name := range scOnDisk(t, dir) {
		if got := scDiskText(t, dir, name); got != want[name] {
			return expectf("%s on disk = %q, want %q", name, got, want[name])
		}
	}
	return nil
}

func scDiskText(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// scStashDB opens a second connection to co's checkout database, as the
// stash functions take one.
func scStashDB(t *testing.T, co *checkout.Checkout) *sql.DB {
	t.Helper()
	path, err := checkout.FindCheckoutDB(simio.OSStorage{}, co.Dir(), false)
	if err != nil {
		t.Fatal(err)
	}
	ckdb, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ckdb.Close() })
	return ckdb
}

// scStashRecordOnly stashes a pending merge. Dense, it is fossil's stash:
// the merge stays pending, and the file the merge added is reverted away, so
// the pop fails as fossil's does. Sparse, the merged in-cone file round-trips
// while the record-only changes stay pending, unmaterialized, and the commit
// after the pop carries the whole merge.
func scStashRecordOnly(t *testing.T) error {
	for _, c := range []bool{false, true} {
		f := newSCRepo(t, false)
		mid := f.commit(t, scRecordOnlyRemote(), f.c1)
		cone := scDefault(t)
		if !c {
			cone = nil
		}
		co := f.open(t, cone, f.c1)
		if _, err := co.Merge(checkout.MergeOpts{Version: mid}); err != nil {
			return err
		}
		before := scOnDisk(t, co.Dir())
		ckdb := scStashDB(t, co)
		repoDB := f.r.DB().SqlDB()
		if err := stash.Save(co, ckdb, repoDB, simio.RealEnv(), co.Dir(), "record-only"); err != nil {
			return err
		}
		var merges int
		if err := ckdb.QueryRow("SELECT count(*) FROM vmerge").Scan(&merges); err != nil {
			return err
		}
		if merges == 0 {
			return expectf("stash dropped the pending merge (sparse=%v)", c)
		}
		_, err := stash.Pop(ckdb, repoDB, simio.RealEnv(), co.Dir())
		if !c {
			if err == nil || !strings.Contains(err.Error(), "no such file") {
				return expectf("dense pop of a merge-added file = %v, want fossil's missing-file failure", err)
			}
			continue
		}
		if err != nil {
			return err
		}
		if got := scOnDisk(t, co.Dir()); !slices.Equal(got, before) {
			return expectf("stash round trip changed the disk: %q -> %q", before, got)
		}
		rid, _, err := co.Commit(checkout.CommitOpts{Message: "merge", User: "u", Time: f.when.Add(time.Hour)})
		if err != nil {
			return err
		}
		got, err := scContent(f.r, rid)
		if err != nil {
			return err
		}
		if err := scSameContent("merge after a stash round trip", got, scRecordOnlyRemote()); err != nil {
			return err
		}
		d, err := manifest.GetManifest(f.r, rid)
		if err != nil {
			return err
		}
		if len(d.P) != 2 {
			return expectf("commit after a stash round trip has parents %q, want the merge", d.P)
		}
	}
	return nil
}

func scStashApplySparse(t *testing.T) error {
	f := newSCRepo(t, false)
	edited := map[string]string{}
	for name, text := range scFiles {
		edited[name] = text
	}
	edited["src/a.c"] = "stashed edit\n"
	want := scFossilDense(t, f, f.c1, [][]string{
		{"@write", "src/a.c", "stashed edit\n"}, {"stash", "save", "-m", "s"}, {"stash", "pop"},
		{"commit", "--nosync", "--no-warnings", "--allow-fork", "-m", "applied"},
	})
	if err := scSameContent("fossil stash round trip", want, edited); err != nil {
		return err
	}
	co := f.open(t, scDefault(t), f.c1)
	scWrite(t, co.Dir(), "src/a.c", "stashed edit\n")
	ckdb := scStashDB(t, co)
	if err := stash.Save(co, ckdb, f.r.DB().SqlDB(), simio.RealEnv(), co.Dir(), "s"); err != nil {
		return err
	}
	if got := scDiskText(t, co.Dir(), "src/a.c"); got != scFiles["src/a.c"] {
		return expectf("stash save left src/a.c = %q", got)
	}
	if _, err := stash.Pop(ckdb, f.r.DB().SqlDB(), simio.RealEnv(), co.Dir()); err != nil {
		return err
	}
	if got := scOnDisk(t, co.Dir()); !slices.Equal(got, scDefaultCovered) {
		return expectf("stash round trip materialized %q", got)
	}
	rid, _, err := co.Commit(checkout.CommitOpts{Message: "applied", User: "u", Time: f.when.Add(time.Hour)})
	if err != nil {
		return err
	}
	got, err := scContent(f.r, rid)
	if err != nil {
		return err
	}
	return scSameContent("stash round trip on a sparse checkout", got, edited)
}

func scStashApplyOutside(t *testing.T) error {
	f := newSCRepo(t, false)
	co := f.open(t, nil, f.c1)
	scWrite(t, co.Dir(), "tools/t.c", "outside edit\n")
	ckdb := scStashDB(t, co)
	if err := stash.Save(co, ckdb, f.r.DB().SqlDB(), simio.RealEnv(), co.Dir(), "s"); err != nil {
		return err
	}
	if err := ckdb.Close(); err != nil {
		return err
	}
	if _, err := co.SetCone(scDefault(t)); err != nil {
		return err
	}
	ckdb = scStashDB(t, co)
	_, err := stash.Pop(ckdb, f.r.DB().SqlDB(), simio.RealEnv(), co.Dir())
	if !errors.Is(err, checkout.ErrOutsideCone) || !strings.Contains(err.Error(), "cone") {
		return expectf("applying an out-of-cone stash = %v, want a refusal naming the cone", err)
	}
	if got := scOnDisk(t, co.Dir()); !slices.Equal(got, scDefaultCovered) {
		return expectf("refused stash apply materialized %q", got)
	}
	entries, err := stash.List(ckdb)
	if err != nil {
		return err
	}
	if len(entries) != 1 {
		return expectf("refused stash pop dropped the entry: %d left", len(entries))
	}
	return nil
}
