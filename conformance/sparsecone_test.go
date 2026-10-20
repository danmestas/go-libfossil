package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/simio"
)

// The sparsecone suite checks go-libfossil's cone specification and sparse
// worktree against draft-fossil-sparse-cone-00 §3 and §4. Cases build a
// repository, open a checkout with a cone through internal/checkout -- the
// layer every public entry point goes through -- and look at the disk, the
// checkout database and the check-ins it writes. Store classification and
// explicit compaction have additional cases; remaining subtree and sync gaps
// are recorded in the table. zit is the oracle for the serialized form and
// what a cone materializes: sparsecone_zit_test.go replays its recorded answers.

type scCase struct {
	Case
	run func(t *testing.T) error
}

func sparseconeSuiteCases() []Case {
	var all []Case
	for _, c := range sparseconeCases() {
		all = append(all, c.Case)
	}
	return all
}

func TestSparseCone(t *testing.T) {
	for _, c := range sparseconeCases() {
		check(t, c.Case, func() error { return c.run(t) })
	}
}

func sparseconeCases() []scCase {
	var all []scCase
	for _, group := range [][]scCase{
		scSpecCases(), scWorktreeCases(), scCommitCases(), scZitReplayCases(),
		scStoreCases(), scCompactCases(), scSyncCases(), scCanonicalCases(), scProtocolBoundsCases(),
		scWorktreeCompleteCases(), scRecoveryCases(),
	} {
		all = append(all, group...)
	}
	return all
}

// scFiles is the suite's first check-in.
var scFiles = map[string]string{
	"README":              "readme\n",
	"docs/d.txt":          "docs\n",
	"src/a.c":             "a\n",
	"src/b.c":             "b\n",
	"src/vendor/v.c":      "vendor\n",
	"src/vendor/keep/k.c": "keep\n",
	"src.txt":             "sibling\n",
	"src-x/s.c":           "dash sibling\n",
	"tools/t.c":           "tools\n",
	"tools/x/y/z.txt":     "deep\n",
}

// scRepo holds scFiles as one check-in, a tree manifest when tree is set.
type scRepo struct {
	r    *repo.Repo
	c1   libfossil.FslID
	when time.Time
}

func newSCRepo(t *testing.T, tree bool) *scRepo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "sc.fossil"), "u", simio.CryptoRand{}, "")
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
	f := &scRepo{r: r, when: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	f.c1 = f.commit(t, scFiles, 0)
	return f
}

func (f *scRepo) commit(t *testing.T, files map[string]string, parent libfossil.FslID) libfossil.FslID {
	t.Helper()
	var mf []manifest.File
	for name, text := range files {
		mf = append(mf, manifest.File{Name: name, Content: []byte(text)})
	}
	sort.Slice(mf, func(i, j int) bool { return mf[i].Name < mf[j].Name })
	f.when = f.when.Add(time.Hour)
	rid, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{
		Files: mf, Comment: "c", User: "u", Parent: parent, Time: f.when,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

// open creates a checkout with cone c (nil for dense) and extracts rid.
func (f *scRepo) open(t *testing.T, c *cone.Cone, rid libfossil.FslID) *checkout.Checkout {
	t.Helper()
	co, err := checkout.Create(f.r, t.TempDir(), checkout.CreateOpts{Cone: c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(rid, checkout.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	return co
}

func scCone(t *testing.T, include, exclude []string) *cone.Cone {
	t.Helper()
	c, err := cone.New(include, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// scDefault covers src, carves out src/vendor and keeps src/vendor/keep.
func scDefault(t *testing.T) *cone.Cone {
	return scCone(t, []string{"src", "src/vendor/keep"}, []string{"src/vendor"})
}

var scDefaultCovered = []string{"src/a.c", "src/b.c", "src/vendor/keep/k.c"}

// The fixture must count user files, not private checkout state or sidecars.
func TestSparseConeDiskNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		".libfossil-checkout", ".libfossil-checkout-wal",
		".libfossil-checkout-shm", ".libfossil-checkout-journal", "src/a.c",
	} {
		scWrite(t, dir, name, "fixture\n")
	}
	if err := scSameList("user files", scOnDisk(t, dir), []string{"src/a.c"}); err != nil {
		t.Fatal(err)
	}
}

// scOnDisk lists the files under dir, the checkout database and its
// journals excluded, and anything zit keeps there.
func scOnDisk(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, ".libfossil-checkout") ||
			strings.HasPrefix(rel, ".fslckout") || strings.HasPrefix(rel, "_FOSSIL_") ||
			strings.HasPrefix(rel, ".zit") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
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

func scWrite(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// scSameList reports a difference between two sorted lists.
func scSameList(what string, got, want []string) error {
	if !slices.Equal(got, want) {
		return expectf("%s %q, want %q", what, got, want)
	}
	return nil
}

// scErrIs reports err unless it wraps want.
func scErrIs(what string, err, want error) error {
	if !errors.Is(err, want) {
		return expectf("%s: error %v, want %v", what, err, want)
	}
	return nil
}

// --- §2, §3: the cone specification ---------------------------------------

func scSpecCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.2.covers", Req: "SC-2-1"}, scCovers},
		{Case{ID: "sparsecone.3.1.version", Req: "SC-3.1-3"}, scVersion},
		{Case{ID: "sparsecone.3.2.validate", Req: "SC-3.2-1"}, scValidate},
		{Case{ID: "sparsecone.3.2.minimal", Req: "SC-3.2-2"}, scMinimal},
		{Case{ID: "sparsecone.3.2.capacity", Req: "SC-3.2-3"}, scCapacity},
		{Case{ID: "sparsecone.3.3.longest", Req: "SC-3.3-1"}, scLongest},
		{Case{ID: "sparsecone.3.3.reinclude", Req: "SC-3.3-2"}, scReinclude},
		{Case{ID: "sparsecone.3.3.redundant", Req: "SC-3.3-3"}, scRedundant},
		{Case{ID: "sparsecone.3.3.dead", Req: "SC-3.3-4"}, func(*testing.T) error {
			_, err := cone.New([]string{"src"}, []string{"tools"})
			return scErrIs("an exclusion nothing covers", err, cone.ErrDeadExclusion)
		}},
		{Case{ID: "sparsecone.3.3.contradiction", Req: "SC-3.3-5"}, func(*testing.T) error {
			_, err := cone.New([]string{"src", "tools"}, []string{"src"})
			return scErrIs("a path with both polarities", err, cone.ErrContradiction)
		}},
		{Case{ID: "sparsecone.3.3.ranges", Req: "SC-3.3-6"}, scRanges},
		{Case{ID: "sparsecone.3.4.literal", Req: "SC-3.4-1"}, scLiteral},
		{Case{ID: "sparsecone.3.4.metachar", Req: "SC-3.4-3"}, scLiteral},
		{Case{ID: "sparsecone.3.4.file", Req: "SC-3.4-2"}, scFileMember},
		{Case{ID: "sparsecone.3.5.amend", Req: "SC-3.5-1"}, scAmend},
		{Case{ID: "sparsecone.8.bounds", Req: "SC-8-1"}, scBounds},
	}
}

func scCovers(t *testing.T) error {
	c := scCone(t, []string{"src"}, nil)
	for path, want := range map[string]bool{
		"src": true, "src/a.c": true, "src/x/y": true,
		"src.txt": false, "src-x/s.c": false, "srcx": false, "s": false,
	} {
		if c.Covers(path) != want {
			return expectf("cone {src} covers %q = %v", path, !want)
		}
	}
	// The absent cone is dense: every file materializes.
	f := newSCRepo(t, false)
	return scSameList("a dense checkout materializes", scOnDisk(t, f.open(t, nil, f.c1).Dir()), keysOf(scFiles))
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func scVersion(t *testing.T) error {
	plain := scCone(t, []string{"src"}, nil).Serialize()
	carved := scDefault(t).Serialize()
	if plain[0] == 0 || carved[0] != 0 || carved[1] != 2 {
		return expectf("version markers: path-only %x, with exclusions %x", plain[:1], carved[:2])
	}
	for _, v := range []byte{0, 1, 3, 255} {
		forged := append([]byte{0, v}, carved[2:]...)
		if _, err := cone.Parse(forged); err == nil {
			return expectf("a cone of serialized version %d was read", v)
		}
	}
	return nil
}

func scValidate(t *testing.T) error {
	for _, m := range []string{"", ".", "..", "a/./b", "a/../b", "a//b", "a/", "/a", "a\\b", ".fslckout", "x/_FOSSIL_"} {
		if _, err := cone.New([]string{m}, nil); err == nil {
			return expectf("member %q was accepted", m)
		}
	}
	return nil
}

func scMinimal(t *testing.T) error {
	c := scCone(t, []string{"src/vendor", "src", "docs/x", "docs"}, []string{"src/vendor/keep", "src/vendor/keep/deeper"})
	// src/vendor is covered by src with no exclusion between: dropped. The
	// exclusion src/vendor/keep stays (its nearest cover is an inclusion);
	// src/vendor/keep/deeper is covered by it with the same polarity.
	if err := scSameList("inclusions", c.Include(), []string{"docs", "src"}); err != nil {
		return err
	}
	return scSameList("exclusions", c.Exclude(), []string{"src/vendor/keep"})
}

func scCapacity(t *testing.T) error {
	names := make([]string, cone.MaxMembers+1)
	for i := range names {
		names[i] = fmt.Sprintf("m%03d", i)
	}
	for _, n := range []int{64, 255} {
		c, err := cone.New(names[:n], nil)
		if err != nil {
			return expectf("%d members refused: %v", n, err)
		}
		back, err := cone.Parse(c.Serialize())
		if err != nil || len(back.Include()) != n {
			return expectf("%d members do not round-trip: %v", n, err)
		}
	}
	if _, err := cone.New(names, nil); err == nil {
		return expectf("%d members accepted past the count byte", len(names))
	}
	return nil
}

func scLongest(t *testing.T) error {
	c := scDefault(t)
	for path, member := range map[string]string{
		"src/a.c": "src", "src/vendor/v.c": "src/vendor", "src/vendor/keep/k.c": "src/vendor/keep",
	} {
		if d := c.Decide(path); d.Member != member {
			return expectf("%s decided by %q, want %q", path, d.Member, member)
		}
	}
	shuffled := scCone(t, []string{"src/vendor/keep", "src"}, []string{"src/vendor"})
	if !bytes.Equal(shuffled.Serialize(), c.Serialize()) || shuffled.Digest() != c.Digest() {
		return expectf("input order changed the serialization")
	}
	return nil
}

func scReinclude(t *testing.T) error {
	c := scDefault(t)
	if c.Covers("src/vendor/v.c") || !c.Covers("src/vendor/keep/k.c") {
		return expectf("exclusion then re-inclusion: v.c %v, k.c %v", c.Covers("src/vendor/v.c"), c.Covers("src/vendor/keep/k.c"))
	}
	f := newSCRepo(t, false)
	return scSameList("materialized", scOnDisk(t, f.open(t, c, f.c1).Dir()), scDefaultCovered)
}

func scRedundant(t *testing.T) error {
	c := scCone(t, []string{"src"}, []string{"src/vendor", "src/vendor/x"})
	return scSameList("exclusions", c.Exclude(), []string{"src/vendor"})
}

// A tree walk under a cone with exclusions lists exactly the covered files,
// and never asks for a sub-manifest wholly outside the cone: here each such
// node is removed from the store first.
func scRanges(t *testing.T) error {
	f := newSCRepo(t, true)
	c := scCone(t, []string{"src/vendor"}, []string{"src/vendor/keep"})
	want := []string{"src/vendor/v.c"}
	d, err := manifest.GetManifest(f.r, f.c1)
	if err != nil {
		return err
	}
	removed := 0
	for i, s := range d.S {
		limit := ""
		if i+1 < len(d.S) {
			limit = d.S[i+1].Name
		}
		if c.MayCover(s.Name, limit) {
			continue
		}
		if _, err := f.r.DB().Exec("UPDATE blob SET content=NULL, size=-1 WHERE uuid=?", s.UUID); err != nil {
			return err
		}
		removed++
	}
	if removed == 0 {
		return expectf("the fixture prunes nothing")
	}
	files, err := manifest.ListFilesFiltered(f.r, f.c1, c)
	if err != nil {
		return expectf("filtered walk with %d out-of-cone nodes absent: %v", removed, err)
	}
	var got []string
	for _, e := range files {
		got = append(got, e.Name)
	}
	return scSameList("filtered", got, want)
}

func scLiteral(t *testing.T) error {
	c := scCone(t, []string{"src/*", "a?b", "[x]"}, nil)
	if c.Covers("src/a.c") || !c.Covers("src/*") || !c.Covers("src/*/y") || c.Covers("axb") || !c.Covers("[x]") {
		return expectf("pattern characters were not taken literally")
	}
	return nil
}

func scFileMember(t *testing.T) error {
	f := newSCRepo(t, false)
	c := scCone(t, []string{"README", "src/b.c"}, nil)
	return scSameList("materialized", scOnDisk(t, f.open(t, c, f.c1).Dir()), []string{"README", "src/b.c"})
}

func scAmend(t *testing.T) error {
	start := scDefault(t)
	wider, err := start.With([]string{"tools"}, nil)
	if err != nil {
		return err
	}
	if !wider.Equal(scCone(t, []string{"tools", "src", "src/vendor/keep"}, []string{"src/vendor"})) {
		return expectf("adding a member differs from restating the set")
	}
	back, err := wider.Without([]string{"tools"})
	if err != nil {
		return err
	}
	if !back.Equal(start) {
		return expectf("removing the added member does not restore the cone")
	}
	return nil
}

func scBounds(t *testing.T) error {
	if _, err := cone.New([]string{strings.Repeat("a", cone.MaxMemberLen+1)}, nil); err == nil {
		return expectf("a member of %d bytes was accepted", cone.MaxMemberLen+1)
	}
	if _, err := cone.New([]string{strings.Repeat("a", cone.MaxMemberLen)}, nil); err != nil {
		return expectf("a member of %d bytes was refused: %v", cone.MaxMemberLen, err)
	}
	return nil
}

// --- §3.1, §3.5, §4, §4.1: the sparse worktree ----------------------------

func scWorktreeCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.3.1.local", Req: "SC-3.1-1"}, scLocal},
		{Case{ID: "sparsecone.3.1.reshape", Req: "SC-3.1-2"}, scReshape},
		{Case{ID: "sparsecone.3.5.widen", Req: "SC-3.5-2"}, scWiden},
		{Case{ID: "sparsecone.3.5.narrow", Req: "SC-3.5-3"}, scNarrow},
		{Case{ID: "sparsecone.4.trees", Req: "SC-4-1"}, scTrees},
		{Case{ID: "sparsecone.4.1.materialize", Req: "SC-4.1-1"}, func(t *testing.T) error {
			f := newSCRepo(t, false)
			return scSameList("materialized", scOnDisk(t, f.open(t, scDefault(t), f.c1).Dir()), scDefaultCovered)
		}},
		{Case{ID: "sparsecone.4.1.record", Req: "SC-4.1-2"}, scRecord},
		{Case{ID: "sparsecone.4.1.status", Req: "SC-4.1-3"}, scStatus},
		{Case{ID: "sparsecone.4.1.refuse", Req: "SC-4.1-4"}, scRefuse},
		{Case{ID: "sparsecone.4.1.seam", Req: "SC-4.1-5"}, scSeam},
	}
}

func scLocal(t *testing.T) error {
	f := newSCRepo(t, false)
	var before, after int
	if err := f.r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before); err != nil {
		return err
	}
	co := f.open(t, scDefault(t), f.c1)
	if err := f.r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&after); err != nil {
		return err
	}
	if before != after {
		return expectf("opening a sparse checkout stored %d artifacts", after-before)
	}
	reopened, err := checkout.Open(f.r, co.Dir(), checkout.OpenOpts{})
	if err != nil {
		return err
	}
	defer reopened.Close()
	if reopened.Cone() == nil || !reopened.Cone().Equal(scDefault(t)) {
		return expectf("the cone did not survive a reopen of the checkout")
	}
	return nil
}

func scReshape(t *testing.T) error {
	f := newSCRepo(t, false)
	co := f.open(t, scDefault(t), f.c1)
	for _, next := range []*cone.Cone{scCone(t, []string{"tools"}, nil), scDefault(t), nil, scCone(t, []string{"README"}, nil)} {
		if _, err := co.SetCone(next); err != nil {
			return expectf("SetCone(%v): %v", next, err)
		}
		var want []string
		for _, name := range keysOf(scFiles) {
			if next == nil || next.Covers(name) {
				want = append(want, name)
			}
		}
		if err := scSameList(fmt.Sprintf("after SetCone(%v)", next), scOnDisk(t, co.Dir()), want); err != nil {
			return err
		}
	}
	return nil
}

func scWiden(t *testing.T) error {
	f := newSCRepo(t, false)
	co := f.open(t, scDefault(t), f.c1)
	wider, err := scDefault(t).With([]string{"tools"}, nil)
	if err != nil {
		return err
	}
	change, err := co.SetCone(wider)
	if err != nil {
		return err
	}
	return scSameList("widening materialized", change.Materialized, []string{"tools/t.c", "tools/x/y/z.txt"})
}

func scNarrow(t *testing.T) error {
	f := newSCRepo(t, false)
	co := f.open(t, nil, f.c1)
	var before, after int
	f.r.DB().QueryRow("SELECT count(*) FROM blob WHERE size>=0").Scan(&before)
	change, err := co.SetCone(scCone(t, []string{"src/a.c"}, nil))
	if err != nil {
		return err
	}
	f.r.DB().QueryRow("SELECT count(*) FROM blob WHERE size>=0").Scan(&after)
	if before != after {
		return expectf("narrowing changed the store: %d artifacts, then %d", before, after)
	}
	if len(change.Dematerialized) != len(scFiles)-1 {
		return expectf("narrowing dematerialized %q", change.Dematerialized)
	}
	return scSameList("on disk", scOnDisk(t, co.Dir()), []string{"src/a.c"})
}

func scTrees(t *testing.T) error {
	for _, tree := range []bool{false, true} {
		f := newSCRepo(t, tree)
		if err := scSameList(fmt.Sprintf("tree=%v materialized", tree),
			scOnDisk(t, f.open(t, scDefault(t), f.c1).Dir()), scDefaultCovered); err != nil {
			return err
		}
	}
	return nil
}

func scRecord(t *testing.T) error {
	f := newSCRepo(t, false)
	co := f.open(t, scDefault(t), f.c1)
	path, err := checkout.FindCheckoutDB(simio.OSStorage{}, co.Dir(), false)
	if err != nil {
		return err
	}
	vfdb, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		return err
	}
	defer vfdb.Close()
	var n int
	if err := vfdb.QueryRow("SELECT count(*) FROM vfile").Scan(&n); err != nil {
		return err
	}
	if n != len(scDefaultCovered) {
		return expectf("the checkout database records %d files, the cone covers %d", n, len(scDefaultCovered))
	}
	return nil
}

func scStatus(t *testing.T) error {
	f := newSCRepo(t, false)
	co := f.open(t, scDefault(t), f.c1)
	scWrite(t, co.Dir(), "tools/stray.c", "stray\n")
	scWrite(t, co.Dir(), "src/vendor/v.c", "by hand\n")
	scWrite(t, co.Dir(), "src/a.c", "edited\n")
	vid, _, err := co.Version()
	if err != nil {
		return err
	}
	var changed []string
	if err := co.VisitChanges(vid, true, func(e checkout.ChangeEntry) error {
		changed = append(changed, e.Name)
		return nil
	}); err != nil {
		return err
	}
	if err := scSameList("status", changed, []string{"src/a.c"}); err != nil {
		return err
	}
	_, err = co.Manage(checkout.ManageOpts{Paths: []string{"tools/stray.c"}})
	return scErrIs("tracking an uncovered file", err, checkout.ErrOutsideCone)
}

func scRefuse(t *testing.T) error {
	f := newSCRepo(t, false)
	co := f.open(t, scDefault(t), f.c1)
	scWrite(t, co.Dir(), "src/vendor/new.c", "new\n")
	_, err := co.Manage(checkout.ManageOpts{Paths: []string{"src/vendor/new.c"}})
	if err := scErrIs("tracking an excluded path", err, checkout.ErrOutsideCone); err != nil {
		return err
	}
	msg := err.Error()
	for _, want := range []string{"src/vendor/new.c", "!src/vendor", "sparse cone"} {
		if !strings.Contains(msg, want) {
			return expectf("refusal %q does not name %q", msg, want)
		}
	}
	for _, word := range []string{"missing", "corrupt", "damage", "not found"} {
		if strings.Contains(msg, word) {
			return expectf("refusal %q speaks of %s", msg, word)
		}
	}
	return nil
}

// Every form of manifest goes through the one seam, which narrows each the
// same way: flat, delta and tree.
func scSeam(t *testing.T) error {
	c := scDefault(t)
	for _, tree := range []bool{false, true} {
		f := newSCRepo(t, tree)
		rids := []libfossil.FslID{f.c1}
		if !tree {
			var mf []manifest.File
			for _, name := range keysOf(scFiles) {
				mf = append(mf, manifest.File{Name: name, Content: []byte(scFiles[name] + "2")})
			}
			delta, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{Files: mf, Comment: "d", User: "u",
				Parent: f.c1, Delta: true, Time: f.when.Add(time.Hour)})
			if err != nil {
				return err
			}
			rids = append(rids, delta)
		}
		for _, rid := range rids {
			files, err := manifest.ListFilesFiltered(f.r, rid, c)
			if err != nil {
				return err
			}
			var got []string
			for _, e := range files {
				got = append(got, e.Name)
			}
			if err := scSameList(fmt.Sprintf("tree=%v rid %d", tree, rid), got, scDefaultCovered); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- §4.2 to §4.4, §7: commits and operations that cross the cone ----------

func scCommitCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.4.2.union", Req: "SC-4.2-1"}, scUnion},
		{Case{ID: "sparsecone.4.2.verbatim", Req: "SC-4.2-2"}, scVerbatim},
		{Case{ID: "sparsecone.4.2.tree", Req: "SC-4.2-3"}, scTreeCommit},
		{Case{ID: "sparsecone.4.2.ordinary", Req: "SC-4.2-4"}, scOrdinary},
		{Case{ID: "sparsecone.4.3.rcard", Req: "SC-4.3-1"}, scRCard},
		{Case{ID: "sparsecone.4.3.absent", Req: "SC-4.3-3"}, scAbsentR},
		{Case{ID: "sparsecone.4.3.dense", Req: "SC-4.3-4"}, scRCard},
		{Case{ID: "sparsecone.4.4.update", Req: "SC-4.4-1"}, scUpdate},
		{Case{ID: "sparsecone.4.4.merge", Req: "SC-4.4-1"}, scMerge},
		{Case{ID: "sparsecone.4.4.count", Req: "SC-4.4-2"}, scUpdate},
		{Case{ID: "sparsecone.4.4.read", Req: "SC-4.4-3"}, scRead},
		{Case{ID: "sparsecone.7.derived", Req: "SC-7-1"}, scDerived},
	}
}

// scEdit makes the suite's standard change in co and commits it at when.
func scEdit(t *testing.T, co *checkout.Checkout, when time.Time) (libfossil.FslID, string, error) {
	t.Helper()
	scWrite(t, co.Dir(), "src/a.c", "a edited\n")
	scWrite(t, co.Dir(), "src/vendor/keep/new.c", "new\n")
	if _, err := co.Manage(checkout.ManageOpts{Paths: []string{"src/vendor/keep/new.c"}}); err != nil {
		return 0, "", err
	}
	if err := co.Unmanage(checkout.UnmanageOpts{Paths: []string{"src/b.c"}}); err != nil {
		return 0, "", err
	}
	return co.Commit(checkout.CommitOpts{Message: "edit", User: "u", Time: when})
}

// scEdited is scFiles after scEdit.
func scEdited() map[string]string {
	out := map[string]string{"src/a.c": "a edited\n", "src/vendor/keep/new.c": "new\n"}
	for k, v := range scFiles {
		if _, set := out[k]; !set && k != "src/b.c" {
			out[k] = v
		}
	}
	return out
}

func scContent(r *repo.Repo, rid libfossil.FslID) (map[string]string, error) {
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range files {
		frid, ok := content.AvailableByUUID(r.DB(), e.UUID)
		if !ok {
			return nil, fmt.Errorf("%s: content missing", e.Name)
		}
		data, err := content.Expand(r.DB(), frid)
		if err != nil {
			return nil, err
		}
		out[e.Name] = string(data)
	}
	return out, nil
}

func scSameContent(what string, got, want map[string]string) error {
	if len(got) != len(want) {
		return expectf("%s: %d files, want %d", what, len(got), len(want))
	}
	for name, text := range want {
		if got[name] != text {
			return expectf("%s: %s = %q, want %q", what, name, got[name], text)
		}
	}
	return nil
}

func scUnion(t *testing.T) error {
	f := newSCRepo(t, false)
	rid, _, err := scEdit(t, f.open(t, scDefault(t), f.c1), f.when.Add(time.Hour))
	if err != nil {
		return err
	}
	got, err := scContent(f.r, rid)
	if err != nil {
		return err
	}
	return scSameContent("the sparse commit", got, scEdited())
}

// The parent's out-of-cone F cards appear in the child's manifest byte for
// byte.
func scVerbatim(t *testing.T) error {
	f := newSCRepo(t, false)
	rid, _, err := scEdit(t, f.open(t, scDefault(t), f.c1), f.when.Add(time.Hour))
	if err != nil {
		return err
	}
	parent, err := content.Expand(f.r.DB(), f.c1)
	if err != nil {
		return err
	}
	child, err := content.Expand(f.r.DB(), rid)
	if err != nil {
		return err
	}
	c := scDefault(t)
	for _, line := range strings.Split(string(parent), "\n") {
		fields := strings.Fields(line)
		// The fixture's names need no escaping, so a card's name is its token.
		if len(fields) < 3 || fields[0] != "F" || c.Covers(fields[1]) {
			continue
		}
		if !bytes.Contains(child, []byte("\n"+line+"\n")) {
			return expectf("out-of-cone card %q is not in the child", line)
		}
	}
	return nil
}

func scTreeCommit(t *testing.T) error {
	f := newSCRepo(t, true)
	parent, err := manifest.GetManifest(f.r, f.c1)
	if err != nil {
		return err
	}
	var before int
	f.r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before)
	rid, _, err := scEdit(t, f.open(t, scDefault(t), f.c1), f.when.Add(time.Hour))
	if err != nil {
		return err
	}
	child, err := manifest.GetManifest(f.r, rid)
	if err != nil {
		return err
	}
	shared := 0
	for _, s := range child.S {
		if slices.Contains(parent.S, s) {
			shared++
		}
	}
	if shared == 0 {
		return expectf("no untouched subtree of the parent is re-referenced: %d nodes", len(child.S))
	}
	return nil
}

// A sparse checkout's commit is the very artifact a dense checkout's commit
// of the same change is, flat and tree.
func scOrdinary(t *testing.T) error {
	for _, tree := range []bool{false, true} {
		var uuids []string
		for _, c := range []*cone.Cone{scDefault(t), nil} {
			f := newSCRepo(t, tree)
			_, uuid, err := scEdit(t, f.open(t, c, f.c1), f.when.Add(time.Hour))
			if err != nil {
				return err
			}
			uuids = append(uuids, uuid)
		}
		if uuids[0] != uuids[1] {
			return expectf("tree=%v: sparse commit %s, dense commit %s", tree, uuids[0], uuids[1])
		}
	}
	return nil
}

func scRCard(t *testing.T) error {
	f := newSCRepo(t, false)
	rid, _, err := scEdit(t, f.open(t, scDefault(t), f.c1), f.when.Add(time.Hour))
	if err != nil {
		return err
	}
	d, err := manifest.GetManifest(f.r, rid)
	if err != nil {
		return err
	}
	if d.R == "" {
		return expectf("the sparse commit carries no R card")
	}
	return f.r.WithTx(func(tx *db.Tx) error { return manifest.VerifyRCard(tx, nil, rid) })
}

// A check-in with no R card reads and verifies as healthy.
func scAbsentR(t *testing.T) error {
	f := newSCRepo(t, false)
	files, err := manifest.ListFiles(f.r, f.c1)
	if err != nil {
		return err
	}
	parentUUID := ""
	f.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", f.c1).Scan(&parentUUID)
	d := &deck.Deck{Type: deck.Checkin, C: "no R", D: f.when.Add(time.Hour), U: deck.User("u"), P: []string{parentUUID}}
	for _, e := range files {
		d.F = append(d.F, deck.FileCard{Name: e.Name, UUID: e.UUID, Perm: e.Perm})
	}
	data, err := d.Marshal()
	if err != nil {
		return err
	}
	if err := f.r.WithTx(func(tx *db.Tx) error { _, _, err := blob.Store(tx, data); return err }); err != nil {
		return err
	}
	if _, err := manifest.Crosslink(f.r); err != nil {
		return err
	}
	report, err := verify.Verify(f.r)
	if err != nil {
		return err
	}
	if !report.OK() {
		return expectf("verify reports a check-in without an R card: %+v", report.Issues)
	}
	return nil
}

func scUpdate(t *testing.T) error {
	f := newSCRepo(t, false)
	next := scEditedMap(map[string]string{"src/a.c": "a2\n", "tools/t.c": "tools2\n", "docs/new.txt": "n\n"}, "README")
	c2 := f.commit(t, next, f.c1)
	co := f.open(t, scDefault(t), f.c1)
	res, err := co.Update(checkout.UpdateOpts{TargetRID: c2})
	if err != nil {
		return err
	}
	if res.OutOfCone != 3 {
		return expectf("update reported %d out-of-cone changes, want 3", res.OutOfCone)
	}
	if err := scSameList("on disk after update", scOnDisk(t, co.Dir()), scDefaultCovered); err != nil {
		return err
	}
	scWrite(t, co.Dir(), "src/b.c", "b2\n")
	rid, _, err := co.Commit(checkout.CommitOpts{Message: "m", User: "u", Time: f.when.Add(time.Hour)})
	if err != nil {
		return err
	}
	got, err := scContent(f.r, rid)
	if err != nil {
		return err
	}
	next["src/b.c"] = "b2\n"
	return scSameContent("the commit after update", got, next)
}

// scEditedMap is scFiles with set applied and drop removed.
func scEditedMap(set map[string]string, drop ...string) map[string]string {
	out := map[string]string{}
	for k, v := range scFiles {
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

// §4.4 applies out-of-cone changes to the record without widening the cone.
// The record-only cases also verify committed bytes and conflict handling.
func scMerge(t *testing.T) error {
	f := newSCRepo(t, false)
	theirs := f.commit(t, scEditedMap(map[string]string{"tools/t.c": "theirs\n"}), f.c1)
	ours := f.commit(t, scEditedMap(map[string]string{"src/a.c": "ours\n"}), f.c1)
	co := f.open(t, scDefault(t), ours)
	if _, err := co.Merge(checkout.MergeOpts{Version: theirs}); err != nil {
		return expectf("the merge was refused: %v", err)
	}
	return nil
}

func scRead(t *testing.T) error {
	f := newSCRepo(t, false)
	co := f.open(t, scDefault(t), f.c1)
	if _, err := os.Stat(filepath.Join(co.Dir(), "tools/t.c")); err == nil {
		return expectf("tools/t.c is materialized")
	}
	entry, found, err := manifest.LookupFile(f.r, f.c1, "tools/t.c")
	if err != nil || !found {
		return expectf("lookup of an uncovered file: %v, %v", found, err)
	}
	rid, ok := content.AvailableByUUID(f.r.DB(), entry.UUID)
	if !ok {
		return expectf("an uncovered file's content is not readable")
	}
	data, err := content.Expand(f.r.DB(), rid)
	if err != nil || string(data) != scFiles["tools/t.c"] {
		return expectf("read %q, %v", data, err)
	}
	return nil
}

// The same change committed sparsely and densely leaves the same derived
// state: the cone reaches no derivation.
func scDerived(t *testing.T) error {
	const query = `SELECT (SELECT count(*) FROM mlink), (SELECT count(*) FROM plink),
		(SELECT group_concat(b.uuid) FROM leaf l JOIN blob b ON b.rid=l.rid),
		(SELECT count(*) FROM tagxref), (SELECT count(*) FROM event)`
	var rows []string
	for _, c := range []*cone.Cone{scDefault(t), nil} {
		f := newSCRepo(t, false)
		if _, _, err := scEdit(t, f.open(t, c, f.c1), f.when.Add(time.Hour)); err != nil {
			return err
		}
		var mlink, plink, tags, events int
		var leaves string
		if err := f.r.DB().QueryRow(query).Scan(&mlink, &plink, &leaves, &tags, &events); err != nil {
			return err
		}
		rows = append(rows, fmt.Sprint(mlink, plink, leaves, tags, events))
	}
	if rows[0] != rows[1] {
		return expectf("derived state sparse %s, dense %s", rows[0], rows[1])
	}
	return nil
}
