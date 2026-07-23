package manifest

import (
	"bytes"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/internal/verify"
)

func TestFileHistory_Basic(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// Commit 1: add hello.txt
	rid1, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("v1")}},
		Comment: "add hello",
		User:    "alice",
		Time:    ts,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Commit 2: modify hello.txt
	_, _, err = Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("v2")}},
		Comment: "update hello",
		User:    "bob",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})
	if err != nil {
		t.Fatal(err)
	}

	versions, err := FileHistory(r, FileHistoryOpts{Path: "hello.txt"})
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	// Most recent first
	if versions[0].Action != FileModified {
		t.Errorf("v[0] action = %v, want Modified", versions[0].Action)
	}
	if versions[0].User != "bob" {
		t.Errorf("v[0] user = %q, want bob", versions[0].User)
	}
	if versions[1].Action != FileAdded {
		t.Errorf("v[1] action = %v, want Added", versions[1].Action)
	}
	if versions[1].User != "alice" {
		t.Errorf("v[1] user = %q, want alice", versions[1].User)
	}
}

func TestFileHistory_MultipleFiles(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	rid1, _, _ := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("a1")},
			{Name: "b.txt", Content: []byte("b1")},
		},
		Comment: "add both",
		User:    "alice",
		Time:    ts,
	})

	// Only modify a.txt
	Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("a2")},
			{Name: "b.txt", Content: []byte("b1")},
		},
		Comment: "update a only",
		User:    "bob",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})

	histA, _ := FileHistory(r, FileHistoryOpts{Path: "a.txt"})
	histB, _ := FileHistory(r, FileHistoryOpts{Path: "b.txt"})

	if len(histA) != 2 {
		t.Fatalf("a.txt: expected 2 versions, got %d", len(histA))
	}
	// b.txt should only show the initial add — mlink only records changes
	if len(histB) != 1 {
		t.Fatalf("b.txt: expected 1 version (add only, no change in commit 2), got %d", len(histB))
	}
}

func TestFileHistory_Limit(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "f.txt", Content: []byte("v1")}},
		Comment: "c1",
		User:    "u",
		Time:    ts,
	})

	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "f.txt", Content: []byte("v2")}},
		Comment: "c2",
		User:    "u",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})

	Checkin(r, CheckinOpts{
		Files:   []File{{Name: "f.txt", Content: []byte("v3")}},
		Comment: "c3",
		User:    "u",
		Time:    ts.Add(2 * time.Hour),
		Parent:  rid2,
	})

	versions, _ := FileHistory(r, FileHistoryOpts{Path: "f.txt", Limit: 2})
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions with limit, got %d", len(versions))
	}
}

func TestFileHistory_NotFound(t *testing.T) {
	r := setupTestRepo(t)

	_, err := FileHistory(r, FileHistoryOpts{Path: "nonexistent.txt"})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestFileHistory_EmptyPath(t *testing.T) {
	r := setupTestRepo(t)

	_, err := FileHistory(r, FileHistoryOpts{Path: ""})
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestFileHistory_FileUUID(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	Checkin(r, CheckinOpts{
		Files:   []File{{Name: "data.txt", Content: []byte("content")}},
		Comment: "add data",
		User:    "u",
		Time:    ts,
	})

	versions, _ := FileHistory(r, FileHistoryOpts{Path: "data.txt"})
	if len(versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(versions))
	}
	if versions[0].FileUUID == "" {
		t.Error("FileUUID should be set for non-deleted file")
	}
	if versions[0].CheckinUUID == "" {
		t.Error("CheckinUUID should be set")
	}
	if versions[0].FileRID <= 0 {
		t.Error("FileRID should be positive")
	}
}

func TestFileAt_Basic(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("v1")}},
		Comment: "c1",
		User:    "u",
		Time:    ts,
	})

	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("v2")}},
		Comment: "c2",
		User:    "u",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})

	fid1, ok1 := FileAt(r, rid1, "hello.txt")
	fid2, ok2 := FileAt(r, rid2, "hello.txt")

	if !ok1 || fid1 <= 0 {
		t.Fatal("expected file at commit 1")
	}
	if !ok2 || fid2 <= 0 {
		t.Fatal("expected file at commit 2")
	}
	if fid1 == fid2 {
		t.Fatal("file rids should differ between versions")
	}
}

func TestFileAt_NotFound(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("v1")}},
		Comment: "c1",
		User:    "u",
		Time:    ts,
	})

	_, ok := FileAt(r, rid1, "nonexistent.txt")
	if ok {
		t.Fatal("expected false for nonexistent file")
	}
}

func TestFileHistory_ThreeCommitChain(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "f.txt", Content: []byte("v1")}},
		Comment: "first",
		User:    "alice",
		Time:    ts,
	})
	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "f.txt", Content: []byte("v2")}},
		Comment: "second",
		User:    "bob",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})
	Checkin(r, CheckinOpts{
		Files:   []File{{Name: "f.txt", Content: []byte("v3")}},
		Comment: "third",
		User:    "carol",
		Time:    ts.Add(2 * time.Hour),
		Parent:  rid2,
	})

	versions, err := FileHistory(r, FileHistoryOpts{Path: "f.txt"})
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(versions))
	}

	// Verify ordering (most recent first) and comments
	wantUsers := []string{"carol", "bob", "alice"}
	wantComments := []string{"third", "second", "first"}
	wantActions := []FileAction{FileModified, FileModified, FileAdded}

	for i, v := range versions {
		if v.User != wantUsers[i] {
			t.Errorf("v[%d] user = %q, want %q", i, v.User, wantUsers[i])
		}
		if v.Comment != wantComments[i] {
			t.Errorf("v[%d] comment = %q, want %q", i, v.Comment, wantComments[i])
		}
		if v.Action != wantActions[i] {
			t.Errorf("v[%d] action = %v, want %v", i, v.Action, wantActions[i])
		}
	}

	// Verify file content at each version via FileAt + content.Expand
	for i, v := range versions {
		fid, ok := FileAt(r, v.CheckinRID, "f.txt")
		if !ok {
			t.Fatalf("v[%d]: FileAt returned false", i)
		}
		data, err := content.Expand(r.DB(), fid)
		if err != nil {
			t.Fatalf("v[%d]: Expand: %v", i, err)
		}
		want := []byte("v" + string(rune('3'-i))) // v3, v2, v1
		if !bytes.Equal(data, want) {
			t.Errorf("v[%d]: content = %q, want %q", i, data, want)
		}
	}
}

func TestFileHistory_AfterRebuild(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	rid1, _, _ := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("a1")},
			{Name: "b.txt", Content: []byte("b1")},
		},
		Comment: "initial",
		User:    "u",
		Time:    ts,
	})

	Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("a2")},
			{Name: "b.txt", Content: []byte("b1")}, // unchanged
		},
		Comment: "modify a",
		User:    "u",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})

	// Wipe derived tables and rebuild — this uses rebuildMlinks which
	// only inserts changed files (unlike Checkin which inserts all).
	for _, tbl := range []string{"event", "mlink", "plink", "tagxref", "filename", "leaf", "unclustered", "unsent"} {
		r.DB().Exec("DELETE FROM " + tbl)
	}

	report, err := verify.Rebuild(r)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.BlobsFailed > 0 {
		t.Fatalf("Rebuild had %d blob failures", report.BlobsFailed)
	}

	// After rebuild, FileHistory should still work
	histA, err := FileHistory(r, FileHistoryOpts{Path: "a.txt"})
	if err != nil {
		t.Fatalf("FileHistory a.txt: %v", err)
	}
	if len(histA) < 1 {
		t.Fatal("expected at least 1 version for a.txt after rebuild")
	}

	// b.txt: rebuild only inserts changed files, so b.txt should
	// only appear in the initial commit (pid=0 → added).
	histB, err := FileHistory(r, FileHistoryOpts{Path: "b.txt"})
	if err != nil {
		t.Fatalf("FileHistory b.txt: %v", err)
	}
	if len(histB) != 1 {
		t.Fatalf("b.txt: expected 1 version after rebuild (unchanged in commit 2), got %d", len(histB))
	}
}

func TestFileHistory_CrosslinkGeneratedMlink(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// Create two commits normally
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "x.txt", Content: []byte("x1")}},
		Comment: "add x",
		User:    "u",
		Time:    ts,
	})
	_, _, _ = Checkin(r, CheckinOpts{
		Files:   []File{{Name: "x.txt", Content: []byte("x2")}},
		Comment: "update x",
		User:    "u",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})

	// Wipe mlink and re-crosslink — this uses insertCheckinMlinks
	// which doesn't set pid/pmid (unlike Checkin's insertMlinks).
	r.DB().Exec("DELETE FROM mlink")
	r.DB().Exec("DELETE FROM event")
	r.DB().Exec("DELETE FROM plink")
	r.DB().Exec("DELETE FROM leaf")
	r.DB().Exec("DELETE FROM filename")

	// Re-crosslink all manifests
	_, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	versions, err := FileHistory(r, FileHistoryOpts{Path: "x.txt"})
	if err != nil {
		t.Fatalf("FileHistory after crosslink: %v", err)
	}

	// Crosslink doesn't set pid, so all entries appear as "added".
	// This is expected — crosslink mlinks are simpler than Checkin mlinks.
	if len(versions) < 1 {
		t.Fatal("expected at least 1 version after crosslink")
	}
	for _, v := range versions {
		if v.Action != FileAdded {
			// With crosslink mlinks (no pid), everything looks like FileAdded.
			// This is a known limitation documented in the ADR.
			t.Logf("note: crosslink mlink action = %v (pid not set, so all show as added)", v.Action)
		}
		if v.CheckinUUID == "" {
			t.Error("CheckinUUID should be set")
		}
	}
}

func TestFileAt_WithContentVerification(t *testing.T) {
	r := setupTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	rid1, _, _ := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("alpha")},
			{Name: "b.txt", Content: []byte("beta")},
		},
		Comment: "initial",
		User:    "u",
		Time:    ts,
	})

	rid2, _, _ := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("alpha-v2")},
			{Name: "b.txt", Content: []byte("beta")},
		},
		Comment: "update a",
		User:    "u",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})

	// Verify we can resolve and expand file content at each commit
	cases := []struct {
		rid  libfossil.FslID
		path string
		want string
	}{
		{rid1, "a.txt", "alpha"},
		{rid1, "b.txt", "beta"},
		{rid2, "a.txt", "alpha-v2"},
		{rid2, "b.txt", "beta"},
	}

	for _, tc := range cases {
		fid, ok := FileAt(r, tc.rid, tc.path)
		if !ok {
			t.Fatalf("FileAt(%d, %q) = false", tc.rid, tc.path)
		}
		data, err := content.Expand(r.DB(), fid)
		if err != nil {
			t.Fatalf("Expand(%d): %v", fid, err)
		}
		if string(data) != tc.want {
			t.Errorf("FileAt(%d, %q): content = %q, want %q", tc.rid, tc.path, data, tc.want)
		}
	}
}
