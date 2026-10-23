package content_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// slotStore is a store of hand-built trees: leaves of F cards, nodes and
// check-ins of S cards, stored and crosslinked one artifact at a time.
type slotStore struct {
	t    *testing.T
	r    *repo.Repo
	when time.Time
	rid  map[string]libfossil.FslID
}

func newSlotStore(t *testing.T) *slotStore {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "slots.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return &slotStore{t: t, r: r, when: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), rid: map[string]libfossil.FslID{}}
}

func (s *slotStore) store(d *deck.Deck) string {
	s.t.Helper()
	data, err := d.Marshal()
	if err != nil {
		s.t.Fatal(err)
	}
	rid, uuid, err := blob.Store(s.r.DB(), data)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := manifest.Crosslink(s.r); err != nil {
		s.t.Fatal(err)
	}
	s.rid[uuid] = rid
	return uuid
}

// leaf stores the files named, each with its own content.
func (s *slotStore) leaf(names ...string) string {
	s.t.Helper()
	d := &deck.Deck{Type: deck.Submanifest}
	for _, name := range names {
		_, uuid, err := blob.Store(s.r.DB(), []byte(name+"\n"))
		if err != nil {
			s.t.Fatal(err)
		}
		d.F = append(d.F, deck.FileCard{Name: name, UUID: uuid})
	}
	return s.store(d)
}

// slots are S cards as name, node pairs.
func slots(pairs ...string) []deck.SubmanifestCard {
	var out []deck.SubmanifestCard
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, deck.SubmanifestCard{Name: pairs[i], UUID: pairs[i+1]})
	}
	return out
}

func (s *slotStore) node(pairs ...string) string {
	return s.store(&deck.Deck{Type: deck.Submanifest, S: slots(pairs...)})
}

func (s *slotStore) checkin(parent string, pairs ...string) string {
	s.when = s.when.Add(time.Hour)
	d := &deck.Deck{Type: deck.Checkin, C: "c", D: s.when, U: deck.User("u"), S: slots(pairs...)}
	if parent != "" {
		d.P = []string{parent}
	}
	return s.store(d)
}

func (s *slotStore) keep(c *cone.Cone, reach map[string]bool) *content.SlotKeep {
	s.t.Helper()
	ctx := context.Background()
	refs, err := content.LoadConeReferences(ctx, s.r.DB(), c)
	if err != nil {
		s.t.Fatal(err)
	}
	k, err := refs.SlotKeep(ctx, c, reach)
	if err != nil {
		s.t.Fatal(err)
	}
	return k
}

func slotCone(t *testing.T, c string, include ...string) *cone.Cone {
	t.Helper()
	h, err := cone.NewHorizon(c, cone.RuleDescendants, cone.DepthContent)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cone.NewWithHorizon(include, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// draft-fossil-horizon-00 §6.2 and zit's sparse.zig walkSlots (3a2df62515):
// a node's last slot takes its bound from its parent. Under c1 the slot of
// n_lib's last card, q/a, runs to c1's next card, r/x, and misses src, so
// l_q is withheld; c2 names n_lib last, the slot runs to the end, reaches
// src, and l_q, older than c2, is kept, voiding every cursor handed out
// between l_q's arrival and c2's (§6.2.6).
func TestSlotKeepWideningKeepsOlderNode(t *testing.T) {
	s := newSlotStore(t)
	lLib, lQ := s.leaf("lib/a"), s.leaf("q/a")
	nLib := s.node("lib/a", lLib, "q/a", lQ)
	lR := s.leaf("r/x")
	c1 := s.checkin("", "lib/a", nLib, "r/x", lR)
	sc := slotCone(t, c1, "lib", "src")

	first := s.keep(sc, nil)
	for uuid, want := range map[string]bool{nLib: false, lLib: false, lR: false, lQ: true} {
		if got := first.Withholds(s.rid[uuid]); got != want {
			t.Errorf("under c1, withholds %s = %v, want %v", uuid[:8], got, want)
		}
	}
	// A cursor handed out once c1 arrived names a store whose walk this one
	// repeats.
	for c := int64(s.rid[c1]); c <= int64(s.rid[c1])+2; c++ {
		if first.Voids(c) {
			t.Fatalf("under c1, cursor %d voided", c)
		}
	}

	c2 := s.checkin(c1, "lib/a", nLib)
	second := s.keep(sc, nil)
	if second.Withholds(s.rid[lQ]) {
		t.Fatal("under c2, l_q is still withheld")
	}
	for c := int64(s.rid[c1]); c <= int64(s.rid[c2]); c++ {
		want := c < int64(s.rid[c2])
		if got := second.Voids(c); got != want {
			t.Errorf("under c2, cursor %d voided = %v, want %v", c, got, want)
		}
	}
}

// A structural horizon keeps only the trees K reaches (§5.1): the slot walk
// starts from those roots alone, so a node only a tree outside them keeps
// is withheld.
func TestSlotKeepStartsFromKeptTrees(t *testing.T) {
	s := newSlotStore(t)
	lOld := s.leaf("src/old")
	c1 := s.checkin("", "src/old", lOld)
	lNew := s.leaf("src/new")
	c2 := s.checkin(c1, "src/new", lNew)
	sc := slotCone(t, c2, "src")
	all := s.keep(sc, nil)
	if all.Withholds(s.rid[lOld]) || all.Withholds(s.rid[lNew]) {
		t.Fatal("every tree kept: an in-cone node is withheld")
	}
	k := s.keep(sc, map[string]bool{c2: true})
	if !k.Withholds(s.rid[lOld]) || k.Withholds(s.rid[lNew]) {
		t.Fatalf("K's trees alone: withholds old=%v new=%v, want true, false",
			k.Withholds(s.rid[lOld]), k.Withholds(s.rid[lNew]))
	}
}
