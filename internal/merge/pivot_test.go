package merge

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// pivotDAG commits check-ins into a new repository, each at its own minute
// of 2026-01-01, so a test controls the event mtimes pivot_find orders by.
type pivotDAG struct {
	t    *testing.T
	r    *repo.Repo
	rids map[string]libfossil.FslID
}

func newPivotDAG(t *testing.T) *pivotDAG {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "pivot.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return &pivotDAG{t: t, r: r, rids: map[string]libfossil.FslID{}}
}

// commit adds check-in name at minute, its first parent the primary one.
func (d *pivotDAG) commit(name string, minute int, parents ...string) {
	d.t.Helper()
	opts := manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "f.txt", Content: []byte(name + "\n")}},
		Comment: name,
		User:    "u",
		Time:    time.Date(2026, 1, 1, 0, minute, 0, 0, time.UTC),
	}
	for i, p := range parents {
		rid, ok := d.rids[p]
		if !ok {
			d.t.Fatalf("parent %s of %s is not committed", p, name)
		}
		if i == 0 {
			opts.Parent = rid
		} else {
			opts.MergeParents = append(opts.MergeParents, rid)
		}
	}
	rid, _, err := manifest.Checkin(d.r, opts)
	if err != nil {
		d.t.Fatalf("checkin %s: %v", name, err)
	}
	d.rids[name] = rid
}

func (d *pivotDAG) name(rid libfossil.FslID) string {
	for n, r := range d.rids {
		if r == rid {
			return n
		}
	}
	return "?"
}

// skewedCrissCross is two merges that cross: p merges b4 into a1 and s
// merges a1 into b3. a1 is one edge from both, b3 one from s and two from
// p, but b3 is newer, so fossil's pivot_find, which expands the newest
// pending check-in first, settles on b3 where a breadth-first walk stops at
// a1.
func skewedCrissCross(t *testing.T) *pivotDAG {
	d := newPivotDAG(t)
	d.commit("r", 1)
	d.commit("a1", 2, "r")
	d.commit("b3", 5, "r")
	d.commit("b4", 6, "b3")
	d.commit("p", 7, "a1", "b4")
	d.commit("s", 8, "b3", "a1")
	return d
}

func TestFindCommonAncestorIsFossilPivot(t *testing.T) {
	d := skewedCrissCross(t)
	for _, pair := range [][2]string{{"p", "s"}, {"s", "p"}} {
		got, err := FindCommonAncestor(d.r, d.rids[pair[0]], d.rids[pair[1]])
		if err != nil {
			t.Fatalf("FindCommonAncestor(%s, %s): %v", pair[0], pair[1], err)
		}
		if d.name(got) != "b3" {
			t.Errorf("FindCommonAncestor(%s, %s) = %s, want b3, the newest common ancestor", pair[0], pair[1], d.name(got))
		}
	}
}

func TestFindPivotIgnoreMergesFollowsPrimaryParents(t *testing.T) {
	d := skewedCrissCross(t)
	// Along primary links p reaches a1 and r, s reaches b3 and r.
	got, err := FindPivot(context.Background(), d.r, PivotQuery{
		Primary: d.rids["p"], Secondaries: []libfossil.FslID{d.rids["s"]}, IgnoreMerges: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.name(got.RID) != "r" {
		t.Fatalf("pivot over primary links = %s, want r", d.name(got.RID))
	}
}

func TestFindPivotSecondariesShareASide(t *testing.T) {
	d := newPivotDAG(t)
	d.commit("r", 1)
	d.commit("x", 2, "r")
	d.commit("m", 3, "x")
	d.commit("v", 4, "r")
	// m and v meet at r; with x merged into v already, m and {v, x} meet at
	// x, as fossil's merge finds with the pending merge's vmerge row.
	got, err := FindPivot(context.Background(), d.r, PivotQuery{
		Primary: d.rids["m"], Secondaries: []libfossil.FslID{d.rids["v"], d.rids["x"]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.name(got.RID) != "x" {
		t.Fatalf("pivot with a merged-in secondary = %s, want x", d.name(got.RID))
	}
}

func TestFindPivotSameCheckinIsItsParent(t *testing.T) {
	d := newPivotDAG(t)
	d.commit("r", 1)
	d.commit("c", 2, "r")
	// pivot_find tests a check-in for a child in the queue from the other
	// side, which c never has: fossil merge-base c c answers r.
	got, err := FindPivot(context.Background(), d.r, PivotQuery{
		Primary: d.rids["c"], Secondaries: []libfossil.FslID{d.rids["c"]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.name(got.RID) != "r" {
		t.Fatalf("pivot of c with itself = %s, want r", d.name(got.RID))
	}
}

func TestFindPivotNoCommonAncestor(t *testing.T) {
	d := newPivotDAG(t)
	d.commit("a", 1)
	d.commit("b", 2)
	_, err := FindPivot(context.Background(), d.r, PivotQuery{
		Primary: d.rids["a"], Secondaries: []libfossil.FslID{d.rids["b"]},
	})
	if !errors.Is(err, ErrNoCommonAncestor) {
		t.Fatalf("disjoint check-ins: %v, want ErrNoCommonAncestor", err)
	}
}

func TestFindPivotBound(t *testing.T) {
	d := skewedCrissCross(t)
	q := PivotQuery{Primary: d.rids["p"], Secondaries: []libfossil.FslID{d.rids["s"]}, Limit: 2}
	if _, err := FindPivot(context.Background(), d.r, q); !errors.Is(err, ErrBound) {
		t.Fatalf("a walk past its bound: %v, want ErrBound", err)
	}
	q.Limit = 64
	if got, err := FindPivot(context.Background(), d.r, q); err != nil || d.name(got.RID) != "b3" {
		t.Fatalf("a walk within its bound = %s, %v; want b3", d.name(got.RID), err)
	}
}

func TestFindPivotReportsUnheldParents(t *testing.T) {
	d := skewedCrissCross(t)
	got, err := FindPivot(context.Background(), d.r, PivotQuery{
		Primary: d.rids["p"], Secondaries: []libfossil.FslID{d.rids["s"]},
	})
	if err != nil || got.Unheld {
		t.Fatalf("a walk over held check-ins: %+v, %v; want Unheld false", got, err)
	}
	// Turn r into a phantom: it has no event, so its mtime counts as 0.
	if _, err := d.r.DB().Exec("DELETE FROM event WHERE objid=?", d.rids["r"]); err != nil {
		t.Fatal(err)
	}
	got, err = FindPivot(context.Background(), d.r, PivotQuery{
		Primary: d.rids["a1"], Secondaries: []libfossil.FslID{d.rids["b3"]},
	})
	if err != nil || d.name(got.RID) != "r" || !got.Unheld {
		t.Fatalf("a walk reaching r without its event: %s %+v, %v; want r, Unheld", d.name(got.RID), got, err)
	}
}
