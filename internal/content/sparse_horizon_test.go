package content_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// hzMode is how the fixture's check-ins name their files: full manifests,
// c3 a delta manifest over c2 (naming src/keep.c only through its B
// baseline), or sub-manifest trees.
type hzMode string

const (
	hzFull  hzMode = "full"
	hzDelta hzMode = "delta"
	hzTree  hzMode = "tree"
)

// hzFixture is c1 -> c2 -> c3. src/keep.c enters at c1 and never changes,
// so mlink records it at c1 alone though every check-in names it: the case
// a horizon computed from mlink would elide.
type hzFixture struct {
	r          *repo.Repo
	c1, c2, c3 libfossil.FslID
	// blob is each version's rid, by "keep", "old1", "old2", "gone", "now",
	// "new"; every one of them is a phantom.
	blob map[string]libfossil.FslID
	uuid map[string]string
}

func newHZFixture(t *testing.T, mode hzMode) *hzFixture {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "hz.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if mode == hzTree {
		for k, v := range map[string]string{
			manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
		} {
			if err := r.SetConfig(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := &hzFixture{r: r, blob: map[string]libfossil.FslID{}, uuid: map[string]string{}}
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	commit := func(parent libfossil.FslID, delta bool, files ...string) libfossil.FslID {
		t.Helper()
		// Held files that never change make a delta manifest smaller than
		// its full form, so the writer emits one.
		mf := []manifest.File{
			{Name: "lib/x.c", Content: []byte("x\n")}, {Name: "lib/y.c", Content: []byte("y\n")},
			{Name: "lib/z.c", Content: []byte("z\n")},
		}
		for i := 0; i < len(files); i += 2 {
			mf = append(mf, manifest.File{Name: files[i], Content: []byte(files[i+1])})
		}
		when = when.Add(time.Hour)
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: mf, Comment: "c", User: "u", Parent: parent, Time: when,
			Delta: delta,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	f.c1 = commit(0, false, "doc/gone.txt", "gone\n", "src/keep.c", "keep\n", "src/old.c", "old 1\n")
	f.c2 = commit(f.c1, false, "doc/now.txt", "now\n", "src/keep.c", "keep\n", "src/old.c", "old 2\n")
	f.c3 = commit(f.c2, mode == hzDelta,
		"doc/now.txt", "now\n", "src/keep.c", "keep\n", "src/new.c", "new\n", "src/old.c", "old 2\n")
	if mode == hzDelta {
		if d := hzDeck(t, r, f.c3); d.B != f.uuidOf(t, f.c2) || hzNames(d.F)["src/keep.c"] {
			t.Fatalf("c3 is not a delta manifest over c2: %+v", d)
		}
	}
	for key, at := range map[string]struct {
		rid  libfossil.FslID
		name string
	}{
		"keep": {f.c1, "src/keep.c"}, "old1": {f.c1, "src/old.c"}, "gone": {f.c1, "doc/gone.txt"},
		"old2": {f.c2, "src/old.c"}, "now": {f.c2, "doc/now.txt"}, "new": {f.c3, "src/new.c"},
	} {
		e, ok, err := manifest.LookupFile(r, at.rid, at.name)
		if err != nil || !ok {
			t.Fatalf("LookupFile %s: %v, %v", at.name, ok, err)
		}
		f.uuid[key] = e.UUID
	}
	for key, uuid := range f.uuid {
		f.blob[key] = hzPhantomize(t, r, uuid)
	}
	return f
}

func hzDeck(t *testing.T, r *repo.Repo, rid libfossil.FslID) *deck.Deck {
	t.Helper()
	data, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	d, err := deck.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func hzNames(cards []deck.FileCard) map[string]bool {
	out := map[string]bool{}
	for _, f := range cards {
		out[f.Name] = true
	}
	return out
}

// hzPhantomize turns a held artifact into a phantom, first expanding every
// delta over it in place.
func hzPhantomize(t *testing.T, r *repo.Repo, uuid string) libfossil.FslID {
	t.Helper()
	rid, ok := content.AvailableByUUID(r.DB(), uuid)
	if !ok {
		t.Fatalf("hzPhantomize: %s is not held", uuid)
	}
	err := r.WithTx(func(tx *db.Tx) error {
		rows, err := tx.Query("SELECT rid FROM delta WHERE srcid=?", rid)
		if err != nil {
			return err
		}
		var children []libfossil.FslID
		for rows.Next() {
			var child libfossil.FslID
			if err := rows.Scan(&child); err != nil {
				return errors.Join(err, rows.Close())
			}
			children = append(children, child)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, child := range children {
			if err := content.Undelta(tx, child); err != nil {
				return err
			}
		}
		for _, stmt := range []string{
			"DELETE FROM delta WHERE rid=?",
			"UPDATE blob SET size=-1, content=NULL WHERE rid=?",
			"INSERT OR IGNORE INTO phantom(rid) VALUES(?)",
		} {
			if _, err := tx.Exec(stmt, rid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

func (f *hzFixture) uuidOf(t *testing.T, rid libfossil.FslID) string {
	t.Helper()
	var uuid string
	if err := f.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	return uuid
}

// setHorizon makes the store cone a content horizon at c under rule, with
// the path members include (none: horizon-only).
func (f *hzFixture) setHorizon(t *testing.T, c string, rule cone.HorizonRule, include ...string) {
	t.Helper()
	h, err := cone.NewHorizon(c, rule, cone.DepthContent)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cone.NewWithHorizon(include, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), sc); err != nil {
		t.Fatal(err)
	}
}

// expect requires each named blob in its class and the elided count to be
// the number named elided.
func (f *hzFixture) expect(t *testing.T, want map[string]content.Absence) {
	t.Helper()
	a, err := content.LoadAbsencesContext(context.Background(), f.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	elided := 0
	for key, k := range want {
		if k == content.Elided {
			elided++
		}
		if got := a.Class(f.blob[key]); got != k {
			t.Errorf("%s = %v, want %v", key, got, k)
		}
	}
	if got := a.Count(content.Elided); got != elided {
		t.Errorf("elided count %d, want %d", got, elided)
	}
}

func hzClasses(transit content.Absence, elided ...string) map[string]content.Absence {
	out := map[string]content.Absence{}
	for _, key := range []string{"keep", "old1", "old2", "gone", "now", "new"} {
		out[key] = transit
	}
	for _, key := range elided {
		out[key] = content.Elided
	}
	return out
}

// Under descendants of c2, K is {c2, c3}. Every blob a K check-in names is
// above, including src/keep.c, which mlink places at c1 only and which c2
// names directly or through a sub-manifest node. With C at the delta
// manifest c3, K is {c3}, which names it and every other c2 file only
// through its B baseline, c2. Blobs only c1 names are elided.
func TestHorizonAboveFollowsHeldTrees(t *testing.T) {
	for _, mode := range []hzMode{hzFull, hzDelta, hzTree} {
		t.Run(string(mode), func(t *testing.T) {
			f := newHZFixture(t, mode)
			c := f.c2
			if mode == hzDelta {
				c = f.c3
			}
			f.setHorizon(t, f.uuidOf(t, c), cone.RuleDescendants)
			f.expect(t, hzClasses(content.Transit, "old1", "gone"))
		})
	}
}

// Moving C to an ancestor (a deepen) and losing the cone are
// recomputation: the classes follow, and no blob or phantom row changes.
func TestHorizonReclassifiesWithoutMigration(t *testing.T) {
	f := newHZFixture(t, hzTree)
	dump := func() string {
		var out string
		if err := f.r.DB().QueryRow(`SELECT
			(SELECT group_concat(rid || ':' || size || ':' || uuid, ',') FROM (SELECT * FROM blob ORDER BY rid)) || '|' ||
			(SELECT group_concat(rid, ',') FROM (SELECT * FROM phantom ORDER BY rid))`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := dump()
	f.setHorizon(t, f.uuidOf(t, f.c3), cone.RuleDescendants)
	f.expect(t, hzClasses(content.Transit, "old1", "gone"))
	f.setHorizon(t, f.uuidOf(t, f.c1), cone.RuleDescendants)
	f.expect(t, hzClasses(content.Transit))
	f.setHorizon(t, f.uuidOf(t, f.c2), cone.RuleDescendants)
	f.expect(t, hzClasses(content.Transit, "old1", "gone"))
	if err := content.SetStoreCone(f.r.DB(), nil); err != nil {
		t.Fatal(err)
	}
	f.expect(t, hzClasses(content.Transit))
	if after := dump(); after != before {
		t.Fatalf("reclassification changed stored rows:\n%s\nwas\n%s", after, before)
	}
}

// With path members as well, a blob is kept only when it is above the
// horizon and in the path cone.
func TestHorizonIntersectsPathCone(t *testing.T) {
	for _, mode := range []hzMode{hzFull, hzTree} {
		t.Run(string(mode), func(t *testing.T) {
			f := newHZFixture(t, mode)
			f.setHorizon(t, f.uuidOf(t, f.c2), cone.RuleDescendants, "src")
			f.expect(t, hzClasses(content.Transit, "old1", "gone", "now"))
		})
	}
}

// Path members never elide structure under a horizon: a phantom
// sub-manifest node outside the path cone and below the horizon is wanted.
func TestHorizonPathConeKeepsStructure(t *testing.T) {
	f := newHZFixture(t, hzTree)
	var node string
	inC2 := map[string]bool{}
	for _, s := range hzDeck(t, f.r, f.c2).S {
		inC2[s.UUID] = true
	}
	for _, s := range hzDeck(t, f.r, f.c1).S {
		if strings.HasPrefix(s.Name, "doc/") && !inC2[s.UUID] {
			node = s.UUID
		}
	}
	if node == "" {
		t.Fatal("c1 has no doc/ node of its own")
	}
	rid := hzPhantomize(t, f.r, node)
	f.setHorizon(t, f.uuidOf(t, f.c2), cone.RuleDescendants, "src")
	a, err := content.LoadAbsences(f.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Class(rid); got != content.Transit {
		t.Fatalf("a node outside the paths and below the horizon = %v, want transit", got)
	}
}

// A phantom a held artifact is a delta against is transit whatever the
// horizon says (draft-fossil-horizon-00 §4.2), and a shunned one is shunned.
func TestHorizonDeltaBaseAndShun(t *testing.T) {
	f := newHZFixture(t, hzFull)
	target := []byte("derived from old 1\n")
	d, err := delta.Create([]byte("old 1\n"), target)
	if err != nil {
		t.Fatal(err)
	}
	name := hash.SHA3(target)
	if len(f.uuid["old1"]) == 40 {
		name = hash.SHA1(target)
	}
	if _, err := blob.StoreDeltaRaw(f.r.DB(), name, d, f.blob["old1"], nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.DB().Exec("INSERT INTO shun(uuid, mtime) VALUES(?, 0)", f.uuid["gone"]); err != nil {
		t.Fatal(err)
	}
	f.setHorizon(t, f.uuidOf(t, f.c2), cone.RuleDescendants)
	want := hzClasses(content.Transit)
	want["gone"] = content.Shunned
	f.expect(t, want)
}

// A horizon whose C the store does not hold as a check-in elides nothing:
// no blob can be proven below a horizon the store cannot place, and eliding
// everything would leave the pull that brings C nothing to ask for. Path
// members still elide.
func TestHorizonUnplacedElidesNothing(t *testing.T) {
	f := newHZFixture(t, hzFull)
	for _, c := range []string{strings.Repeat("ab", 20), f.uuid["old2"]} {
		f.setHorizon(t, c, cone.RuleDescendants)
		f.expect(t, hzClasses(content.Transit))
		f.setHorizon(t, c, cone.RuleDescendants, "src")
		f.expect(t, hzClasses(content.Transit, "gone", "now"))
	}
}

// A structural horizon is not implemented: the store refuses it rather
// than hold a cone its classification would read as a content horizon.
func TestSetStoreConeRefusesStructuralHorizon(t *testing.T) {
	f := newHZFixture(t, hzFull)
	h, err := cone.NewHorizon(f.uuidOf(t, f.c2), cone.RuleDescendants, cone.DepthStructural)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), sc); err == nil {
		t.Fatal("a structural horizon was stored")
	}
	if got, err := content.StoreCone(f.r.DB()); err != nil || got != nil {
		t.Fatalf("after the refusal the store cone is %v, %v; want dense", got, err)
	}
}

// The predicate a sync applies to what it received is the classification's:
// it places exactly the blobs LoadAbsences elides below the horizon, and it
// is nil for a cone without one, which elides nothing by age.
func TestHorizonBelowMatchesClassification(t *testing.T) {
	for _, mode := range []hzMode{hzFull, hzDelta, hzTree} {
		t.Run(string(mode), func(t *testing.T) {
			f := newHZFixture(t, mode)
			c := f.c2
			if mode == hzDelta {
				c = f.c3
			}
			f.setHorizon(t, f.uuidOf(t, c), cone.RuleDescendants)
			sc, err := content.StoreCone(f.r.DB())
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			refs, err := content.LoadConeReferences(ctx, f.r.DB(), sc)
			if err != nil {
				t.Fatal(err)
			}
			below, err := refs.HorizonBelow(ctx, f.r.DB(), sc)
			if err != nil || below == nil {
				t.Fatalf("HorizonBelow = %v, %v", below != nil, err)
			}
			for key, rid := range f.blob {
				want := key == "old1" || key == "gone"
				if got := below(rid); got != want {
					t.Errorf("%s below = %v, want %v", key, got, want)
				}
			}
			for _, rid := range []libfossil.FslID{f.c1, f.c2, f.c3} {
				if below(rid) {
					t.Errorf("check-in rid=%d placed below the horizon", rid)
				}
			}
			paths, err := cone.New([]string{"src"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if none, err := refs.HorizonBelow(ctx, f.r.DB(), paths); err != nil || none != nil {
				t.Fatalf("a path cone's HorizonBelow = %v, %v; want nil", none != nil, err)
			}
		})
	}
}

// A cluster a horizon store was sent is not held: its phantom, which the
// cluster tag marks, is elided under a horizon and transit without one,
// and a held delta's base stays transit whatever it is.
func TestHorizonElidesClusterPhantom(t *testing.T) {
	f := newHZFixture(t, hzFull)
	d := &deck.Deck{Type: deck.Cluster, M: []string{f.uuid["old1"]}}
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cluster := hzPhantomCluster(t, f.r, data)
	classOf := func() content.Absence {
		t.Helper()
		a, err := content.LoadAbsences(f.r.DB())
		if err != nil {
			t.Fatal(err)
		}
		return a.Class(cluster)
	}
	if got := classOf(); got != content.Transit {
		t.Fatalf("dense store: cluster phantom %v, want transit", got)
	}
	paths, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), paths); err != nil {
		t.Fatal(err)
	}
	if got := classOf(); got != content.Transit {
		t.Fatalf("path cone: cluster phantom %v, want transit", got)
	}
	f.setHorizon(t, strings.Repeat("ab", 20), cone.RuleDescendants)
	if got := classOf(); got != content.Elided {
		t.Fatalf("horizon: cluster phantom %v, want elided", got)
	}
	target := append(data, "derived\n"...)
	dd, err := delta.Create(data, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blob.StoreDeltaRaw(f.r.DB(), hash.SHA1(target), dd, cluster, nil); err != nil {
		t.Fatal(err)
	}
	if got := classOf(); got != content.Transit {
		t.Fatalf("horizon: a held delta's cluster base %v, want transit", got)
	}
}

// hzPhantomCluster records data as a cluster this store was sent and did
// not keep: a phantom carrying the cluster tag.
func hzPhantomCluster(t *testing.T, r *repo.Repo, data []byte) libfossil.FslID {
	t.Helper()
	rid, err := blob.StorePhantom(r.DB(), hash.SHA1(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB().Exec(`INSERT INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
		VALUES(7, 1, ?, ?, NULL, 0, ?)`, rid, rid, rid); err != nil {
		t.Fatal(err)
	}
	return rid
}
