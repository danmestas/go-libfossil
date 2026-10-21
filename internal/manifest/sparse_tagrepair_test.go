package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

// A pretagged partial check-in must recheck its merge parent as well as its
// primary parent; waiting for file derivation would expose a stale leaf.
func TestPartialLinkRechecksPretaggedParents(t *testing.T) {
	f := newTreeFixture(t)
	r := f.r
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	q := storeCheckin(t, r, "q", "trunk", base)
	p := storeCheckin(t, r, "p", "feature", base.Add(time.Minute), q)
	if _, err := Crosslink(r); err != nil {
		t.Fatal(err)
	}
	assertLeaves(t, r, "before partial child", []string{"p", "q"})
	inside := f.leaf(f.file("src/a"))
	outside := f.leaf(f.file("vendor/a"))
	child := treeDeck(base.Add(2*time.Minute), []deck.SubmanifestCard{inside, outside})
	child.C, child.P = "c", []string{q, p}
	uuid := f.store(child)
	rid := ridOf(t, r, uuid)
	data, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	elideWriterNode(t, r, uuid)
	elideWriterNode(t, r, outside.UUID)
	f.store(&deck.Deck{Type: deck.Control, D: base.Add(time.Hour), U: deck.User("testuser"),
		T: []deck.TagCard{{Type: deck.TagPropagating, Name: "branch", UUID: uuid, Value: "feature"}}})
	if _, err := Crosslink(r); err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	if _, _, err := blob.Store(r.DB(), data); err != nil {
		t.Fatal(err)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatal(err)
	}
	var events, links, gaps int
	if err := r.DB().QueryRow(`SELECT
		(SELECT count(*) FROM event WHERE objid=?),
		(SELECT count(*) FROM mlink WHERE mid=?),
		(SELECT count(*) FROM orphan WHERE rid=?)`, rid, rid, rid).
		Scan(&events, &links, &gaps); err != nil {
		t.Fatal(err)
	}
	if events != 1 || links != 0 || gaps != 1 {
		t.Fatalf("partial state: events=%d links=%d gaps=%d", events, links, gaps)
	}
	assertLeaves(t, r, "after partial child", []string{"c", "q"})
}
