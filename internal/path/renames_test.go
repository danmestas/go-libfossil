package path

import (
	"database/sql"
	"reflect"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// buildRenames adds fossil's mlink and filename tables to a plink graph.
// Each rename is {mid, pfnid, fnid}, fnid 0 for a deletion; filename fnid n
// is named by names[n].
func buildRenames(t *testing.T, edges [][3]int, names []string, renames [][3]int) *sql.DB {
	t.Helper()
	db := buildPlink(t, edges)
	if _, err := db.Exec(`CREATE TABLE filename(fnid INTEGER PRIMARY KEY, name TEXT UNIQUE);
		CREATE TABLE mlink(mid INTEGER, fid INTEGER, pfnid INTEGER, fnid INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for fnid, name := range names {
		if fnid == 0 {
			continue
		}
		if _, err := db.Exec("INSERT INTO filename(fnid, name) VALUES(?, ?)", fnid, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range renames {
		fid, pfnid, fnid := 1, r[1], r[2]
		if fnid == 0 {
			fid, pfnid, fnid = 0, 0, r[1]
		}
		if _, err := db.Exec("INSERT INTO mlink(mid, fid, pfnid, fnid) VALUES(?, ?, ?, ?)",
			r[0], fid, pfnid, fnid); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestFilenameChanges(t *testing.T) {
	// 1 -> 2 -> 3 on the primary line, 1 -> 4 beside it. 2 renames a to
	// b, 3 renames b to c and d to e, 4 renames a to f.
	names := []string{"", "a", "b", "c", "d", "e", "f"}
	db := buildRenames(t, [][3]int{{1, 2, 1}, {2, 3, 1}, {1, 4, 1}}, names,
		[][3]int{{2, 1, 2}, {3, 2, 3}, {3, 4, 5}, {4, 1, 6}})
	cases := []struct {
		from, to libfossil.FslID
		revOK    bool
		want     []NameChange
	}{
		{1, 3, false, []NameChange{{"d", "e"}, {"a", "c"}}},
		{1, 3, true, []NameChange{{"d", "e"}, {"a", "c"}}},
		{2, 3, false, []NameChange{{"d", "e"}, {"b", "c"}}},
		// Backwards only when revOK, as find_filename_changes' one-way path.
		{3, 1, false, nil},
		{3, 1, true, []NameChange{{"e", "d"}, {"c", "a"}}},
		{3, 4, false, nil},
		{3, 4, true, []NameChange{{"e", "d"}, {"c", "f"}}},
		{3, 3, true, nil},
	}
	for _, tc := range cases {
		got, err := FilenameChanges(db, tc.from, tc.to, tc.revOK)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("FilenameChanges(%d, %d, %v) = %v, want %v", tc.from, tc.to, tc.revOK, got, tc.want)
		}
	}
}

// A name a file leaves can be taken by another file that is renamed in
// turn: both changes start from the same name, newest first, as fossil
// lists them.
func TestFilenameChangesReusedName(t *testing.T) {
	names := []string{"", "a", "b", "c"}
	db := buildRenames(t, [][3]int{{1, 2, 1}, {2, 3, 1}}, names,
		[][3]int{{2, 1, 2}, {3, 1, 3}})
	got, err := FilenameChanges(db, 1, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []NameChange{{"a", "c"}, {"a", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilenameChanges = %v, want %v", got, want)
	}
}

// A file deleted on the way drops out.
func TestFilenameChangesDeleted(t *testing.T) {
	names := []string{"", "a", "b"}
	db := buildRenames(t, [][3]int{{1, 2, 1}, {2, 3, 1}}, names,
		[][3]int{{2, 1, 2}, {3, 2, 0}})
	got, err := FilenameChanges(db, 1, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("FilenameChanges = %v, want none", got)
	}
}
