package checkout

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Naming a directory adds the files beneath it, as fossil's add does, and
// never the directory itself: "." used to be stored as a file, and the next
// commit failed reading it. Dotfiles found by the walk are skipped, as
// fossil skips them without --dotfiles; one named explicitly is added.
func TestManageExpandsDirectories(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"lib/a.go": "a", "lib/deep/b.go": "b", "lib/.hidden": "h",
		".config/c": "c", "top.txt": "t", ".keep": "k",
	} {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{"lib"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{".", ".keep"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := co.db.Query("SELECT pathname FROM vfile WHERE rid=0 ORDER BY pathname")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var added []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		added = append(added, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{".keep", "lib/a.go", "lib/deep/b.go", "top.txt"}
	if !slices.Equal(added, want) {
		t.Fatalf("added %q, want %q", added, want)
	}
	if _, _, err := co.Commit(CommitOpts{Message: "add tree", User: "test"}); err != nil {
		t.Fatalf("commit after adding a directory: %v", err)
	}
}
