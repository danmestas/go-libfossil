package vfile

import (
	"database/sql"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestClassify pins each row shape the fossil binary and go-libfossil write
// to its change type, in fossil's priority order.
func TestClassify(t *testing.T) {
	renamed := sql.NullString{String: "old.txt", Valid: true}
	same := sql.NullString{String: "f.txt", Valid: true}
	cases := []struct {
		name    string
		row     Row
		missing bool
		want    Change
	}{
		{"unchanged", Row{Pathname: "f.txt", RID: 2}, false, ChangeNone},
		{"origname equals pathname", Row{Pathname: "f.txt", RID: 2, Origname: same}, false, ChangeNone},
		{"fossil add, chnged 0", Row{Pathname: "f.txt"}, false, ChangeAdded},
		{"edit", Row{Pathname: "f.txt", RID: 2, Chnged: 1}, false, ChangeModified},
		{"merge outcome", Row{Pathname: "f.txt", RID: 2, Chnged: 2}, false, ChangeModified},
		{"rename", Row{Pathname: "f.txt", RID: 2, Origname: renamed}, false, ChangeRenamed},
		{"rename and edit",
			Row{Pathname: "f.txt", RID: 2, Origname: renamed, Chnged: 1}, false, ChangeRenamed},
		{"removed beats missing", Row{Pathname: "f.txt", RID: 2, Deleted: 1}, true, ChangeRemoved},
		{"missing beats added", Row{Pathname: "f.txt"}, true, ChangeMissing},
		{"missing beats edit", Row{Pathname: "f.txt", RID: 2, Chnged: 1}, true, ChangeMissing},
	}
	for _, tc := range cases {
		if got := Classify(tc.row, tc.missing); got != tc.want {
			t.Errorf("%s: Classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRowQuestions pins the predicates callers use instead of reading
// columns.
func TestRowQuestions(t *testing.T) {
	add := Row{Pathname: "a"}
	if !add.IsAdded() || !add.ContentChanged() {
		t.Error("an add (rid=0, chnged=0) must be added with changed content")
	}
	merged := Row{Pathname: "m", RID: 2, Chnged: 4}
	if !merged.PendingMerge() || !merged.ContentChanged() {
		t.Error("chnged=4 must be a pending merge with changed content")
	}
	edit := Row{Pathname: "e", RID: 2, Chnged: 1}
	if edit.PendingMerge() {
		t.Error("a plain edit is not a pending merge")
	}
	if (Row{Pathname: "r", Origname: sql.NullString{Valid: true, String: "r"}}).RenamedFrom() != "" {
		t.Error("origname equal to pathname is not a rename")
	}
}

// TestLoadOrdersByPathnameAndFiltersVersion pins Load against a real vfile
// table: only the requested version's rows, sorted by pathname, with NULL
// origname read as invalid.
func TestLoadOrdersByPathnameAndFiltersVersion(t *testing.T) {
	d, err := db.OpenSQL(t.TempDir()+"/ck.db", db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	for _, stmt := range []string{
		`CREATE TABLE vfile(id INTEGER PRIMARY KEY, vid INTEGER, chnged INT DEFAULT 0,
			deleted BOOLEAN DEFAULT 0, isexe BOOLEAN, islink BOOLEAN, rid INTEGER,
			mrid INTEGER, mtime INTEGER, pathname TEXT, origname TEXT, mhash TEXT)`,
		`INSERT INTO vfile(vid, pathname, rid, isexe, islink) VALUES(7, 'b.txt', 3, 0, 0)`,
		`INSERT INTO vfile(vid, pathname, rid, isexe, islink) VALUES(7, 'a.txt', 2, 1, 0)`,
		`INSERT INTO vfile(vid, pathname, rid, isexe, islink) VALUES(8, 'c.txt', 4, 0, 0)`,
	} {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := Load(d, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("Load returned %d rows, want 2", len(rows))
	}
	if rows[0].Pathname != "a.txt" || rows[1].Pathname != "b.txt" {
		t.Fatalf("Load order = %s, %s; want a.txt, b.txt", rows[0].Pathname, rows[1].Pathname)
	}
	if rows[0].IsExe != 1 || rows[0].Origname.Valid {
		t.Fatalf("a.txt = %+v, want isexe 1 and NULL origname", rows[0])
	}
}
