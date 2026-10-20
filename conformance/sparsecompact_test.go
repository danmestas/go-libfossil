package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
)

// These exercise Appendix A step 5 against real flat and tree histories. The
// expected keep set is computed from the fixture's paths, not the compactor.
func scCompactCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.5.4.compact-flat", Req: "SC-5.4-2"}, func(t *testing.T) error { return scCompactHistory(t, "flat") }},
		{Case{ID: "sparsecone.5.4.compact-tree", Req: "SC-5.4-2"}, func(t *testing.T) error { return scCompactHistory(t, "tree") }},
		{Case{ID: "sparsecone.5.4.compact-hybrid", Req: "SC-5.4-2"}, func(t *testing.T) error { return scCompactHistory(t, "hybrid") }},
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
