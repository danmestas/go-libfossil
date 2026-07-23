package path

import (
	"database/sql"
	"testing"

	libdb "github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// buildPlink creates an in-memory SQLite DB with plink table and populates it.
// Each edge is [3]interface{}{pid, cid, isprim}.
func buildPlink(t *testing.T, edges [][3]int) *sql.DB {
	t.Helper()
	db, err := sql.Open(libdb.RegisteredDriver().Name, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE plink(
		pid INTEGER, cid INTEGER, isprim BOOLEAN, mtime REAL,
		UNIQUE(pid, cid));
		CREATE INDEX plink_i2 ON plink(cid, pid)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		_, err := db.Exec("INSERT INTO plink(pid, cid, isprim) VALUES(?,?,?)", e[0], e[1], e[2])
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLinearChain(t *testing.T) {
	// 1->2->3->4->5
	db := buildPlink(t, [][3]int{
		{1, 2, 1}, {2, 3, 1}, {3, 4, 1}, {4, 5, 1},
	})
	path, err := Shortest(db, 1, 5, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 5 {
		t.Fatalf("expected length 5, got %d: %v", len(path), rids(path))
	}
	expect := []libfossil.FslID{1, 2, 3, 4, 5}
	for i, n := range path {
		if n.RID != expect[i] {
			t.Errorf("path[%d] = %d, want %d", i, n.RID, expect[i])
		}
	}
}

func TestDiamondMerge(t *testing.T) {
	// 1->2(prim), 1->3(merge), 2->4(prim), 3->4(merge)
	db := buildPlink(t, [][3]int{
		{1, 2, 1}, {1, 3, 0}, {2, 4, 1}, {3, 4, 0},
	})
	path, err := Shortest(db, 1, 4, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 {
		t.Fatalf("expected length 3, got %d: %v", len(path), rids(path))
	}
}

func TestDirectOnly(t *testing.T) {
	// 1->2(prim), 1->3(merge), 3->4(prim)
	db := buildPlink(t, [][3]int{
		{1, 2, 1}, {1, 3, 0}, {3, 4, 1},
	})

	// directOnly=true: can't cross 1->3 merge edge, so no path 1->4
	_, err := Shortest(db, 1, 4, true, nil)
	if err != ErrNoPath {
		t.Fatalf("expected ErrNoPath with directOnly=true, got %v", err)
	}

	// directOnly=false: 1->3->4 works
	path, err := Shortest(db, 1, 4, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 {
		t.Fatalf("expected length 3, got %d: %v", len(path), rids(path))
	}
}

func TestSkipSet(t *testing.T) {
	// 1->2->3->4, skip node 3
	db := buildPlink(t, [][3]int{
		{1, 2, 1}, {2, 3, 1}, {3, 4, 1},
	})
	skip := map[libfossil.FslID]bool{3: true}
	_, err := Shortest(db, 1, 4, false, skip)
	if err != ErrNoPath {
		t.Fatalf("expected ErrNoPath with skip={3}, got %v", err)
	}
}

func TestNoPath(t *testing.T) {
	// disconnected: 1->2, 3->4
	db := buildPlink(t, [][3]int{
		{1, 2, 1}, {3, 4, 1},
	})
	_, err := Shortest(db, 1, 4, false, nil)
	if err != ErrNoPath {
		t.Fatalf("expected ErrNoPath, got %v", err)
	}
}

func TestReversePath(t *testing.T) {
	// 1->2->3, find 3 to 1 (walk backwards via reverse edges)
	db := buildPlink(t, [][3]int{
		{1, 2, 1}, {2, 3, 1},
	})
	path, err := Shortest(db, 3, 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 {
		t.Fatalf("expected length 3, got %d: %v", len(path), rids(path))
	}
	expect := []libfossil.FslID{3, 2, 1}
	for i, n := range path {
		if n.RID != expect[i] {
			t.Errorf("path[%d] = %d, want %d", i, n.RID, expect[i])
		}
	}
}

func TestSameNode(t *testing.T) {
	db := buildPlink(t, [][3]int{{1, 2, 1}})
	path, err := Shortest(db, 1, 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 1 || path[0].RID != 1 {
		t.Fatalf("expected single node [1], got %v", rids(path))
	}
}

func rids(path []PathNode) []libfossil.FslID {
	out := make([]libfossil.FslID, len(path))
	for i, n := range path {
		out[i] = n.RID
	}
	return out
}
