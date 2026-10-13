package checkout

import (
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// The update target with no version named is fossil's (src/update.c): the
// newest open leaf descended from the checkout, kept to the checkout's
// branch when there are several, refused when that still leaves several.

// featTags start the branch feat on the check-in that carries them.
var featTags = []deck.TagCard{
	{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feat"},
	{Type: deck.TagPropagating, Name: "sym-feat", UUID: "*"},
	{Type: deck.TagCancel, Name: "sym-trunk", UUID: "*"},
}

// updateDAG builds check-ins in a fresh repository; day orders their times.
type updateDAG struct {
	t *testing.T
	r *repo.Repo
}

func newUpdateDAG(t *testing.T) *updateDAG {
	t.Helper()
	r, err := repo.CreateWithEnv(t.TempDir()+"/u.fossil", "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return &updateDAG{t: t, r: r}
}

func (d *updateDAG) commit(name string, day int, parent libfossil.FslID,
	tags ...deck.TagCard) libfossil.FslID {
	d.t.Helper()
	rid, _, err := manifest.Checkin(d.r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: name + ".txt", Content: []byte(name + "\n")}},
		Comment: name,
		User:    "test",
		Parent:  parent,
		Time:    time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC),
		Tags:    tags,
	})
	if err != nil {
		d.t.Fatalf("Checkin %s: %v", name, err)
	}
	return rid
}

// checkoutAt opens a checkout and points it at rid.
func (d *updateDAG) checkoutAt(rid libfossil.FslID) *Checkout {
	d.t.Helper()
	co, err := Create(d.r, d.t.TempDir(), CreateOpts{})
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() { co.Close() })
	var uuid string
	if err := d.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		d.t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout", itoa(int64(rid))); err != nil {
		d.t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout-hash", uuid); err != nil {
		d.t.Fatal(err)
	}
	return co
}

// TestCalcUpdateVersion_KeepsToBranch: c1 has c2 on trunk and b1, newer,
// starting feat. Both are descendant leaves; fossil keeps the one on the
// checkout's branch, c2, where the newest leaf overall is b1.
func TestCalcUpdateVersion_KeepsToBranch(t *testing.T) {
	d := newUpdateDAG(t)
	c1 := d.commit("c1", 1, 0)
	c2 := d.commit("c2", 2, c1)
	d.commit("b1", 3, c1, featTags...)
	got, err := d.checkoutAt(c1).CalcUpdateVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != c2 {
		t.Fatalf("CalcUpdateVersion = %d, want %d (c2, trunk's leaf)", got, c2)
	}
}

// TestCalcUpdateVersion_OnlyDescendants: x, a newer leaf, does not descend
// from c2, the checkout, so update stays on c2.
func TestCalcUpdateVersion_OnlyDescendants(t *testing.T) {
	d := newUpdateDAG(t)
	c1 := d.commit("c1", 1, 0)
	c2 := d.commit("c2", 2, c1)
	d.commit("x", 3, c1, featTags...)
	got, err := d.checkoutAt(c2).CalcUpdateVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("CalcUpdateVersion = %d, want 0 (no descendant of c2)", got)
	}
}

// TestCalcUpdateVersion_StaysOffNewBranch: c1's only child, b1, starts feat,
// so c1 is itself a leaf on trunk beside b2 on feat. Kept to trunk, the
// target is c1, and fossil 2.28's update reports it is already up to date.
func TestCalcUpdateVersion_StaysOffNewBranch(t *testing.T) {
	d := newUpdateDAG(t)
	c1 := d.commit("c1", 1, 0)
	b1 := d.commit("b1", 2, c1, featTags...)
	d.commit("b2", 3, b1)
	got, err := d.checkoutAt(c1).CalcUpdateVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("CalcUpdateVersion = %d, want 0 (c1 is trunk's leaf)", got)
	}
}

// TestCalcUpdateVersion_MultipleDescendants: two trunk leaves descend from
// c1, so the target is underdetermined and fossil refuses.
func TestCalcUpdateVersion_MultipleDescendants(t *testing.T) {
	d := newUpdateDAG(t)
	c1 := d.commit("c1", 1, 0)
	d.commit("a", 2, c1)
	d.commit("b", 3, c1)
	got, err := d.checkoutAt(c1).CalcUpdateVersion()
	if err == nil || !strings.Contains(err.Error(), "multiple descendants") {
		t.Fatalf("CalcUpdateVersion = %d, %v; want a multiple-descendants error", got, err)
	}
}

// TestCalcUpdateVersion_ClosedLeafSkipped: of c1's two trunk leaves, b is
// closed, so the open a is the only candidate. With every leaf closed,
// fossil falls back to the closed ones.
func TestCalcUpdateVersion_ClosedLeafSkipped(t *testing.T) {
	d := newUpdateDAG(t)
	c1 := d.commit("c1", 1, 0)
	a := d.commit("a", 2, c1)
	b := d.commit("b", 3, c1, deck.TagCard{Type: deck.TagSingleton, Name: "closed", UUID: "*"})
	got, err := d.checkoutAt(c1).CalcUpdateVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != a {
		t.Fatalf("CalcUpdateVersion = %d, want %d (a, the open leaf)", got, a)
	}
	c := d.commit("c", 4, b, deck.TagCard{Type: deck.TagSingleton, Name: "closed", UUID: "*"})
	got, err = d.checkoutAt(b).CalcUpdateVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("CalcUpdateVersion = %d, want %d (c, closed but the only leaf)", got, c)
	}
}

// TestCalcUpdateVersion_UntaggedCheckoutOnMainBranch: c1's branch rows are
// removed, so Section 6.1 puts it on trunk, the default branch, with both its
// leaves: the target is underdetermined. fossil compares c1's missing row,
// keeps neither leaf and stays on c1.
func TestCalcUpdateVersion_UntaggedCheckoutOnMainBranch(t *testing.T) {
	d := newUpdateDAG(t)
	c1 := d.commit("c1", 1, 0)
	d.commit("a", 2, c1)
	d.commit("b", 3, c1)
	if _, err := d.r.DB().Exec(`DELETE FROM tagxref WHERE rid = ?
		AND tagid = (SELECT tagid FROM tag WHERE tagname = 'branch')`, int64(c1)); err != nil {
		t.Fatal(err)
	}
	got, err := d.checkoutAt(c1).CalcUpdateVersion()
	if err == nil || !strings.Contains(err.Error(), `branch "trunk"`) {
		t.Fatalf("CalcUpdateVersion = %d, %v; want a multiple-descendants error on trunk",
			got, err)
	}
}

// dropBranch removes rid's branch rows, leaving it with no active branch tag.
func (d *updateDAG) dropBranch(rid libfossil.FslID) {
	d.t.Helper()
	if _, err := d.r.DB().Exec(`DELETE FROM tagxref WHERE rid = ?
		AND tagid = (SELECT tagid FROM tag WHERE tagname = 'branch')`, int64(rid)); err != nil {
		d.t.Fatal(err)
	}
}

// TestCalcUpdateVersion_UntaggedLeafCountsOnMainBranch: the checkout is on
// trunk and one of its two leaves has no branch row. The same-branch tests
// and the branch filter read that leaf as trunk's alike (Section 6.1), so
// the target is underdetermined rather than the tagged leaf alone.
func TestCalcUpdateVersion_UntaggedLeafCountsOnMainBranch(t *testing.T) {
	d := newUpdateDAG(t)
	c1 := d.commit("c1", 1, 0)
	d.commit("a", 2, c1)
	b := d.commit("b", 3, c1)
	d.dropBranch(b)
	got, err := d.checkoutAt(c1).CalcUpdateVersion()
	if err == nil || !strings.Contains(err.Error(), "multiple descendants") {
		t.Fatalf("CalcUpdateVersion = %d, %v; want a multiple-descendants error", got, err)
	}
}

// TestCalcUpdateVersion_AllUntaggedIsMultiple: with no branch rows anywhere
// the whole graph reads as the default branch, so two leaves are refused
// rather than the checkout staying put.
func TestCalcUpdateVersion_AllUntaggedIsMultiple(t *testing.T) {
	d := newUpdateDAG(t)
	c1 := d.commit("c1", 1, 0)
	a := d.commit("a", 2, c1)
	b := d.commit("b", 3, c1)
	d.dropBranch(c1)
	d.dropBranch(a)
	d.dropBranch(b)
	got, err := d.checkoutAt(c1).CalcUpdateVersion()
	if err == nil || !strings.Contains(err.Error(), "multiple descendants") {
		t.Fatalf("CalcUpdateVersion = %d, %v; want a multiple-descendants error", got, err)
	}
}
