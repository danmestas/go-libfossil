package libfossil

import (
	"strings"
	"testing"
)

// A FileToCommit that names its content by UUID is carried into the check-in
// without its bytes: the F card repeats the hash, nothing new is stored, and
// the file reads back as it did at the parent.
func TestCommit_CarriedByUUID(t *testing.T) {
	r := newTestRepo(t)
	v1 := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "keep.txt", Content: []byte("kept as is\n")},
		{Name: "drop.txt", Content: []byte("to be deleted\n")},
		{Name: "edit.txt", Content: []byte("before\n")},
	})
	parent, err := r.ListFiles(v1)
	if err != nil {
		t.Fatalf("ListFiles(v1): %v", err)
	}
	var keep FileEntry
	for _, f := range parent {
		if f.Name == "keep.txt" {
			keep = f
		}
	}
	if keep.UUID == "" {
		t.Fatal("keep.txt has no UUID at v1")
	}

	var blobsBefore int64
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&blobsBefore); err != nil {
		t.Fatalf("count blobs: %v", err)
	}

	// Carry keep.txt by hash, change edit.txt, and leave drop.txt out: with
	// PartialManifest the omission is the deletion.
	v2, _, err := r.Commit(CommitOpts{
		ParentID:        v1,
		PartialManifest: true,
		Comment:         "v2",
		User:            "test",
		Files: []FileToCommit{
			{Name: "keep.txt", UUID: keep.UUID, Perm: keep.Perm},
			{Name: "edit.txt", Content: []byte("after\n")},
		},
	})
	if err != nil {
		t.Fatalf("Commit v2: %v", err)
	}

	got, err := r.ReadFile(v2, "keep.txt")
	if err != nil {
		t.Fatalf("ReadFile keep.txt: %v", err)
	}
	if string(got) != "kept as is\n" {
		t.Errorf("keep.txt at v2 = %q, want the parent's bytes", got)
	}
	if got, err := r.ReadFile(v2, "edit.txt"); err != nil || string(got) != "after\n" {
		t.Errorf("edit.txt at v2 = %q, %v", got, err)
	}
	if _, err := r.ReadFile(v2, "drop.txt"); err == nil {
		t.Error("drop.txt still present at v2; PartialManifest should have dropped it")
	}

	files, err := r.ListFiles(v2)
	if err != nil {
		t.Fatalf("ListFiles(v2): %v", err)
	}
	for _, f := range files {
		if f.Name == "keep.txt" && f.UUID != keep.UUID {
			t.Errorf("keep.txt UUID at v2 = %s, want %s carried verbatim", f.UUID, keep.UUID)
		}
	}

	// Two new artifacts only: the v2 manifest and edit.txt's new content.
	var blobsAfter int64
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&blobsAfter); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if blobsAfter-blobsBefore != 2 {
		t.Errorf("blob rows grew by %d, want 2 (manifest + edited file); a carried file must store nothing", blobsAfter-blobsBefore)
	}
}

func TestCommit_CarriedByUUID_Rejections(t *testing.T) {
	r := newTestRepo(t)
	v1 := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "a.txt", Content: []byte("a\n")},
	})
	parent, err := r.ListFiles(v1)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	// Both UUID and Content is a caller error, refused before anything is written.
	_, _, err = r.Commit(CommitOpts{
		ParentID: v1, Comment: "bad", User: "test",
		Files: []FileToCommit{{Name: "a.txt", UUID: parent[0].UUID, Content: []byte("a\n")}},
	})
	if err == nil || !strings.Contains(err.Error(), "both UUID and Content") {
		t.Errorf("UUID+Content: got %v, want a refusal naming both", err)
	}

	// A UUID the repository does not hold cannot be carried: the check-in
	// would name content its writer never saw.
	unknown := strings.Repeat("0", 64)
	_, _, err = r.Commit(CommitOpts{
		ParentID: v1, Comment: "bad", User: "test",
		Files: []FileToCommit{{Name: "b.txt", UUID: unknown}},
	})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("unknown UUID: got %v, want 'not available'", err)
	}
}

// A sparse store skips the held check for a file outside its cone, so the
// UUID's shape is all that stands between caller text and the F card. Each
// malformed UUID is refused there, including one whose newline would write
// a card of its own into the check-in.
func TestCommit_CarriedByUUID_MalformedOnSparseStore(t *testing.T) {
	r := newTestRepo(t)
	v1 := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "src/a.txt", Content: []byte("a\n")},
	})
	c, err := NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatalf("NewCone: %v", err)
	}
	if err := r.SetStoreCone(c); err != nil {
		t.Fatalf("SetStoreCone: %v", err)
	}

	for _, uuid := range []string{
		strings.Repeat("z", 64),
		strings.Repeat("A", 40),
		strings.Repeat("a", 41),
		strings.Repeat("a", 40) + "\nT +sym-injected * x",
	} {
		_, _, err := r.Commit(CommitOpts{
			ParentID: v1, Comment: "bad", User: "test",
			Files: []FileToCommit{{Name: "docs/b.txt", UUID: uuid}},
		})
		if err == nil || !strings.Contains(err.Error(), "not an artifact hash") {
			t.Errorf("UUID %q: got %v, want 'not an artifact hash'", uuid, err)
		}
	}

	// A well-formed hash outside the cone is still carried without being held.
	elided := strings.Repeat("b", 64)
	if _, _, err := r.Commit(CommitOpts{
		ParentID: v1, Comment: "ok", User: "test",
		Files: []FileToCommit{{Name: "docs/b.txt", UUID: elided}},
	}); err != nil {
		t.Errorf("elided well-formed UUID: %v", err)
	}
}
