package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

type sparseParentArrival struct {
	f        *treeFixture
	child    string
	payloads [][]byte
}

func newSparseParentArrival(t *testing.T) sparseParentArrival {
	t.Helper()
	f := newTreeFixture(t)
	inside := f.file("src/a")
	outside := f.leaf(f.file("vendor/a"))
	refs := []deck.SubmanifestCard{f.leaf(inside), outside}
	pd := treeDeck(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), refs)
	pd.T = []deck.TagCard{{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}}
	parent := f.store(pd)
	child := f.store(&deck.Deck{
		C: "child", D: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), U: deck.User("u"),
		P: []string{parent}, F: []deck.FileCard{inside},
	})
	var payloads [][]byte
	for _, uuid := range []string{parent, outside.UUID} {
		rid, _ := content.AvailableByUUID(f.r.DB(), uuid)
		data, err := content.Expand(f.r.DB(), rid)
		if err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, data)
		elideWriterNode(t, f.r, uuid)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), c); err != nil {
		t.Fatal(err)
	}
	return sparseParentArrival{f: f, child: child, payloads: payloads}
}

// Restore the parent spine and its outside subtree in both orders, then compare
// the completed derivatives to a dense rebuild to detect stale partial rows.
func TestSparseParentAndSubtreeArrivalOrders(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent-first", true: "node-first"}[reverse], func(t *testing.T) {
			f := newSparseParentArrival(t)
			if _, err := Crosslink(f.f.r); err != nil {
				t.Fatal(err)
			}
			// The parent is absent, so the child's spine is published against
			// its phantom while its orphan row keeps the mlinks waiting.
			f.assertChild(t, 1, 0, 1)
			for i := 0; i < 2; i++ {
				j := i
				if reverse {
					j = 1 - i
				}
				f.f.storeBytes(f.payloads[j])
				if _, err := Crosslink(f.f.r); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					f.assertChild(t, 1, 0, 1)
				}
			}
			f.assertChild(t, 1, 1, 0)
			f.assertCompleteGraph(t)
			for _, payload := range f.payloads {
				f.f.storeBytes(payload)
			}
			if n, err := Crosslink(f.f.r); err != nil || n != 0 {
				t.Fatalf("duplicate arrivals = %d, %v", n, err)
			}
			want := dumpDerivedRows(t, f.f.r)
			rebuildSparseDerived(t, f.f)
			if got := dumpDerivedRows(t, f.f.r); got != want {
				t.Fatalf("parent arrival derivation diverged from dense rebuild:\n%s\nwant:\n%s", got, want)
			}
			f.assertChild(t, 1, 1, 0)
			f.assertCompleteGraph(t)
		})
	}
}

func (f sparseParentArrival) assertChild(t *testing.T, events, mlinks, orphans int) {
	t.Helper()
	rid, _ := content.AvailableByUUID(f.f.r.DB(), f.child)
	for _, check := range []struct {
		query string
		want  int
	}{
		{"SELECT count(*) FROM event WHERE objid=?", events},
		{"SELECT count(*) FROM mlink WHERE mid=?", mlinks},
		{"SELECT count(*) FROM orphan WHERE rid=?", orphans},
	} {
		var n int
		if err := f.f.r.DB().QueryRow(check.query, rid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != check.want {
			t.Fatalf("%s = %d, want %d", check.query, n, check.want)
		}
	}
}

func (f sparseParentArrival) assertCompleteGraph(t *testing.T) {
	t.Helper()
	rid, _ := content.AvailableByUUID(f.f.r.DB(), f.child)
	for _, query := range []string{
		"SELECT count(*) FROM plink WHERE cid=?",
		"SELECT count(*) FROM leaf WHERE rid=?",
		`SELECT count(*) FROM tagxref x JOIN tag t ON t.tagid=x.tagid
		 WHERE x.rid=? AND t.tagname='branch' AND x.value='trunk'`,
	} {
		var n int
		if err := f.f.r.DB().QueryRow(query, rid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%s = %d, want 1", query, n)
		}
	}
	var leaves int
	if err := f.f.r.DB().QueryRow("SELECT count(*) FROM leaf").Scan(&leaves); err != nil {
		t.Fatal(err)
	}
	if leaves != 1 {
		t.Fatalf("complete graph leaves = %d, want only child", leaves)
	}
}

func rebuildSparseDerived(t *testing.T, f *treeFixture) {
	t.Helper()
	if err := f.r.WithTx(func(tx *db.Tx) error {
		for _, table := range []string{"event", "plink", "mlink", "tagxref", "leaf", "orphan"} {
			if _, err := tx.Exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
		_, err := CrosslinkRebuild(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
