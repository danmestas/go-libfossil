package manifest

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// These tests drive the tree reader of draft-fossil-submanifests-00 through
// the enumeration seam: trees are built by hand from deck values, stored as
// blobs, and read back through ListFiles, LookupFile, Crosslink and
// VerifyRCard -- the same entry points every consumer uses -- so a test
// passes only when the seam, not a tree-aware caller, gets the answer right.

// treeFixture builds tree artifacts in one repository.
type treeFixture struct {
	t *testing.T
	r *repo.Repo
	// content maps a file name to the bytes stored for it.
	content map[string][]byte
}

func newTreeFixture(t *testing.T) *treeFixture {
	t.Helper()
	return &treeFixture{t: t, r: setupTestRepo(t), content: map[string][]byte{}}
}

// file stores a file's content and returns its F card.
func (f *treeFixture) file(name string) deck.FileCard {
	f.t.Helper()
	data := []byte("content of " + name + "\n")
	f.content[name] = data
	return deck.FileCard{Name: name, UUID: f.storeBytes(data)}
}

// marshal renders d without storing it.
func (f *treeFixture) marshal(d *deck.Deck) []byte {
	f.t.Helper()
	data, err := d.Marshal()
	if err != nil {
		f.t.Fatalf("Marshal: %v", err)
	}
	return data
}

// store stores d's bytes and returns the artifact's name.
func (f *treeFixture) store(d *deck.Deck) string {
	f.t.Helper()
	return f.storeBytes(f.marshal(d))
}

func (f *treeFixture) storeBytes(data []byte) string {
	f.t.Helper()
	var uuid string
	if err := f.r.WithTx(func(tx *db.Tx) error {
		var err error
		_, uuid, err = blob.Store(tx, data)
		return err
	}); err != nil {
		f.t.Fatalf("blob.Store: %v", err)
	}
	return uuid
}

// leaf stores a leaf sub-manifest over files and returns its S card.
func (f *treeFixture) leaf(files ...deck.FileCard) deck.SubmanifestCard {
	f.t.Helper()
	return deck.SubmanifestCard{Name: files[0].Name, UUID: f.store(&deck.Deck{F: files})}
}

// interior stores an interior sub-manifest over refs and returns its S card.
func (f *treeFixture) interior(refs ...deck.SubmanifestCard) deck.SubmanifestCard {
	f.t.Helper()
	return deck.SubmanifestCard{Name: refs[0].Name, UUID: f.store(&deck.Deck{S: refs})}
}

// treeDeck is a tree manifest over refs, with optional parents.
func treeDeck(at time.Time, refs []deck.SubmanifestCard, parents ...string) *deck.Deck {
	return &deck.Deck{C: "tree", D: at, S: refs, P: parents, U: deck.User("u")}
}

// checkin stores manifest d and crosslinks the repository, returning d's rid.
func (f *treeFixture) checkin(d *deck.Deck) (libfossil.FslID, string) {
	f.t.Helper()
	uuid := f.storeBytes(f.marshal(d))
	if _, err := Crosslink(f.r); err != nil {
		f.t.Fatalf("Crosslink: %v", err)
	}
	rid, ok := blob.Exists(f.r.DB(), uuid)
	if !ok {
		f.t.Fatalf("manifest %s not stored", uuid)
	}
	return rid, uuid
}

// rFor computes the R card over the files named, from the fixture's content.
func (f *treeFixture) rFor(names ...string) string {
	f.t.Helper()
	sort.Strings(names)
	d := &deck.Deck{}
	for _, name := range names {
		d.F = append(d.F, deck.FileCard{Name: name, UUID: "x" + name})
	}
	r, err := d.ComputeR(func(uuid string) ([]byte, error) { return f.content[uuid[1:]], nil })
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func listedNames(t *testing.T, r *repo.Repo, rid libfossil.FslID) []string {
	t.Helper()
	files, err := ListFiles(r, rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	names := make([]string, len(files))
	for i, e := range files {
		names[i] = e.Name
	}
	return names
}

var treeTime = time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)

func TestTreeEnumeratesTwoLevels(t *testing.T) {
	f := newTreeFixture(t)
	a, b, c, d := f.file("README.md"), f.file("src/main.c"), f.file("src/node11.c"), f.file("src/util.c")
	refs := []deck.SubmanifestCard{f.leaf(a, b), f.leaf(c, d)}
	rid, _ := f.checkin(treeDeck(treeTime, refs))

	got := strings.Join(listedNames(t, f.r, rid), " ")
	if want := "README.md src/main.c src/node11.c src/util.c"; got != want {
		t.Fatalf("ListFiles = %q, want %q", got, want)
	}
	for name, want := range map[string]bool{
		"README.md": true, "src/main.c": true, "src/util.c": true,
		"AAA": false, "src/mz": false, "zzz": false,
	} {
		_, found, err := LookupFile(f.r, rid, name)
		if err != nil || found != want {
			t.Errorf("LookupFile(%q) = %v, %v; want %v", name, found, err, want)
		}
	}
}

func TestTreeEnumeratesThreeLevels(t *testing.T) {
	f := newTreeFixture(t)
	var leaves []deck.SubmanifestCard
	var want []string
	for i := 0; i < 4; i++ {
		x, y := fmt.Sprintf("f%d0.c", i), fmt.Sprintf("f%d1.c", i)
		leaves = append(leaves, f.leaf(f.file(x), f.file(y)))
		want = append(want, x, y)
	}
	refs := []deck.SubmanifestCard{f.interior(leaves[0], leaves[1]), f.interior(leaves[2], leaves[3])}
	rid, _ := f.checkin(treeDeck(treeTime, refs))
	if got := strings.Join(listedNames(t, f.r, rid), " "); got != strings.Join(want, " ") {
		t.Fatalf("ListFiles = %q, want %q", got, want)
	}
	if _, found, err := LookupFile(f.r, rid, "f31.c"); err != nil || !found {
		t.Fatalf("LookupFile(f31.c) = %v, %v", found, err)
	}
}

// A tree check-in crosslinks with the same mlink rows a flat check-in of the
// same files gets, and its R card verifies over the walked enumeration.
func TestTreeCrosslinksLikeFlat(t *testing.T) {
	f := newTreeFixture(t)
	a, b, c := f.file("a.txt"), f.file("b.txt"), f.file("c.txt")
	refs := []deck.SubmanifestCard{f.leaf(a, b), f.leaf(c)}
	tree := treeDeck(treeTime, refs)
	tree.R = f.rFor("a.txt", "b.txt", "c.txt")
	rid, uuid := f.checkin(tree)

	var events, mlinks int
	f.r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&events)
	f.r.DB().QueryRow("SELECT count(*) FROM mlink WHERE mid=? AND pmid=0", rid).Scan(&mlinks)
	if events != 1 || mlinks != 3 {
		t.Fatalf("event rows %d, root mlink rows %d; want 1 and 3", events, mlinks)
	}
	if err := f.r.WithTx(func(tx *db.Tx) error { return VerifyRCard(tx, nil, rid) }); err != nil {
		t.Fatalf("VerifyRCard: %v", err)
	}

	// A child tree that edits b.txt and drops c.txt: one change row, one
	// delete row, as a flat child would give.
	b2 := deck.FileCard{Name: "b.txt", UUID: f.storeBytes([]byte("b two\n"))}
	child := treeDeck(treeTime.Add(time.Hour), []deck.SubmanifestCard{f.leaf(a, b2)}, uuid)
	crid, _ := f.checkin(child)
	rows, err := f.r.DB().Query(
		`SELECT fn.name, CASE WHEN m.fid=0 THEN 'del' ELSE 'chg' END
		 FROM mlink m JOIN filename fn ON fn.fnid=m.fnid WHERE m.mid=? ORDER BY fn.name`, crid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name, kind string
		rows.Scan(&name, &kind)
		got = append(got, name+":"+kind)
	}
	if want := "b.txt:chg c.txt:del"; strings.Join(got, " ") != want {
		t.Fatalf("child mlink rows %q, want %q", got, want)
	}
}

// A delta manifest may name a tree as its baseline (§5.4): the merge walk
// takes the walked enumeration as its baseline side.
func TestDeltaOverTree(t *testing.T) {
	f := newTreeFixture(t)
	a, b, c := f.file("a.txt"), f.file("b.txt"), f.file("c.txt")
	_, baseUUID := f.checkin(treeDeck(treeTime, []deck.SubmanifestCard{f.leaf(a, b), f.leaf(c)}))
	a2 := deck.FileCard{Name: "a.txt", UUID: f.storeBytes([]byte("a two\n"))}
	f.content["a.txt"] = []byte("a two\n")
	delta := &deck.Deck{B: baseUUID, C: "delta", D: treeTime.Add(time.Hour),
		F: []deck.FileCard{a2, {Name: "c.txt"}}, P: []string{baseUUID}, U: deck.User("u")}
	delta.R = f.rFor("a.txt", "b.txt")
	rid, _ := f.checkin(delta)

	if got := strings.Join(listedNames(t, f.r, rid), " "); got != "a.txt b.txt" {
		t.Fatalf("ListFiles = %q, want %q", got, "a.txt b.txt")
	}
	for name, want := range map[string]string{"a.txt": a2.UUID, "b.txt": b.UUID, "c.txt": ""} {
		e, found, err := LookupFile(f.r, rid, name)
		if err != nil || found != (want != "") || e.UUID != want {
			t.Errorf("LookupFile(%q) = %q, %v, %v; want %q", name, e.UUID, found, err, want)
		}
	}
	if err := f.r.WithTx(func(tx *db.Tx) error { return VerifyRCard(tx, nil, rid) }); err != nil {
		t.Fatalf("VerifyRCard: %v", err)
	}
}

// §5.1 faults: each tree is rejected by ListFiles, never listed in part.
func TestTreeWalkFaults(t *testing.T) {
	cases := map[string]func(f *treeFixture) []deck.SubmanifestCard{
		"not a sub-manifest": func(f *treeFixture) []deck.SubmanifestCard {
			return []deck.SubmanifestCard{{Name: "a.txt", UUID: f.file("a.txt").UUID}}
		},
		"leaf first name differs": func(f *treeFixture) []deck.SubmanifestCard {
			l := f.leaf(f.file("b.txt"))
			return []deck.SubmanifestCard{{Name: "a.txt", UUID: l.UUID}}
		},
		"interior first name differs": func(f *treeFixture) []deck.SubmanifestCard {
			i := f.interior(f.leaf(f.file("b.txt")))
			return []deck.SubmanifestCard{{Name: "a.txt", UUID: i.UUID}}
		},
		"siblings overlap": func(f *treeFixture) []deck.SubmanifestCard {
			first := f.leaf(f.file("a.txt"), f.file("c.txt"))
			second := f.leaf(f.file("b.txt"))
			return []deck.SubmanifestCard{first, second}
		},
		"deeper than 16": func(f *treeFixture) []deck.SubmanifestCard {
			ref := f.leaf(f.file("a.txt"))
			for depth := 1; depth < maxTreeDepth+1; depth++ {
				ref = f.interior(ref)
			}
			return []deck.SubmanifestCard{ref}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTreeFixture(t)
			uuid := f.store(treeDeck(treeTime, build(f)))
			rid, _ := blob.Exists(f.r.DB(), uuid)
			_, err := ListFiles(f.r, rid)
			var treeErr *SubmanifestError
			if err == nil || !errors.As(err, &treeErr) || treeErr.Missing {
				t.Fatalf("ListFiles = %v, want a tree fault", err)
			}
		})
	}
}

// Sixteen levels, counting the leaf, is the deepest legal tree.
func TestTreeDepthSixteenAccepted(t *testing.T) {
	f := newTreeFixture(t)
	ref := f.leaf(f.file("a.txt"))
	for depth := 1; depth < maxTreeDepth; depth++ {
		ref = f.interior(ref)
	}
	uuid := f.store(treeDeck(treeTime, []deck.SubmanifestCard{ref}))
	rid, _ := blob.Exists(f.r.DB(), uuid)
	if got := strings.Join(listedNames(t, f.r, rid), " "); got != "a.txt" {
		t.Fatalf("ListFiles = %q", got)
	}
	if _, found, err := LookupFile(f.r, rid, "a.txt"); err != nil || !found {
		t.Fatalf("LookupFile = %v, %v", found, err)
	}
}

// The repeated-artifact check of §5.1 step 3. Content addressing makes a real
// cycle unconstructible, so the walk is primed with the frame it would meet.
func TestTreeWalkRefusesRepeatedArtifact(t *testing.T) {
	f := newTreeFixture(t)
	l := f.leaf(f.file("a.txt"))
	rid, _ := blob.Exists(f.r.DB(), l.UUID)
	w := &treeWalk{
		loader: &subtreeLoader{q: f.r.DB()},
		stack:  []treeFrame{{cards: []deck.SubmanifestCard{l}}, {rid: rid, cards: []deck.SubmanifestCard{l}}},
	}
	var treeErr *SubmanifestError
	if err := w.step(); err == nil || !errors.As(err, &treeErr) {
		t.Fatalf("step = %v, want a cycle fault", err)
	}
}

// A missing sub-manifest fails the enumeration entry points hard, defers the
// check-in with an idempotent gap record, and the check-in links once the
// sub-manifest arrives (§5.2, §7.3, §9.4).
func TestTreeMissingSubmanifestDefersThenLinks(t *testing.T) {
	f := newTreeFixture(t)
	a, b := f.file("a.txt"), f.file("b.txt")
	present := f.leaf(a)
	absentDeck := &deck.Deck{F: []deck.FileCard{b}}
	absentBytes := f.marshal(absentDeck)
	absent := deck.SubmanifestCard{Name: "b.txt", UUID: repoHash(f.r, absentBytes)}
	rid, _ := f.checkin(treeDeck(treeTime, []deck.SubmanifestCard{present, absent}))

	if _, err := ListFiles(f.r, rid); err == nil {
		t.Fatal("ListFiles listed a tree with a missing sub-manifest")
	} else if got, ok := MissingSubmanifest(err); !ok || got != absent.UUID {
		t.Fatalf("ListFiles error %v, want the missing %s", err, absent.UUID)
	}
	if _, _, err := LookupFile(f.r, rid, "b.txt"); err == nil {
		t.Fatal("LookupFile resolved a name under a missing sub-manifest")
	}
	if _, found, err := LookupFile(f.r, rid, "a.txt"); err != nil || !found {
		t.Fatalf("LookupFile(a.txt) = %v, %v; its leaf is present", found, err)
	}
	countRows := func(query string, args ...any) int {
		var n int
		if err := f.r.DB().QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countRows("SELECT count(*) FROM event WHERE objid=?", rid); n != 0 {
		t.Fatalf("a deferred tree check-in has %d event rows", n)
	}
	phantom, ok := blob.Exists(f.r.DB(), absent.UUID)
	if !ok {
		t.Fatal("no phantom for the missing sub-manifest")
	}
	orphanQuery := "SELECT count(*) FROM orphan WHERE rid=? AND baseline=?"
	if n := countRows(orphanQuery, rid, phantom); n != 1 {
		t.Fatalf("gap record rows %d, want 1", n)
	}
	if _, err := Crosslink(f.r); err != nil { // a second sweep: still one gap row
		t.Fatal(err)
	}
	if n := countRows(orphanQuery, rid, phantom); n != 1 {
		t.Fatalf("gap record rows after a second sweep %d, want 1", n)
	}

	f.storeBytes(absentBytes)
	AfterDephantomize(t.Context(), f.r, phantom)
	if n := countRows("SELECT count(*) FROM event WHERE objid=?", rid); n != 1 {
		t.Fatalf("event rows after the sub-manifest arrived %d, want 1", n)
	}
	if n := countRows("SELECT count(*) FROM orphan WHERE rid=?", rid); n != 0 {
		t.Fatalf("gap rows after linking %d, want 0", n)
	}
	if got := strings.Join(listedNames(t, f.r, rid), " "); got != "a.txt b.txt" {
		t.Fatalf("ListFiles = %q", got)
	}
}

// §7.1: storing a tree reserves a phantom for each S reference not yet held,
// private when the tree is.
func TestTreeReferencesBecomePhantoms(t *testing.T) {
	f := newTreeFixture(t)
	missing := deck.SubmanifestCard{Name: "z.txt", UUID: repoHash(f.r, []byte("never stored\n"))}
	interior := &deck.Deck{S: []deck.SubmanifestCard{missing}}
	uuid := f.store(interior)
	rid, _ := blob.Exists(f.r.DB(), uuid)
	if err := content.MakePrivate(f.r.DB(), int64(rid)); err != nil {
		t.Fatal(err)
	}
	if _, err := Crosslink(f.r); err != nil {
		t.Fatal(err)
	}
	prid, ok := blob.Exists(f.r.DB(), missing.UUID)
	if !ok {
		t.Fatal("no phantom for the interior node's S reference")
	}
	if !content.IsPrivate(f.r.DB(), int64(prid)) {
		t.Fatal("the phantom of a private node's reference is public")
	}
	seen, err := SeenSubmanifest(f.r.DB())
	if err != nil || !seen {
		t.Fatalf("SeenSubmanifest = %v, %v", seen, err)
	}
	// §9.2: the sub-manifest is filed as examined, so no sweep selects it again.
	var n int
	f.r.DB().QueryRow("SELECT count(*) FROM "+nonArtifactTable+" WHERE rid=?", rid).Scan(&n)
	if n != 1 {
		t.Fatalf("sub-manifest completion marker rows %d, want 1", n)
	}
	candidates, err := collectCrosslinkCandidates(f.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		if c.rid == rid {
			t.Fatal("the sweep selects an examined sub-manifest again")
		}
	}
}

// A check-in whose parent is a tree is written flat, never as a delta
// against the tree (§5.4).
func TestCheckinOnTreeParentIsFlat(t *testing.T) {
	f := newTreeFixture(t)
	a, b := f.file("a.txt"), f.file("b.txt")
	parent, _ := f.checkin(treeDeck(treeTime, []deck.SubmanifestCard{f.leaf(a, b)}))
	rid, _, err := Checkin(f.r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: f.content["a.txt"]},
			{Name: "b.txt", Content: []byte("b two\n")},
		},
		Comment: "on a tree", User: "u", Time: treeTime.Add(time.Hour), Parent: parent, Delta: true,
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	d, err := GetManifest(f.r, rid)
	if err != nil {
		t.Fatal(err)
	}
	if d.B != "" || len(d.S) != 0 || len(d.F) != 2 {
		t.Fatalf("manifest B=%q S=%d F=%d; want a flat two-file manifest", d.B, len(d.S), len(d.F))
	}
}

// A repository whose earlier, pre-sub-manifest sweeps filed blobs as
// unparseable drops that verdict, so a tree is examined again.
func TestNonArtifactTableRevisionDropsOldVerdicts(t *testing.T) {
	r := setupTestRepo(t)
	if _, err := r.DB().Exec("CREATE TABLE crosslink_nonartifact(rid INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := ensureNonArtifactTable(r.DB()); err != nil {
		t.Fatal(err)
	}
	var n int
	r.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='crosslink_nonartifact'").Scan(&n)
	if n != 0 {
		t.Fatal("the superseded verdict table survives")
	}
}
