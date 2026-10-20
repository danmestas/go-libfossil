package checkout

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// These tests drive the sparse worktree of draft-fossil-sparse-cone-00 §4:
// a checkout with a cone materializes and tracks only the covered files,
// commits carry the uncovered ones from the baseline unchanged, and every
// operation that would act on an uncovered path refuses by naming the cone.

// sparseFiles is the fixture's first check-in.
var sparseFiles = map[string]string{
	"README":               "readme\n",
	"docs/d.txt":           "docs\n",
	"src/a.c":              "a\n",
	"src/b.c":              "b\n",
	"src/vendor/v.c":       "vendor\n",
	"src/vendor/keep/k.c":  "keep\n",
	"src.txt":              "sibling\n",
	"tools/t.c":            "tools\n",
	"tools/deep/x/y/z.txt": "deep\n",
}

// sparseCone covers src, carves out src/vendor and keeps src/vendor/keep.
func sparseCone(t *testing.T) *cone.Cone {
	t.Helper()
	c, err := cone.New([]string{"src", "src/vendor/keep"}, []string{"src/vendor"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var sparseCovered = []string{"src/a.c", "src/b.c", "src/vendor/keep/k.c"}

type sparseFixture struct {
	repo *repo.Repo
	c1   libfossil.FslID
	time time.Time
}

// newSparseRepo commits sparseFiles, as a tree manifest when tree is set.
func newSparseRepo(t *testing.T, tree bool) *sparseFixture {
	t.Helper()
	r, err := repo.CreateWithEnv(t.TempDir()+"/sparse.fossil", "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if tree {
		for k, v := range map[string]string{
			manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
		} {
			if err := r.SetConfig(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := &sparseFixture{repo: r, time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	f.c1 = f.commit(t, sparseFiles, 0)
	d, err := manifest.GetManifest(r, f.c1)
	if err != nil {
		t.Fatal(err)
	}
	if tree != (len(d.S) > 0) {
		t.Fatalf("tree=%v but the check-in carries %d S cards", tree, len(d.S))
	}
	return f
}

func (f *sparseFixture) commit(t *testing.T, files map[string]string, parent libfossil.FslID) libfossil.FslID {
	t.Helper()
	var mf []manifest.File
	for name, text := range files {
		mf = append(mf, manifest.File{Name: name, Content: []byte(text)})
	}
	sort.Slice(mf, func(i, j int) bool { return mf[i].Name < mf[j].Name })
	f.time = f.time.Add(time.Hour)
	rid, _, err := manifest.Checkin(f.repo, manifest.CheckinOpts{
		Files: mf, Comment: "c", User: "test", Parent: parent, Time: f.time,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

// open creates a checkout with cone c (nil for dense) and extracts rid.
func (f *sparseFixture) open(t *testing.T, c *cone.Cone, rid libfossil.FslID) *Checkout {
	t.Helper()
	co, err := Create(f.repo, t.TempDir(), CreateOpts{Cone: c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	return co
}

// onDisk lists the regular files under the checkout, the checkout database
// and its journal files excluded.
func onDisk(t *testing.T, co *Checkout) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(co.Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(co.Dir(), path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(rel, PreferredDBName()) {
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

func writeFile(t *testing.T, co *Checkout, name, text string) {
	t.Helper()
	path := filepath.Join(co.Dir(), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func vfileCount(t *testing.T, co *Checkout) int {
	t.Helper()
	var n int
	if err := co.db.QueryRow("SELECT count(*) FROM vfile").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// §4.1: a checkout given a cone materializes exactly the covered files,
// over flat and tree manifests; the record keeps every file, and the cone
// survives a reopen.
func TestSparseMaterializesExactlyTheCone(t *testing.T) {
	for _, tree := range []bool{false, true} {
		f := newSparseRepo(t, tree)
		co := f.open(t, sparseCone(t), f.c1)
		if got := onDisk(t, co); !slices.Equal(got, sparseCovered) {
			t.Fatalf("tree=%v: on disk %q, want %q", tree, got, sparseCovered)
		}
		if n := vfileCount(t, co); n != len(sparseFiles) {
			t.Fatalf("tree=%v: vfile holds %d rows, want the full record of %d", tree, n, len(sparseFiles))
		}
		reopened, err := Open(f.repo, co.Dir(), OpenOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if got := reopened.Cone(); got == nil || !got.Equal(sparseCone(t)) {
			t.Fatalf("tree=%v: reopened cone %v", tree, got)
		}
		reopened.Close()
	}
}

func TestSparseOpenRefusesCorruptCone(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	if err := setVVar(co.db, coneVVar, "02zz"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.repo, co.Dir(), OpenOpts{}); err == nil || !strings.Contains(err.Error(), "cone") {
		t.Fatalf("Open with a corrupt cone: %v", err)
	}
}

// §4.1: status works within the cone. An uncovered path is neither missing
// nor extra, a stray uncovered file on disk is not reported, and an
// in-cone edit is.
func TestSparseStatusStaysInCone(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	if has, err := co.HasChanges(); err != nil || has {
		t.Fatalf("HasChanges = %v, %v on a fresh sparse checkout", has, err)
	}
	if got := changedNames(t, co); len(got) != 0 {
		t.Fatalf("fresh sparse checkout reports %q", got)
	}
	writeFile(t, co, "tools/stray.c", "stray\n")
	writeFile(t, co, "src/vendor/v.c", "materialized by hand\n")
	writeFile(t, co, "src/a.c", "edited\n")
	extras := -1
	co.obs = &testObserver{onScanCompleted: func(_ context.Context, e ScanEnd) { extras = e.FilesExtra }}
	if got := changedNames(t, co); !slices.Equal(got, []string{"src/a.c"}) {
		t.Fatalf("changes = %q, want only src/a.c", got)
	}
	if extras != 0 {
		t.Fatalf("scan counted %d extra files, want 0: uncovered paths are not untracked", extras)
	}
}

// §4.1: tracking requests outside the cone are refused, naming the cone.
func TestSparseRefusesTrackingOutsideCone(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	writeFile(t, co, "tools/new.c", "new\n")
	writeFile(t, co, "src/vendor/new.c", "new\n")
	writeFile(t, co, "src/new.c", "new\n")
	for _, name := range []string{"tools/new.c", "src/vendor/new.c", "tools/t.c"} {
		_, err := co.Manage(ManageOpts{Paths: []string{name}})
		assertOutsideCone(t, err, name)
	}
	if counts, err := co.Manage(ManageOpts{Paths: []string{"src/new.c"}}); err != nil || counts.Added != 1 {
		t.Fatalf("Manage(src/new.c) = %+v, %v", counts, err)
	}
	assertOutsideCone(t, co.Unmanage(UnmanageOpts{Paths: []string{"tools/t.c"}}), "tools/t.c")
	assertOutsideCone(t, co.Rename(RenameOpts{From: "src/a.c", To: "tools/a.c", DoFsMove: true}), "tools/a.c")
	assertOutsideCone(t, co.Rename(RenameOpts{From: "tools/t.c", To: "src/t.c"}), "tools/t.c")
	if err := co.Revert(RevertOpts{Paths: []string{"tools/t.c"}}); err != nil {
		t.Fatalf("record-only revert: %v", err)
	}
	if err := co.Enqueue(EnqueueOpts{Paths: []string{"tools"}}); err != nil {
		t.Fatal(err)
	}
	_, _, err := co.Commit(CommitOpts{Message: "m", User: "u"})
	assertOutsideCone(t, err, "tools")
}

func assertOutsideCone(t *testing.T, err error, path string) {
	t.Helper()
	if !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("%s: error %v, want ErrOutsideCone", path, err)
	}
	for _, want := range []string{path, "src, !src/vendor, src/vendor/keep"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: error %q does not name %q", path, err, want)
		}
	}
}

// §4.2: a sparse commit's file set is the in-cone files as observed plus the
// baseline's out-of-cone entries unchanged, and it is the very check-in a
// dense checkout makes of the same change: a reader cannot tell them apart.
func TestSparseCommitIsTheDenseCommit(t *testing.T) {
	for _, tree := range []bool{false, true} {
		var uuids []string
		var sparse *Checkout
		for _, c := range []*cone.Cone{sparseCone(t), nil} {
			// One repository each: the two commits are the same artifact.
			f := newSparseRepo(t, tree)
			co := f.open(t, c, f.c1)
			if c != nil {
				sparse = co
			}
			writeFile(t, co, "src/a.c", "a edited\n")
			writeFile(t, co, "src/vendor/keep/new.c", "new\n")
			if _, err := co.Manage(ManageOpts{Paths: []string{"src/vendor/keep/new.c"}}); err != nil {
				t.Fatal(err)
			}
			if err := co.Unmanage(UnmanageOpts{Paths: []string{"src/b.c"}}); err != nil {
				t.Fatal(err)
			}
			rid, uuid, err := co.Commit(CommitOpts{Message: "edit", User: "u", Time: f.time.Add(time.Hour)})
			if err != nil {
				t.Fatalf("tree=%v: Commit: %v", tree, err)
			}
			if err := f.repo.WithTx(func(tx *db.Tx) error { return manifest.VerifyRCard(tx, nil, rid) }); err != nil {
				t.Fatalf("tree=%v: R card: %v", tree, err)
			}
			uuids = append(uuids, uuid)
		}
		if uuids[0] != uuids[1] {
			t.Fatalf("tree=%v: sparse commit %s differs from the dense commit %s", tree, uuids[0], uuids[1])
		}
		if got := onDisk(t, sparse); slices.Contains(got, "tools/t.c") {
			t.Fatalf("tree=%v: the commit materialized out-of-cone files: %q", tree, got)
		}
	}
}

// §4.4: update applies out-of-cone changes to the record without
// materializing them, and reports how many it applied.
func TestSparseUpdateAppliesOutOfConeToRecord(t *testing.T) {
	f := newSparseRepo(t, false)
	next := maps(sparseFiles, map[string]string{
		"src/a.c": "a2\n", "tools/t.c": "tools2\n", "docs/new.txt": "new\n",
	}, "README")
	c2 := f.commit(t, next, f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	// An uncovered file standing where the target adds one is not the
	// update's business: it is neither backed up nor replaced.
	writeFile(t, co, "docs/new.txt", "stray\n")
	if err := os.Chmod(filepath.Join(co.Dir(), "docs/new.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := co.Update(UpdateOpts{TargetRID: c2})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(co.Dir(), "docs/new.txt")); err != nil || string(data) != "stray\n" {
		t.Fatalf("update touched an uncovered stray file: %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(co.Dir(), "docs/new.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("update changed an uncovered file's mode: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), "docs/new.txt-original")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update backed up an uncovered file: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(co.Dir(), "docs")); err != nil {
		t.Fatal(err)
	}
	// Undo saves only what the update changed on disk: no uncovered file.
	var saved []string
	rows, err := co.db.Query("SELECT pathname FROM undo ORDER BY pathname")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		saved = append(saved, name)
	}
	rows.Close()
	if !slices.Equal(saved, []string{"src/a.c"}) {
		t.Fatalf("undo saved %q, want only src/a.c", saved)
	}
	if !slices.Equal(res.FilesWritten, []string{"src/a.c"}) || len(res.FilesRemoved) != 0 {
		t.Fatalf("Update wrote %q, removed %q", res.FilesWritten, res.FilesRemoved)
	}
	if res.OutOfCone != 3 {
		t.Fatalf("Update applied %d out-of-cone changes, want 3", res.OutOfCone)
	}
	if got := onDisk(t, co); !slices.Equal(got, sparseCovered) {
		t.Fatalf("on disk %q after update", got)
	}
	writeFile(t, co, "src/b.c", "b2\n")
	rid, _, err := co.Commit(CommitOpts{Message: "m", User: "u", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	want := maps(next, map[string]string{"src/b.c": "b2\n"})
	assertCheckinContent(t, f.repo, rid, want)
}

// maps returns base overlaid with set, minus the names in drop.
func maps(base, set map[string]string, drop ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range set {
		out[k] = v
	}
	for _, k := range drop {
		delete(out, k)
	}
	return out
}

func assertCheckinContent(t *testing.T, r *repo.Repo, rid libfossil.FslID, want map[string]string) {
	t.Helper()
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range files {
		frid, ok := content.AvailableByUUID(r.DB(), file.UUID)
		if !ok {
			t.Fatalf("%s: content missing", file.Name)
		}
		data, err := content.Expand(r.DB(), frid)
		if err != nil {
			t.Fatal(err)
		}
		got[file.Name] = string(data)
	}
	if len(got) != len(want) {
		t.Fatalf("check-in holds %d files, want %d", len(got), len(want))
	}
	for name, text := range want {
		if got[name] != text {
			t.Fatalf("%s = %q, want %q", name, got[name], text)
		}
	}
}

// §4.4: an uncovered merge changes the record, never the working tree.
func TestSparseMergeAcrossConeRecordsChanges(t *testing.T) {
	f := newSparseRepo(t, false)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "theirs\n"}), f.c1)
	ours := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "ours\n"}), f.c1)
	co := f.open(t, sparseCone(t), ours)
	_, err := co.Merge(MergeOpts{Version: theirs})
	if err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, co); !slices.Equal(got, sparseCovered) {
		t.Fatalf("record-only merge touched disk: %q", got)
	}
	var n int
	if err := co.db.QueryRow("SELECT count(*) FROM vmerge WHERE id=0").Scan(&n); err != nil || n != 1 {
		t.Fatalf("record-only merge left %d merge parents (%v)", n, err)
	}

	inCone := f.commit(t, maps(sparseFiles, map[string]string{"src/b.c": "theirs\n"}), f.c1)
	res, err := co.Merge(MergeOpts{Version: inCone})
	if err != nil || len(res.Files) != 1 || res.Files[0].Name != "src/b.c" {
		t.Fatalf("in-cone merge = %+v, %v", res, err)
	}
}

// §3.5: widening materializes the newly covered files, narrowing removes
// the newly uncovered ones, and the cone survives both.
func TestSetConeWidensAndNarrows(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	wider, err := sparseCone(t).With([]string{"tools"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := co.SetCone(wider)
	if err != nil {
		t.Fatal(err)
	}
	wantAdded := []string{"tools/deep/x/y/z.txt", "tools/t.c"}
	if !slices.Equal(res.Materialized, wantAdded) || len(res.Dematerialized) != 0 {
		t.Fatalf("widen: %+v", res)
	}
	if got := onDisk(t, co); !slices.Equal(got, append(slices.Clone(sparseCovered), wantAdded...)) {
		t.Fatalf("on disk after widening: %q", got)
	}

	narrower, err := cone.New([]string{"src/a.c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = co.SetCone(narrower)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"src/b.c", "src/vendor/keep/k.c", "tools/deep/x/y/z.txt", "tools/t.c"}
	if !slices.Equal(res.Dematerialized, want) || len(res.Materialized) != 0 {
		t.Fatalf("narrow: %+v", res)
	}
	if got := onDisk(t, co); !slices.Equal(got, []string{"src/a.c"}) {
		t.Fatalf("on disk after narrowing: %q", got)
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), "tools")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("narrowing left the emptied tools directory: %v", err)
	}
	if !co.Cone().Equal(narrower) {
		t.Fatalf("cone after narrowing: %v", co.Cone())
	}

	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if co.Cone() != nil || len(onDisk(t, co)) != len(sparseFiles) {
		t.Fatalf("dense again: cone %v, on disk %q", co.Cone(), onDisk(t, co))
	}
}

// Narrowing never discards work: a file with local changes that would leave
// the cone is refused by name, and so is a widening that would overwrite an
// untracked file with different content.
func TestSetConeProtectsWork(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	writeFile(t, co, "src/b.c", "edited\n")
	narrower, err := cone.New([]string{"src/a.c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(narrower); err == nil || !strings.Contains(err.Error(), "src/b.c") {
		t.Fatalf("narrowing over an edit: %v", err)
	}
	if !co.Cone().Equal(sparseCone(t)) {
		t.Fatal("a refused narrowing changed the cone")
	}
	writeFile(t, co, "tools/t.c", "not the committed content\n")
	wider, err := sparseCone(t).With([]string{"tools"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(wider); err == nil || !strings.Contains(err.Error(), "tools/t.c") {
		t.Fatalf("widening over an untracked file: %v", err)
	}
}

// A local edit the target's rename would carry out of the cone cannot be
// represented, so the update is refused, naming the cone, and nothing moves.
func TestSparseUpdateRefusesRenameOutOfCone(t *testing.T) {
	f := newSparseRepo(t, false)
	c2 := f.commitRenames(t, map[string]string{"src/a.c": "tools/a.c"})
	co := f.open(t, sparseCone(t), f.c1)
	writeFile(t, co, "src/a.c", "edited\n")
	_, err := co.Update(UpdateOpts{TargetRID: c2})
	assertOutsideCone(t, err, "tools/a.c")
	if v, _, _ := co.Version(); v != f.c1 {
		t.Fatalf("a refused update moved the checkout to %d", v)
	}
}

// commitRenames commits sparseFiles with each old name of renames moved to
// its new one, recorded as a rename, as a child of c1.
func (f *sparseFixture) commitRenames(t *testing.T, renames map[string]string) libfossil.FslID {
	t.Helper()
	var mf []manifest.File
	for name, text := range sparseFiles {
		file := manifest.File{Name: name, Content: []byte(text)}
		if to, ok := renames[name]; ok {
			file.Name, file.OldName = to, name
		}
		mf = append(mf, file)
	}
	sort.Slice(mf, func(i, j int) bool { return mf[i].Name < mf[j].Name })
	f.time = f.time.Add(time.Hour)
	rid, _, err := manifest.Checkin(f.repo, manifest.CheckinOpts{
		Files: mf, Comment: "rename", User: "test", Parent: f.c1, Time: f.time,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

// §4.4 with renames: a rename outside the cone is the record's business
// only; one into the cone materializes the file under its new name; an
// unedited file renamed out of the cone leaves the disk and stays in the
// record. None of them blocks the update.
func TestSparseUpdateFollowsRenamesAcrossTheCone(t *testing.T) {
	f := newSparseRepo(t, false)
	c2 := f.commitRenames(t, map[string]string{
		"tools/t.c":  "tools/t2.c", // outside to outside
		"docs/d.txt": "src/d.txt",  // into the cone
		"src/b.c":    "tools/b.c",  // out of the cone, unedited
	})
	co := f.open(t, sparseCone(t), f.c1)
	res, err := co.Update(UpdateOpts{TargetRID: c2})
	if err != nil {
		t.Fatalf("Update across renames: %v", err)
	}
	if !slices.Equal(res.FilesWritten, []string{"src/d.txt"}) ||
		!slices.Equal(res.FilesRemoved, []string{"src/b.c"}) || res.OutOfCone != 4 {
		t.Fatalf("Update wrote %q, removed %q, recorded %d out of the cone",
			res.FilesWritten, res.FilesRemoved, res.OutOfCone)
	}
	want := []string{"src/a.c", "src/d.txt", "src/vendor/keep/k.c"}
	if got := onDisk(t, co); !slices.Equal(got, want) {
		t.Fatalf("on disk %q, want %q", got, want)
	}
	if has, err := co.HasChanges(); err != nil || has {
		t.Fatalf("HasChanges after the update = %v, %v", has, err)
	}
	writeFile(t, co, "src/a.c", "a2\n")
	rid, _, err := co.Commit(CommitOpts{Message: "m", User: "u", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	next := maps(sparseFiles, map[string]string{
		"tools/t2.c": "tools\n", "src/d.txt": "docs\n", "tools/b.c": "b\n", "src/a.c": "a2\n",
	}, "tools/t.c", "docs/d.txt", "src/b.c")
	assertCheckinContent(t, f.repo, rid, next)
}
