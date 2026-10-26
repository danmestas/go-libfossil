package libfossil

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	fsltype "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestCommitAndAncestry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid1, uuid, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "hello.txt", Content: []byte("hello world\n")},
		},
		Comment: "initial commit",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if uuid == "" {
		t.Fatal("Commit returned empty UUID")
	}
	if len(uuid) != 40 && len(uuid) != 64 {
		t.Errorf("UUID length = %d, want 40 or 64", len(uuid))
	}
	if rid1 <= 0 {
		t.Fatalf("Commit returned rid=%d, want > 0", rid1)
	}

	// Second commit to test the ancestry walk.
	rid2, uuid2, err := r.Commit(CommitOpts{
		ParentID: rid1,
		Files: []FileToCommit{
			{Name: "hello.txt", Content: []byte("hello world v2\n")},
		},
		Comment: "second commit",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := r.Ancestry(LogOpts{Start: rid2, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("Ancestry returned %d entries, want >= 2", len(entries))
	}
	if entries[0].UUID != uuid2 {
		t.Errorf("first entry UUID = %q, want %q", entries[0].UUID, uuid2)
	}
	if entries[0].Comment != "second commit" {
		t.Errorf("first entry Comment = %q, want %q", entries[0].Comment, "second commit")
	}
	if entries[1].UUID != uuid {
		t.Errorf("second entry UUID = %q, want %q", entries[1].UUID, uuid)
	}
}

// TestCommitPreservesParentFiles is the regression for #30: a child
// commit that supplies only a subset of the parent's tracked files must
// still produce a full-tree manifest. Before the fix, the child's
// manifest contained only the supplied subset and a checkout at the
// child's rev silently lost every file the caller did not re-supply.
func TestCommitPreservesParentFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	parentRID, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "a.txt", Content: []byte("alpha\n")},
			{Name: "b.txt", Content: []byte("bravo v1\n")},
			{Name: "c.txt", Content: []byte("charlie\n")},
		},
		Comment: "parent: a, b, c",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("parent commit: %v", err)
	}

	childRID, _, err := r.Commit(CommitOpts{
		ParentID: parentRID,
		Files: []FileToCommit{
			{Name: "b.txt", Content: []byte("bravo v2\n")},
		},
		Comment: "child: only b modified",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("child commit: %v", err)
	}

	files, err := r.ListFiles(childRID)
	if err != nil {
		t.Fatalf("ListFiles(child): %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Name] = f.UUID
	}
	if len(got) != 3 {
		t.Fatalf("child manifest has %d files, want 3 (a.txt, b.txt, c.txt); got %v", len(got), got)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, ok := got[name]; !ok {
			t.Errorf("child manifest missing %q (file tracked at parent was dropped)", name)
		}
	}

	// a.txt and c.txt should keep the parent's blob UUIDs (untouched).
	parentFiles, err := r.ListFiles(parentRID)
	if err != nil {
		t.Fatalf("ListFiles(parent): %v", err)
	}
	parentUUIDs := map[string]string{}
	for _, f := range parentFiles {
		parentUUIDs[f.Name] = f.UUID
	}
	if got["a.txt"] != parentUUIDs["a.txt"] {
		t.Errorf("a.txt UUID changed across commits: parent=%s child=%s", parentUUIDs["a.txt"], got["a.txt"])
	}
	if got["c.txt"] != parentUUIDs["c.txt"] {
		t.Errorf("c.txt UUID changed across commits: parent=%s child=%s", parentUUIDs["c.txt"], got["c.txt"])
	}
	// b.txt should have a NEW UUID — caller supplied new content.
	if got["b.txt"] == parentUUIDs["b.txt"] {
		t.Errorf("b.txt UUID unchanged despite new content: %s", got["b.txt"])
	}
}

func TestListFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "a.txt", Content: []byte("aaa\n")},
			{Name: "b.txt", Content: []byte("bbb\n")},
		},
		Comment: "two files",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	files, err := r.ListFiles(rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("ListFiles returned %d files, want 2", len(files))
	}
	if files[0].Name != "a.txt" || files[1].Name != "b.txt" {
		t.Errorf("files = %v, want a.txt and b.txt", files)
	}
}

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

// These tests drive Repo.ListFilesIn, the public listing narrowed to a cone
// (draft-fossil-sparse-cone-00 §4.1, §4.5): it is ListFiles filtered by
// Cone.Covers, and on a tree manifest it never reads a node the cone cannot
// reach.

// newListConeRepo commits sixteen files under a, b, c and d, as a tree when
// tree is set, and returns the repository and the check-in's rid.
func newListConeRepo(t *testing.T, tree bool) (*Repo, int64) {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if tree {
		if _, _, err := r.EnableSubmanifests(1); err != nil {
			t.Fatal(err)
		}
	}
	var files []FileToCommit
	for _, dir := range []string{"a", "b", "c", "d"} {
		for i := 0; i < 4; i++ {
			name := fmt.Sprintf("%s/%d.c", dir, i)
			files = append(files, FileToCommit{Name: name, Content: []byte(name + "\n")})
		}
	}
	rid, _, err := r.Commit(CommitOpts{Files: files, Comment: "c", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := manifest.GetManifest(r.Inner(), fsltype.FslID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(d.S) > 0; got != tree {
		t.Fatalf("check-in is a tree: %v, want %v", got, tree)
	}
	return r, rid
}

func mustNewCone(t *testing.T, include, exclude []string) *Cone {
	t.Helper()
	c, err := NewCone(include, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func entryNames(files []FileEntry) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return strings.Join(names, " ")
}

// On flat and tree check-ins alike the narrowed listing is the full listing
// filtered by Covers, entry for entry, and a nil cone lists everything.
func TestListFilesInMatchesFilteredListFiles(t *testing.T) {
	for _, tree := range []bool{false, true} {
		r, rid := newListConeRepo(t, tree)
		all, err := r.ListFiles(rid)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.ListFilesIn(rid, nil)
		if err != nil || !reflect.DeepEqual(got, all) {
			t.Fatalf("tree %v: nil cone = %v, %v; want ListFiles", tree, got, err)
		}
		for _, c := range []*Cone{
			mustNewCone(t, []string{"b"}, nil),
			mustNewCone(t, []string{"a", "c/2.c"}, []string{"a/1.c"}),
			mustNewCone(t, []string{"zzz"}, nil),
		} {
			var want []FileEntry
			for _, f := range all {
				if c.Covers(f.Name) {
					want = append(want, f)
				}
			}
			got, err := r.ListFilesIn(rid, c)
			if err != nil {
				t.Fatalf("tree %v, cone %v: %v", tree, c, err)
			}
			if len(got) != len(want) || len(want) > 0 && !reflect.DeepEqual(got, want) {
				t.Errorf("tree %v, cone %v: %q, want %q", tree, c, entryNames(got), entryNames(want))
			}
		}
	}
}

// elideOutside makes every top-level node of the tree check-in rid whose key
// range c cannot reach an elided phantom, as a sparse store holds it, and
// returns how many it elided.
func elideOutside(t *testing.T, r *Repo, rid int64, c *Cone) int {
	t.Helper()
	d, err := manifest.GetManifest(r.Inner(), fsltype.FslID(rid))
	if err != nil {
		t.Fatal(err)
	}
	elided := 0
	for i, s := range d.S {
		limit := ""
		if i+1 < len(d.S) {
			limit = d.S[i+1].Name
		}
		if c.inner.MayCover(s.Name, limit) {
			continue
		}
		for _, stmt := range []string{
			"UPDATE blob SET size=-1,content=NULL WHERE uuid=?",
			"INSERT OR IGNORE INTO phantom SELECT rid FROM blob WHERE uuid=?",
		} {
			if _, err := r.Inner().DB().Exec(stmt, s.UUID); err != nil {
				t.Fatal(err)
			}
		}
		elided++
	}
	return elided
}

// Over a sparse store the narrowed listing reads only the nodes the cone
// reaches, so it succeeds where ListFiles fails on an elided node, and a cone
// that does reach one fails naming it.
func TestListFilesInSkipsElidedNodes(t *testing.T) {
	r, rid := newListConeRepo(t, true)
	c := mustNewCone(t, []string{"a"}, nil)
	if n := elideOutside(t, r, rid, c); n == 0 {
		t.Fatal("no node lies outside the cone")
	}
	if _, err := r.ListFiles(rid); err == nil {
		t.Fatal("ListFiles walked a tree with elided nodes")
	}
	got, err := r.ListFilesIn(rid, c)
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(got); names != "a/0.c a/1.c a/2.c a/3.c" {
		t.Fatalf("ListFilesIn = %q", names)
	}
	if _, err := r.ListFilesIn(rid, mustNewCone(t, []string{"d"}, nil)); err == nil {
		t.Fatal("a cone over an elided node listed without it")
	} else if _, ok := manifest.MissingSubmanifest(err); !ok {
		t.Fatalf("error %v does not name the missing node", err)
	}
}

// The narrowed listing reads through the attached content cache: a narrow
// cone expands fewer artifacts than the full listing, and repeating it
// expands none.
func TestListFilesInUsesContentCache(t *testing.T) {
	r, rid := newListConeRepo(t, true)
	r.SetContentCache(1 << 20)
	c := mustNewCone(t, []string{"a"}, nil)
	if _, err := r.ListFilesIn(rid, c); err != nil {
		t.Fatal(err)
	}
	narrow := cacheStats(t, r).Misses
	if _, err := r.ListFilesIn(rid, c); err != nil {
		t.Fatal(err)
	}
	if again := cacheStats(t, r).Misses - narrow; narrow == 0 || again != 0 {
		t.Fatalf("misses: first %d, repeat %d; want some, then none", narrow, again)
	}
	if _, err := r.ListFiles(rid); err != nil {
		t.Fatal(err)
	}
	if full := cacheStats(t, r).Misses; full <= narrow {
		t.Fatalf("misses after the full listing %d, after the narrow one %d", full, narrow)
	}
}

// TestTimelineEnumeratesEveryEvent is the acceptance criterion for the bug
// this issue fixes: Timeline with no type filter must return every row of
// event, including check-ins that are not first-parent ancestors of any
// single start rid — the thing the old Timeline (now Ancestry) could
// never see.
func TestTimelineEnumeratesEveryEvent(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid1, _, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "trunk root", User: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(CommitOpts{
		ParentID: rid1,
		Files:    []FileToCommit{{Name: "a.txt", Content: []byte("v2")}},
		Comment:  "trunk tip", User: "test",
	}); err != nil {
		t.Fatal(err)
	}
	branchRID, branchUUID, err := r.Commit(CommitOpts{
		ParentID: rid1,
		Files:    []FileToCommit{{Name: "a.txt", Content: []byte("v3")}},
		Comment:  "sibling branch head", User: "test",
		Tags: []TagSpec{{Name: "branch", Value: "feature-x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if branchRID <= 0 {
		t.Fatal("branch commit returned rid <= 0")
	}

	var wantCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM event").Scan(&wantCount); err != nil {
		t.Fatal(err)
	}

	entries, err := r.Timeline(TimelineOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != wantCount {
		t.Fatalf("Timeline returned %d entries, want %d (SELECT count(*) FROM event)", len(entries), wantCount)
	}

	found := false
	for _, e := range entries {
		if e.UUID == branchUUID {
			found = true
		}
	}
	if !found {
		t.Fatalf("sibling branch head %s missing from Timeline output", branchUUID)
	}
}

// TestTimelineTypeFilter covers Type=EventKindCheckin returning exactly
// the check-in rows.
func TestTimelineTypeFilter(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, _, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "checkin", User: "test",
	}); err != nil {
		t.Fatal(err)
	}

	var wantCi int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&wantCi); err != nil {
		t.Fatal(err)
	}

	entries, err := r.Timeline(TimelineOpts{Type: EventKindCheckin})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != wantCi {
		t.Fatalf("Timeline(Type=ci) returned %d entries, want %d", len(entries), wantCi)
	}
	for _, e := range entries {
		if e.Kind != EventKindCheckin {
			t.Fatalf("entry kind = %q, want %q", e.Kind, EventKindCheckin)
		}
	}
}

// TestTimelineOrderingAndLimit covers (mtime DESC, rid DESC) ordering and
// that Limit both bounds the result and is reachable — the defining
// symptom of the original bug was a limit that never bound.
func TestTimelineOrderingAndLimit(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var parent int64
	var rids []int64
	for i := 0; i < 5; i++ {
		rid, _, err := r.Commit(CommitOpts{
			ParentID: parent,
			Files:    []FileToCommit{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
			Comment:  fmt.Sprintf("commit %d", i), User: "test",
			Time: time.Date(2024, 1, 15, 10, i, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		parent = rid
		rids = append(rids, rid)
	}

	entries, err := r.Timeline(TimelineOpts{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	// Newest first: the last two commits, newest rid first.
	if entries[0].RID != rids[4] || entries[1].RID != rids[3] {
		t.Fatalf("order = [%d, %d], want [%d, %d]", entries[0].RID, entries[1].RID, rids[4], rids[3])
	}
}

// TestTimelinePaginationExactlyOnce covers the acceptance criterion that
// paginating the full set yields each event exactly once, including when
// several events share an identical mtime.
func TestTimelinePaginationExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	sameTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	var parent int64
	var allUUIDs []string
	for i := 0; i < 6; i++ {
		rid, uuid, err := r.Commit(CommitOpts{
			ParentID: parent,
			Files:    []FileToCommit{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
			Comment:  fmt.Sprintf("commit %d", i), User: "test",
			Time: sameTime, // every commit shares the exact same mtime
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		parent = rid
		allUUIDs = append(allUUIDs, uuid)
	}

	const pageSize = 2
	// Exactly ceil(n/pageSize) pages: a fixed bound, not a "while non-empty"
	// loop, so a cursor that fails to advance fails this test immediately
	// instead of hanging it.
	maxPages := (len(allUUIDs) + pageSize - 1) / pageSize
	seen := make(map[string]int)
	var total int
	var after Cursor
	for page := 0; page < maxPages; page++ {
		entries, err := r.Timeline(TimelineOpts{Limit: pageSize, After: after})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(entries) == 0 {
			t.Fatalf("page %d returned 0 entries after only %d of %d events seen — cursor did not advance", page, total, len(allUUIDs))
		}
		for _, e := range entries {
			seen[e.UUID]++
			total++
		}
		after = entries[len(entries)-1].Cursor
	}

	if total != len(allUUIDs) {
		t.Fatalf("paginated total = %d across exactly %d pages, want %d", total, maxPages, len(allUUIDs))
	}
	for _, uuid := range allUUIDs {
		if seen[uuid] != 1 {
			t.Fatalf("event %s seen %d times across %d pages, want exactly 1", uuid, seen[uuid], maxPages)
		}
	}

	// The defining symptom of a broken cursor is non-termination: every
	// row enumerated correctly, but the cursor never signals exhaustion.
	// One more call past the last real page must come back empty.
	final, err := r.Timeline(TimelineOpts{Limit: pageSize, After: after})
	if err != nil {
		t.Fatalf("final page: %v", err)
	}
	if len(final) != 0 {
		t.Fatalf("pagination did not terminate: page after the last expected page returned %d entries, want 0: %v", len(final), final)
	}
}

// TestTimelinePaginationSubMillisecondBoundary is the public-API-level
// regression test for a cursor predicate that only distinguishes rows via
// a tolerance window instead of treating rid as a true tie-break at exact
// mtime equality: such a predicate can skip a row, duplicate another, or
// fail to advance the cursor entirely when two rows are close together
// but not equal. rid (checkin/insertion order) is deliberately decoupled
// from mtime order here, since a predicate bug that only shows up when
// they disagree would otherwise pass unnoticed.
func TestTimelinePaginationSubMillisecondBoundary(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const n = 6
	const pageSize = 2
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	var parent int64
	var allUUIDs []string
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(n-1-i) * 3 * time.Millisecond) // 3ms apart, descending as rid ascends
		rid, uuid, err := r.Commit(CommitOpts{
			ParentID: parent,
			Files:    []FileToCommit{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
			Comment:  fmt.Sprintf("commit %d", i), User: "test",
			Time: ts,
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		parent = rid
		allUUIDs = append(allUUIDs, uuid)
	}

	maxPages := (n + pageSize - 1) / pageSize // exactly ceil(n/pageSize) = 3
	seen := make(map[string]int)
	var total int
	var after Cursor
	for page := 0; page < maxPages; page++ {
		entries, err := r.Timeline(TimelineOpts{Limit: pageSize, After: after})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(entries) == 0 {
			t.Fatalf("page %d returned 0 entries after only %d of %d events seen — cursor did not advance", page, total, n)
		}
		for _, e := range entries {
			seen[e.UUID]++
			total++
		}
		after = entries[len(entries)-1].Cursor
	}

	if total != n {
		t.Fatalf("paginated total = %d across exactly %d pages, want %d", total, maxPages, n)
	}
	for _, uuid := range allUUIDs {
		if seen[uuid] != 1 {
			t.Fatalf("event %s seen %d times across %d pages, want exactly 1", uuid, seen[uuid], maxPages)
		}
	}

	// Non-termination is the actual user-facing failure mode: every row
	// enumerated correctly but the cursor never signals exhaustion. Assert
	// the page past the last expected one comes back empty.
	final, err := r.Timeline(TimelineOpts{Limit: pageSize, After: after})
	if err != nil {
		t.Fatalf("final page: %v", err)
	}
	if len(final) != 0 {
		t.Fatalf("pagination did not terminate: page after the last expected page returned %d entries, want 0: %v", len(final), final)
	}
}
