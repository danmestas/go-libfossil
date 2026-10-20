package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// These exercise Appendix A step 5 against real flat and tree histories. The
// expected keep set is computed from the fixture's paths, not the compactor.
func scCompactCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.5.4.compact-flat", Req: "SC-5.4-2"}, func(t *testing.T) error { return scCompactHistory(t, "flat") }},
		{Case{ID: "sparsecone.5.4.compact-tree", Req: "SC-5.4-2"}, func(t *testing.T) error { return scCompactHistory(t, "tree") }},
		{Case{ID: "sparsecone.5.4.compact-hybrid", Req: "SC-5.4-2"}, func(t *testing.T) error { return scCompactHistory(t, "hybrid") }},
		{Case{ID: "sparsecone.5.4.compact-partial-tree", Req: "SC-5.4-2"}, scCompactPartialTree},
	}
}

func TestSparseCompaction(t *testing.T) {
	for _, c := range scCompactCases() {
		check(t, c.Case, func() error { return c.run(t) })
	}
}

func scCompactHoldings(t *testing.T, r *repo.Repo) string {
	t.Helper()
	var result string
	err := r.DB().QueryRow(`SELECT coalesce(group_concat(rid || ':' || size || ':' || hex(content)), '')
 FROM (SELECT rid, size, content FROM blob ORDER BY rid)`).Scan(&result)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func scCompactHistory(t *testing.T, mode string) error {
	origin := newSCRepo(t, mode == "tree")
	if mode == "hybrid" {
		if err := origin.r.SetConfig(manifest.SettingEnableSubmanifests, "on"); err != nil {
			return err
		}
		if err := origin.r.SetConfig(manifest.SettingSubmanifestK, "1"); err != nil {
			return err
		}
	}
	// A historical in-cone name keeps its bytes even after a later rename moves
	// the only current alias outside the cone.
	later := map[string]string{}
	for name, data := range scFiles {
		later[name] = data
	}
	delete(later, "src/a.c")
	later["tools/renamed.c"] = scFiles["src/a.c"]
	origin.commit(t, later, origin.c1)
	s := &scSparse{origin: origin, gimmes: map[string]bool{}}
	if _, err := origin.r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		return err
	}
	clone, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "compact.fossil"), s.transport(origin.r), sync.CloneOpts{})
	if err != nil {
		return err
	}
	defer clone.Close()
	before := scCompactHoldings(t, clone)
	if err := content.SetStoreCone(clone.DB(), scDefault(t)); err != nil {
		return err
	}
	if after := scCompactHoldings(t, clone); after != before {
		return expectf("narrowing changed artifact holdings")
	}
	n, err := content.CompactStore(context.Background(), clone.DB())
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for path := range scFiles {
		if !scDefault(t).Covers(path) {
			wanted[scUUIDOf(t, origin.r, origin.c1, path)] = true
		}
	}
	if n != len(wanted) {
		return expectf("compacted %d artifacts; want %d", n, len(wanted))
	}
	classes := scAbsences(t, clone)
	if err := scSameList("compacted elided names", scClassed(classes, content.Elided), scSorted(wanted)); err != nil {
		return err
	}
	if len(classes) != len(wanted) {
		return expectf("unexpected phantom class: %v", classes)
	}
	if _, err := scVerify(clone); err != nil {
		return err
	}
	if n, err := content.CompactStore(context.Background(), clone.DB()); err != nil || n != 0 {
		return fmt.Errorf("repeat compact: %d, %v", n, err)
	}
	return scCompactHeal(t, s, clone, mode == "tree")
}

// No crosslink exists for this tree: the outside sibling is truly missing.
// Raw proofs must survive compaction, including stale derived file indexes.
func scCompactPartialTree(t *testing.T) error {
	origin := newSCRepo(t, true)
	s := &scSparse{origin: origin, gimmes: map[string]bool{}}
	if _, err := origin.r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		return err
	}
	r, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "partial.fossil"),
		s.transport(origin.r), sync.CloneOpts{})
	if err != nil {
		return err
	}
	defer r.Close()
	if err := content.SetStoreCone(r.DB(), scCone(t, []string{"src"}, nil)); err != nil {
		return err
	}
	if _, err := content.CompactStore(context.Background(), r.DB()); err != nil {
		return err
	}
	inside, insideName, err := blob.Store(r.DB(), []byte("partial compact inside"))
	if err != nil {
		return err
	}
	outside, outsideName, err := blob.Store(r.DB(), []byte("partial compact outside"))
	if err != nil {
		return err
	}
	put := func(d *deck.Deck) string {
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		_, uuid, err := blob.Store(r.DB(), data)
		if err != nil {
			t.Fatal(err)
		}
		return uuid
	}
	inLeaf := put(&deck.Deck{Type: deck.Submanifest, F: []deck.FileCard{{Name: "src/compact", UUID: insideName}}})
	outLeaf := put(&deck.Deck{Type: deck.Submanifest, F: []deck.FileCard{{Name: "zzz/compact", UUID: outsideName}}})
	outNode := put(&deck.Deck{Type: deck.Submanifest, S: []deck.SubmanifestCard{{Name: "zzz/compact", UUID: outLeaf}}})
	missingName := strings.Repeat("a", 40)
	missing, err := blob.StorePhantom(r.DB(), missingName)
	if err != nil {
		return err
	}
	missingChild := put(&deck.Deck{Type: deck.Submanifest, S: []deck.SubmanifestCard{
		{Name: "zzzz/compact", UUID: missingName},
	}})
	missingParent := put(&deck.Deck{Type: deck.Submanifest, S: []deck.SubmanifestCard{
		{Name: "zzzz/compact", UUID: missingChild},
	}})
	put(&deck.Deck{Type: deck.Checkin, D: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), S: []deck.SubmanifestCard{
		{Name: "src/compact", UUID: inLeaf}, {Name: "zzz/compact", UUID: outNode}, {Name: "zzzz/compact", UUID: missingParent},
	}})
	// A disposable stale link falsely calls the covered blob outside.
	if _, err := r.DB().Exec("INSERT INTO filename(name) VALUES('zzz/stale-compact')"); err != nil {
		return err
	}
	if _, err := r.DB().Exec("INSERT INTO mlink(mid,fid,fnid) SELECT 999,?,fnid FROM filename WHERE name='zzz/stale-compact'", inside); err != nil {
		return err
	}
	if n, err := content.CompactStore(context.Background(), r.DB()); err != nil || n != 2 {
		return fmt.Errorf("partial compact = %d, %v; want 2, nil", n, err)
	}
	return scCompactPartialRebuild(t, r, s, [3]libfossil.FslID{inside, outside, missing},
		[3]string{outsideName, missingName, outNode}, outLeaf)
}

func scCompactPartialRebuild(t *testing.T, r *repo.Repo, s *scSparse,
	rids [3]libfossil.FslID, names [3]string, outLeaf string,
) error {
	inside, outside, missing := rids[0], rids[1], rids[2]
	outsideName, missingName, outNode := names[0], names[1], names[2]
	if err := scCompactPartialProof(r, inside, outside, missing, outNode, outLeaf); err != nil {
		return err
	}
	before := scCompactHoldings(t, r)
	if _, err := verify.Rebuild(r); err != nil {
		return err
	}
	if scCompactHoldings(t, r) != before {
		return expectf("rebuild changed partial-tree artifact holdings")
	}
	if err := scCompactPartialProof(r, inside, outside, missing, outNode, outLeaf); err != nil {
		return err
	}
	s.gimmes = map[string]bool{}
	if err := s.sync(t, r, s.origin.r, false, true); err != nil {
		return err
	}
	for _, uuid := range []string{outsideName, missingName, outNode} {
		if s.gimmes[uuid] {
			return expectf("sync requested rebuilt elided artifact %s", uuid)
		}
	}
	return scCompactPartialProof(r, inside, outside, missing, outNode, outLeaf)
}

func scCompactPartialProof(r *repo.Repo, inside, outside, missing libfossil.FslID, outNode, outLeaf string) error {
	if _, err := content.Expand(r.DB(), inside); err != nil {
		return err
	}
	var leaf libfossil.FslID
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", outLeaf).Scan(&leaf); err != nil {
		return err
	}
	if _, err := content.Expand(r.DB(), leaf); err != nil {
		return err
	}
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		return err
	}
	var node libfossil.FslID
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", outNode).Scan(&node); err != nil {
		return err
	}
	for _, rid := range []libfossil.FslID{outside, missing, node} {
		if a.Class(rid) != content.Elided {
			return expectf("partial compact artifact %d is not elided", rid)
		}
	}
	if n, err := content.CompactStore(context.Background(), r.DB()); err != nil || n != 0 {
		return fmt.Errorf("repeat partial compact = %d, %v", n, err)
	}
	return nil
}

func scCompactHeal(t *testing.T, s *scSparse, clone *repo.Repo, losePolicy bool) error {
	var wide *cone.Cone
	if !losePolicy {
		wide = scCone(t, []string{"README", "docs", "src", "src.txt", "src-x", "tools"}, nil)
	}
	// Widening and policy loss both make ordinary sync heal compacted names.
	if err := content.SetStoreCone(clone.DB(), wide); err != nil {
		return err
	}
	if err := s.sync(t, clone, s.origin.r, false, true); err != nil {
		return err
	}
	if got := scAbsences(t, clone); len(got) != 0 {
		return expectf("dense pull left absences: %v", got)
	}
	_, err := scVerify(clone)
	return err
}
