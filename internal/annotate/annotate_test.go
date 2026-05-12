package annotate

import (
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func setupTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// setupThreeCommits creates a repo with 3 commits:
//
//	commit 1 (alice): "line A\nline B\n"
//	commit 2 (bob):   "line A\nline C\n"  (B changed to C)
//	commit 3 (charlie): "line A\nline C\nline D\n" (added D)
//
// Returns the repo and the three checkin RIDs.
func setupThreeCommits(t *testing.T) (*repo.Repo, libfossil.FslID, libfossil.FslID, libfossil.FslID) {
	t.Helper()
	r := setupTestRepo(t)

	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte("line A\nline B\n")}},
		Comment: "commit 1",
		User:    "alice",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("checkin 1: %v", err)
	}

	rid2, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte("line A\nline C\n")}},
		Comment: "commit 2",
		User:    "bob",
		Parent:  rid1,
		Time:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("checkin 2: %v", err)
	}

	rid3, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte("line A\nline C\nline D\n")}},
		Comment: "commit 3",
		User:    "charlie",
		Parent:  rid2,
		Time:    time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("checkin 3: %v", err)
	}

	return r, rid1, rid2, rid3
}

func TestAnnotateBasic(t *testing.T) {
	r, _, _, rid3 := setupThreeCommits(t)

	lines, err := Annotate(r, Options{
		FilePath: "file.txt",
		StartRID: rid3,
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	// line A should be attributed to alice (commit 1)
	if lines[0].Text != "line A" {
		t.Errorf("line 0 text = %q, want %q", lines[0].Text, "line A")
	}
	if lines[0].Version.User != "alice" {
		t.Errorf("line 0 user = %q, want %q", lines[0].Version.User, "alice")
	}

	// line C should be attributed to bob (commit 2)
	if lines[1].Text != "line C" {
		t.Errorf("line 1 text = %q, want %q", lines[1].Text, "line C")
	}
	if lines[1].Version.User != "bob" {
		t.Errorf("line 1 user = %q, want %q", lines[1].Version.User, "bob")
	}

	// line D should be attributed to charlie (commit 3)
	if lines[2].Text != "line D" {
		t.Errorf("line 2 text = %q, want %q", lines[2].Text, "line D")
	}
	if lines[2].Version.User != "charlie" {
		t.Errorf("line 2 user = %q, want %q", lines[2].Version.User, "charlie")
	}
}

func TestAnnotateWithLimit(t *testing.T) {
	r, _, _, rid3 := setupThreeCommits(t)

	// Limit=1 means we can only walk 1 ancestor (commit 2), not commit 1.
	// But we start at commit 3 — limit restricts parent walks.
	// With limit=0 steps allowed, all lines stay at charlie.
	lines, err := Annotate(r, Options{
		FilePath: "file.txt",
		StartRID: rid3,
		Limit:    0, // unlimited — test limit=1 below
	})
	if err != nil {
		t.Fatalf("Annotate unlimited: %v", err)
	}
	if lines[0].Version.User != "alice" {
		t.Errorf("unlimited: line 0 user = %q, want alice", lines[0].Version.User)
	}

	// Now with limit=1: can walk only to commit 2 (one step from commit 3).
	// All lines start attributed to charlie, then one walk to bob.
	// line A appears in bob's version -> attributed to bob
	// line C appears in bob's version -> attributed to bob
	// line D is new in charlie -> stays charlie
	// But we can't walk to alice because limit is exhausted.
	lines, err = Annotate(r, Options{
		FilePath: "file.txt",
		StartRID: rid3,
		Limit:    1,
	})
	if err != nil {
		t.Fatalf("Annotate limit=1: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	// line A -> bob (walked one step)
	if lines[0].Version.User != "bob" {
		t.Errorf("limit=1: line 0 user = %q, want bob", lines[0].Version.User)
	}
	// line C -> bob
	if lines[1].Version.User != "bob" {
		t.Errorf("limit=1: line 1 user = %q, want bob", lines[1].Version.User)
	}
	// line D -> charlie (new in commit 3)
	if lines[2].Version.User != "charlie" {
		t.Errorf("limit=1: line 2 user = %q, want charlie", lines[2].Version.User)
	}
}

func TestAnnotateWithOrigin(t *testing.T) {
	r, _, rid2, rid3 := setupThreeCommits(t)

	// Origin=commit 2: stop before walking past commit 2.
	// Walk from commit 3 to commit 2 (one step), but don't go to commit 1.
	// line A -> bob (from commit 2, can't go further)
	// line C -> bob
	// line D -> charlie
	lines, err := Annotate(r, Options{
		FilePath:  "file.txt",
		StartRID:  rid3,
		OriginRID: rid2,
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0].Version.User != "bob" {
		t.Errorf("origin: line 0 user = %q, want bob", lines[0].Version.User)
	}
	if lines[1].Version.User != "bob" {
		t.Errorf("origin: line 1 user = %q, want bob", lines[1].Version.User)
	}
	if lines[2].Version.User != "charlie" {
		t.Errorf("origin: line 2 user = %q, want charlie", lines[2].Version.User)
	}
}

func TestAnnotateSingleCommit(t *testing.T) {
	r, rid1, _, _ := setupThreeCommits(t)

	lines, err := Annotate(r, Options{
		FilePath: "file.txt",
		StartRID: rid1,
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	// Both lines attributed to alice (single commit, no parent).
	if lines[0].Version.User != "alice" {
		t.Errorf("line 0 user = %q, want alice", lines[0].Version.User)
	}
	if lines[0].Text != "line A" {
		t.Errorf("line 0 text = %q, want %q", lines[0].Text, "line A")
	}
	if lines[1].Version.User != "alice" {
		t.Errorf("line 1 user = %q, want alice", lines[1].Version.User)
	}
	if lines[1].Text != "line B" {
		t.Errorf("line 1 text = %q, want %q", lines[1].Text, "line B")
	}
}
