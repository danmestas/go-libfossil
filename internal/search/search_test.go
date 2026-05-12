package search_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/internal/search"
	"github.com/danmestas/libfossil/simio"

	_ "github.com/danmestas/libfossil/db/driver/ncruces"
)

func newTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	dir := t.TempDir()
	r, err := repo.Create(dir+"/test.fossil", "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestFTS5Available(t *testing.T) {
	r := newTestRepo(t)
	rows, err := r.DB().Query("PRAGMA compile_options")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var opt string
		if err := rows.Scan(&opt); err != nil {
			t.Fatal(err)
		}
		if opt == "ENABLE_FTS5" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("FTS5 not available in SQLite driver — search package requires it")
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	r := newTestRepo(t)
	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	// Verify FTS table exists by attempting a query
	_, err = idx.Search(search.Query{Term: "test"})
	if err != nil {
		t.Fatal("search after open failed:", err)
	}
}

func TestDrop(t *testing.T) {
	r := newTestRepo(t)
	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Drop(); err != nil {
		t.Fatal(err)
	}
	// After drop, Open should recreate tables
	idx2, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = idx2.Search(search.Query{Term: "test"})
	if err != nil {
		t.Fatal("search after drop+reopen failed:", err)
	}
}

func TestNeedsReindex_EmptyRepo(t *testing.T) {
	r := newTestRepo(t)
	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	needs, err := idx.NeedsReindex()
	if err != nil {
		t.Fatal(err)
	}
	// Empty repo has no trunk tip — nothing to index
	if needs {
		t.Fatal("expected NeedsReindex=false for empty repo")
	}
}

func TestNeedsReindex_AfterCheckin(t *testing.T) {
	r := newTestRepo(t)

	// manifest.Checkin creates repos programmatically — no fossil binary needed.
	// manifest.File fields: Name string, Content []byte, Perm string
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello world")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	needs, err := idx.NeedsReindex()
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Fatal("expected NeedsReindex=true after checkin")
	}
}

func TestRebuildIndex_IndexesFiles(t *testing.T) {
	r := newTestRepo(t)

	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "main.go", Content: []byte("package main\n\nfunc handleSync() {}\n")},
			{Name: "README.md", Content: []byte("# Hello World\n")},
		},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	// After rebuild, NeedsReindex should be false
	needs, err := idx.NeedsReindex()
	if err != nil {
		t.Fatal(err)
	}
	if needs {
		t.Fatal("expected NeedsReindex=false after rebuild")
	}
}

func TestRebuildIndex_SkipsBinaries(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Search implementation (Task 4)")
	}
	r := newTestRepo(t)

	binaryContent := make([]byte, 100)
	binaryContent[50] = 0x00 // null byte
	copy(binaryContent, []byte("not really text"))

	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "text.txt", Content: []byte("searchable content here")},
			{Name: "image.bin", Content: binaryContent},
		},
		Comment: "with binary",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	// Verify text file IS searchable
	results, err := idx.Search(search.Query{Term: "searchable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Path != "text.txt" {
		t.Fatalf("expected text.txt, got %s", results[0].Path)
	}

	// Verify binary file is NOT searchable
	binaryResults, err := idx.Search(search.Query{Term: "not really text"})
	if err != nil {
		t.Fatal(err)
	}
	if len(binaryResults) != 0 {
		t.Fatalf("expected 0 results for binary content, got %d", len(binaryResults))
	}
}

func TestRebuildIndex_HandlesDeltaChains(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Search implementation (Task 4)")
	}
	r := newTestRepo(t)

	// First checkin
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "data.txt", Content: []byte("original content")}},
		Comment: "first",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second checkin with Delta=true — blob stored as delta, not full content
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "data.txt", Content: []byte("modified content")}},
		Comment: "second",
		User:    "test",
		Delta:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	// Should find the expanded (modified) content, not the delta bytes
	results, err := idx.Search(search.Query{Term: "modified"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for delta-expanded content, got %d", len(results))
	}
}

func TestRebuildIndex_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Search implementation (Task 4)")
	}
	r := newTestRepo(t)

	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("hello world")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}

	// Rebuild twice — second should no-op
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(search.Query{Term: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result after double rebuild, got %d", len(results))
	}
}

func TestSearch_SubstringMatch(t *testing.T) {
	r := newTestRepo(t)

	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "sync/client.go", Content: []byte("package sync\n\nfunc handleSync() {\n\tlog.Println(\"syncing\")\n}\n")},
			{Name: "sync/server.go", Content: []byte("package sync\n\nfunc ServeHTTP() {\n\tlog.Println(\"serving\")\n}\n")},
		},
		Comment: "sync code",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(search.Query{Term: "handleSync"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'handleSync', got %d", len(results))
	}
	if results[0].Path != "sync/client.go" {
		t.Fatalf("expected sync/client.go, got %s", results[0].Path)
	}
	if results[0].Line != 3 {
		t.Fatalf("expected line 3, got %d", results[0].Line)
	}
	if results[0].MatchLen != 10 {
		t.Fatalf("expected MatchLen=10, got %d", results[0].MatchLen)
	}
}

func TestSearch_CaseInsensitive(t *testing.T) {
	r := newTestRepo(t)

	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.go", Content: []byte("func HandleSync() {}\n")}},
		Comment: "case test",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	// Search with different case — should still match
	results, err := idx.Search(search.Query{Term: "handlesync"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 case-insensitive result, got %d", len(results))
	}
}

func TestSearch_MinTermLength(t *testing.T) {
	r := newTestRepo(t)
	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	// Sub-3-char query returns empty
	results, err := idx.Search(search.Query{Term: "ab"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for 2-char query, got %d", len(results))
	}
}

func TestSearch_WithContext(t *testing.T) {
	r := newTestRepo(t)

	fileContent := "line1\nline2\nline3 target\nline4\nline5\n"
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "f.txt", Content: []byte(fileContent)}},
		Comment: "context test",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(search.Query{Term: "target", ContextLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Line != 3 {
		t.Fatalf("expected line 3, got %d", results[0].Line)
	}
	ctx := results[0].Context
	if !strings.Contains(ctx, "line2") || !strings.Contains(ctx, "line3 target") || !strings.Contains(ctx, "line4") {
		t.Fatalf("context missing expected lines: %q", ctx)
	}
}

func TestSearch_EscapesFTS5SpecialChars(t *testing.T) {
	r := newTestRepo(t)

	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "q.txt", Content: []byte(`she said "hello" to the world`)}},
		Comment: "quotes",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	// Search for a term containing a double quote — should not crash
	results, err := idx.Search(search.Query{Term: `"hello"`})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for quoted search, got %d", len(results))
	}
}

func TestSearch_MaxResults(t *testing.T) {
	r := newTestRepo(t)

	var files []manifest.File
	for i := 0; i < 10; i++ {
		files = append(files, manifest.File{
			Name:    fmt.Sprintf("file%d.txt", i),
			Content: []byte("common search term here"),
		})
	}
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   files,
		Comment: "many files",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(search.Query{Term: "common search", MaxResults: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) > 3 {
		t.Fatalf("expected at most 3 results, got %d", len(results))
	}
}

func TestStaleAfterNewCheckin(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Search implementation (Task 4)")
	}
	r := newTestRepo(t)

	// First checkin
	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("alpha content")}},
		Comment: "first",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	// Verify "alpha" is searchable
	results, err := idx.Search(search.Query{Term: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for alpha, got %d", len(results))
	}

	// Second checkin adds new file with sym-trunk tag to advance the trunk pointer
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("alpha content")},
			{Name: "b.txt", Content: []byte("bravo content")},
		},
		Comment: "second",
		User:    "test",
		Parent:  rid1,
		Tags: []deck.TagCard{
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should be stale now
	needs, err := idx.NeedsReindex()
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Fatal("expected NeedsReindex=true after second checkin")
	}

	// Reindex
	if err := idx.RebuildIndex(); err != nil {
		t.Fatal(err)
	}

	// Now "bravo" should be searchable
	results, err = idx.Search(search.Query{Term: "bravo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for bravo, got %d", len(results))
	}

	// And NeedsReindex is false again
	needs, err = idx.NeedsReindex()
	if err != nil {
		t.Fatal(err)
	}
	if needs {
		t.Fatal("expected NeedsReindex=false after reindex")
	}
}

// TestRebuildIndex_BuggifyResilience verifies that RebuildIndex behaves
// correctly when content.Expand's BUGGIFY site is active (1% chance of
// flipping byte 0 in expanded content). Two outcomes are acceptable:
//
//  1. Manifest corrupted → RebuildIndex returns error (can't read file list).
//  2. File blob corrupted → RebuildIndex succeeds with wrong content indexed.
//     The search index is a derived cache — corrupted content doesn't compromise
//     the repo, just means one file's results are wrong until next reindex.
//
// The key property: RebuildIndex never panics, and if it succeeds, the index
// is structurally valid and searchable.
func TestRebuildIndex_BuggifyResilience(t *testing.T) {
	r := newTestRepo(t)

	// Create files BEFORE enabling BUGGIFY so checkin itself isn't corrupted.
	var files []manifest.File
	for i := 0; i < 50; i++ {
		files = append(files, manifest.File{
			Name:    fmt.Sprintf("src/file%d.go", i),
			Content: []byte(fmt.Sprintf("package src\n\nfunc Function%d() { /* unique content %d */ }\n", i, i)),
		})
	}

	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   files,
		Comment: "buggify resilience test",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := search.Open(r)
	if err != nil {
		t.Fatal(err)
	}

	// Enable BUGGIFY AFTER checkin so only RebuildIndex is affected.
	// Seed 42 gives deterministic fault injection.
	simio.EnableBuggify(42)
	defer simio.DisableBuggify()

	rebuildErr := idx.RebuildIndex()
	if rebuildErr != nil {
		// Outcome 1: BUGGIFY corrupted the manifest blob itself.
		// RebuildIndex correctly returns an error. This is acceptable —
		// a corrupted manifest means we can't enumerate files.
		t.Logf("BUGGIFY corrupted manifest (expected): %v", rebuildErr)
		return
	}

	// Outcome 2: manifest was fine, but some file blobs may be corrupted.
	// Verify the index is structurally valid.

	needs, err := idx.NeedsReindex()
	if err != nil {
		t.Fatal(err)
	}
	if needs {
		t.Fatal("expected NeedsReindex=false after successful rebuild under BUGGIFY")
	}

	// Index must be searchable — queries must not error.
	results, err := idx.Search(search.Query{Term: "Function"})
	if err != nil {
		t.Fatal("Search failed after BUGGIFY rebuild:", err)
	}

	// We expect results for most files. Some may have corrupted content
	// (byte 0 flipped by BUGGIFY), but the FTS index should still work.
	if len(results) == 0 {
		t.Fatal("expected at least some results after BUGGIFY rebuild, got 0")
	}
	t.Logf("BUGGIFY resilience: %d/%d files matched 'Function'", len(results), len(files))
}
