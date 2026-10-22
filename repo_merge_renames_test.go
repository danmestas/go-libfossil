package libfossil

import (
	"reflect"
	"testing"
)

// renameMergeRepo commits base, f.txt alone, then feature from it and trunk
// from it with their own files (Files is the whole tree), and merges
// feature into trunk. It returns the merge's files, name to content, and
// the renames its manifest records, new name to old.
func renameMergeRepo(t *testing.T, feature, trunk []FileToCommit) (map[string]string, map[string]string) {
	t.Helper()
	r := newTestRepo(t)
	base := commit(t, r, 0, "base", []FileToCommit{{Name: "f.txt", Content: []byte("1\nx\nx\n2\n")}})
	branchHead := makeBranch(t, r, base, "feature")
	for _, c := range []struct {
		parent int64
		files  []FileToCommit
	}{{branchHead, feature}, {base, trunk}} {
		if _, _, err := r.Commit(CommitOpts{
			ParentID: c.parent, Files: c.files, Comment: "edit", User: "test", PartialManifest: true,
		}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	rid, _, err := r.Merge("feature", "trunk", "merge", "test")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	files, err := r.ListFiles(rid)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Name] = string(readBlob(t, r, rid, f.Name))
	}
	rows, err := r.DB().Query(`SELECT n.name, p.name FROM mlink
		JOIN filename n ON n.fnid=mlink.fnid JOIN filename p ON p.fnid=mlink.pfnid
		WHERE mid=? AND pfnid>0`, rid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	renames := map[string]string{}
	for rows.Next() {
		var to, from string
		if err := rows.Scan(&to, &from); err != nil {
			t.Fatal(err)
		}
		renames[to] = from
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got, renames
}

// Merge follows a rename the merged-in branch made, as fossil's merge does:
// the destination's edit lands under the new name, and the commit records
// the rename.
func TestMerge_FollowsRenameOnSource(t *testing.T) {
	got, renames := renameMergeRepo(t,
		[]FileToCommit{{Name: "r.txt", Content: []byte("1\nx\nx\n2\n"), OldName: "f.txt"}},
		[]FileToCommit{{Name: "f.txt", Content: []byte("1\nx\nx\n2t\n")}})
	if want := map[string]string{"r.txt": "1\nx\nx\n2t\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("merged files = %q, want %q", got, want)
	}
	if want := map[string]string{"r.txt": "f.txt"}; !reflect.DeepEqual(renames, want) {
		t.Errorf("merge renames = %q, want %q", renames, want)
	}
}

// A file the destination renamed keeps its name and takes the source's
// edit.
func TestMerge_FollowsRenameOnDestination(t *testing.T) {
	got, renames := renameMergeRepo(t,
		[]FileToCommit{{Name: "f.txt", Content: []byte("1f\nx\nx\n2\n")}},
		[]FileToCommit{{Name: "r.txt", Content: []byte("1\nx\nx\n2t\n"), OldName: "f.txt"}})
	if want := map[string]string{"r.txt": "1f\nx\nx\n2t\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("merged files = %q, want %q", got, want)
	}
	if len(renames) != 0 {
		t.Errorf("merge renames = %q, want none", renames)
	}
}

// A file the source deleted and the destination left alone is gone after
// the merge.
func TestMerge_DropsFileDeletedOnSource(t *testing.T) {
	got, _ := renameMergeRepo(t,
		[]FileToCommit{{Name: "g.txt", Content: []byte("g\n")}},
		[]FileToCommit{{Name: "f.txt", Content: []byte("1\nx\nx\n2\n")}, {Name: "h.txt", Content: []byte("h\n")}})
	if want := map[string]string{"g.txt": "g\n", "h.txt": "h\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("merged files = %q, want %q", got, want)
	}
}
