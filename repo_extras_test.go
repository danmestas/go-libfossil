package libfossil

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestUVWriteReadList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	now := time.Now()
	if err := r.UVWrite("wiki/hello.md", []byte("# Hello\n"), now); err != nil {
		t.Fatal(err)
	}

	content, mtime, hash, err := r.UVRead("wiki/hello.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# Hello\n" {
		t.Errorf("UVRead content = %q, want %q", content, "# Hello\n")
	}
	if mtime == 0 {
		t.Error("UVRead mtime = 0, want nonzero")
	}
	if hash == "" {
		t.Error("UVRead hash is empty")
	}

	entries, err := r.UVList()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("UVList returned %d entries, want 1", len(entries))
	}
	if entries[0].Name != "wiki/hello.md" {
		t.Errorf("UVList[0].Name = %q, want %q", entries[0].Name, "wiki/hello.md")
	}
}

func TestTagCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid, _, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "f.txt", Content: []byte("x\n")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	tagRid, err := r.Tag(TagOpts{
		Name:     "sym-v1.0",
		TargetID: rid,
		User:     "test",
		Time:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tagRid <= 0 {
		t.Errorf("Tag returned rid=%d, want > 0", tagRid)
	}
}

// tagNamesOn returns the tag names recorded on rid, as "name,name".
func tagNamesOn(t *testing.T, r *Repo, rid int64) string {
	t.Helper()
	rows, err := r.DB().Query(
		"SELECT tag.tagname FROM tagxref JOIN tag USING(tagid) WHERE rid=? ORDER BY tag.tagname",
		rid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

// TestTagSymPrefix covers issue #293: fossil's tag add puts a check-in tag in
// the symbolic namespace (sym-), leaves a non-check-in tag bare, and refuses
// the reserved prefixes.
func TestTagSymPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := Create(path, CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	ciRid, _, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "f.txt", Content: []byte("x\n")}},
		Comment: "c1", User: "u",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A check-in target gets the sym- prefix.
	ctrlRid, err := r.Tag(TagOpts{Name: "release", TargetID: ciRid, User: "u", Time: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if got := tagNamesOn(t, r, ciRid); !strings.Contains(got, "sym-release") || strings.Contains(got, ",release") {
		t.Errorf("check-in tags = %q, want a sym-release and no bare release", got)
	}

	// A non-check-in target (the control artifact just stored) keeps the name bare.
	if _, err := r.Tag(TagOpts{Name: "meta", TargetID: ctrlRid, User: "u", Time: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got := tagNamesOn(t, r, ctrlRid); !strings.Contains(got, "meta") || strings.Contains(got, "sym-meta") {
		t.Errorf("control-artifact tags = %q, want a bare meta and no sym-meta", got)
	}

	// Reserved prefixes are refused whatever the target.
	for _, name := range []string{"wiki-x", "tkt-x", "event-x"} {
		if _, err := r.Tag(TagOpts{Name: name, TargetID: ciRid, User: "u", Time: time.Now().UTC()}); err == nil {
			t.Errorf("Tag(%q) succeeded, want a refusal", name)
		}
	}
}

func TestConfigGetSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// project-code is set by Create
	pc, err := r.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	if pc == "" {
		t.Error("project-code should not be empty")
	}

	if err := r.SetConfig("custom-key", "custom-value"); err != nil {
		t.Fatal(err)
	}
	val, err := r.Config("custom-key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "custom-value" {
		t.Errorf("Config(custom-key) = %q, want %q", val, "custom-value")
	}
}
