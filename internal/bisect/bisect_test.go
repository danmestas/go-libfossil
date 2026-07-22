package bisect

import (
	"database/sql"
	"errors"
	"testing"

	libdb "github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// setupBisectDB creates an in-memory SQLite DB with vvar, plink, blob, and
// event tables, then inserts a linear chain 1->2->...->8 with matching
// blob and event rows.
func setupBisectDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(libdb.RegisteredDriver().Name, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE vvar(name TEXT PRIMARY KEY, value CLOB) WITHOUT ROWID`,
		`CREATE TABLE plink(pid INTEGER, cid INTEGER, isprim BOOLEAN, mtime REAL, UNIQUE(pid,cid))`,
		`CREATE INDEX plink_i2 ON plink(cid, pid)`,
		`CREATE TABLE blob(rid INTEGER PRIMARY KEY, uuid TEXT, size INTEGER, content BLOB)`,
		`CREATE TABLE event(type TEXT, mtime REAL, objid INTEGER, tagid INTEGER, uid INTEGER, bgcolor TEXT, euser TEXT, ecomment TEXT, omtime REAL, usedbymerge INTEGER)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}

	// Linear chain: 1->2->3->4->5->6->7->8
	for i := 1; i <= 7; i++ {
		if _, err := db.Exec("INSERT INTO plink(pid,cid,isprim) VALUES(?,?,1)", i, i+1); err != nil {
			t.Fatal(err)
		}
	}

	// blob and event rows for each RID.
	for i := 1; i <= 8; i++ {
		uuid := "abc" + string(rune('0'+i)) + "0000000000000000000000000000000000000000"
		if _, err := db.Exec("INSERT INTO blob(rid,uuid,size) VALUES(?,?,100)", i, uuid); err != nil {
			t.Fatal(err)
		}
		mtime := 2460000.5 + float64(i)
		if _, err := db.Exec("INSERT INTO event(type,mtime,objid) VALUES('ci',?,?)", mtime, i); err != nil {
			t.Fatal(err)
		}
	}

	return db
}

func TestBisectSession(t *testing.T) {
	db := setupBisectDB(t)
	s := NewSession(db)

	if err := s.MarkGood(1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBad(8); err != nil {
		t.Fatal(err)
	}

	mid, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}

	// Midpoint of path [1,2,3,4,5,6,7,8] (len=8) should be path[4] = 5
	// But any value 3-6 is reasonable depending on BFS ordering.
	if mid < 3 || mid > 6 {
		t.Fatalf("expected midpoint in [3,6], got %d", mid)
	}
}

func TestBisectConverges(t *testing.T) {
	db := setupBisectDB(t)
	s := NewSession(db)

	// Bug introduced at commit 5.
	if err := s.MarkGood(1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBad(8); err != nil {
		t.Fatal(err)
	}

	iterations := 0
	for iterations < 10 {
		mid, err := s.Next()
		if err != nil {
			if errors.Is(err, ErrBisectComplete) {
				break
			}
			t.Fatal(err)
		}
		iterations++
		if mid >= 5 {
			if err := s.MarkBad(mid); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := s.MarkGood(mid); err != nil {
				t.Fatal(err)
			}
		}
	}

	if iterations >= 10 {
		t.Fatal("bisect did not converge within 10 iterations")
	}

	// After convergence, bad should be 5.
	st := s.Status()
	if st.Bad != 5 {
		t.Fatalf("expected bad=5, got %d", st.Bad)
	}
}

func TestBisectReset(t *testing.T) {
	db := setupBisectDB(t)
	s := NewSession(db)

	if err := s.MarkGood(1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBad(8); err != nil {
		t.Fatal(err)
	}

	s.Reset()

	st := s.Status()
	if st.Good != 0 || st.Bad != 0 {
		t.Fatalf("expected 0/0 after reset, got %d/%d", st.Good, st.Bad)
	}
	if st.Log != "" {
		t.Fatalf("expected empty log after reset, got %q", st.Log)
	}
}

func TestBisectSkip(t *testing.T) {
	db := setupBisectDB(t)
	s := NewSession(db)

	if err := s.MarkGood(1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBad(8); err != nil {
		t.Fatal(err)
	}

	first, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Skip(first); err != nil {
		t.Fatal(err)
	}

	second, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}

	if second == first {
		t.Fatalf("expected different midpoint after skip, both are %d", first)
	}
}

func TestBisectList(t *testing.T) {
	db := setupBisectDB(t)
	s := NewSession(db)

	if err := s.MarkGood(1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBad(8); err != nil {
		t.Fatal(err)
	}

	entries, err := s.List(libfossil.FslID(3))
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 8 {
		t.Fatalf("expected 8 entries, got %d", len(entries))
	}

	// Check labels on first and last.
	if entries[0].Label != "GOOD" {
		t.Errorf("expected first entry labelled GOOD, got %q", entries[0].Label)
	}
	if entries[len(entries)-1].Label != "BAD" {
		t.Errorf("expected last entry labelled BAD, got %q", entries[len(entries)-1].Label)
	}

	// Check that currentRID=3 is labelled CURRENT.
	found := false
	for _, e := range entries {
		if e.RID == 3 && e.Label == "CURRENT" {
			found = true
		}
	}
	if !found {
		t.Error("expected entry with RID=3 labelled CURRENT")
	}
}
