package manifest

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/tag"
	"github.com/danmestas/libfossil/testutil"
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

func TestCheckinBasic(t *testing.T) {
	r := setupTestRepo(t)
	rid, uuid, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("hello world")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if rid <= 0 {
		t.Fatalf("rid = %d", rid)
	}
	if len(uuid) != 40 && len(uuid) != 64 {
		t.Fatalf("uuid len = %d", len(uuid))
	}
	var comment string
	r.DB().QueryRow("SELECT comment FROM event WHERE objid=?", rid).Scan(&comment)
	if comment != "initial commit" {
		t.Fatalf("event comment = %q", comment)
	}
	var leafCount int
	r.DB().QueryRow("SELECT count(*) FROM leaf WHERE rid=?", rid).Scan(&leafCount)
	if leafCount != 1 {
		t.Fatalf("leaf count = %d", leafCount)
	}
}

func TestCheckinFossilRebuild(t *testing.T) {
	if !testutil.HasFossil() {
		t.Skip("fossil not in PATH")
	}
	r := setupTestRepo(t)
	_, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "hello.txt", Content: []byte("hello world")},
			{Name: "src/main.go", Content: []byte("package main\n")},
		},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	r.Close()
	if err := testutil.FossilRebuild(r.Path()); err != nil {
		t.Fatalf("fossil rebuild: %v", err)
	}
}

func TestCheckinMultiple(t *testing.T) {
	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "file.txt", Content: []byte("v1")}},
		Comment: "first",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "file.txt", Content: []byte("v2")}},
		Comment: "second",
		User:    "testuser",
		Parent:  rid1,
		Time:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	var leafCount int
	r.DB().QueryRow("SELECT count(*) FROM leaf WHERE rid=?", rid1).Scan(&leafCount)
	if leafCount != 0 {
		t.Fatal("rid1 still a leaf")
	}
	r.DB().QueryRow("SELECT count(*) FROM leaf WHERE rid=?", rid2).Scan(&leafCount)
	if leafCount != 1 {
		t.Fatal("rid2 not a leaf")
	}
	var plinkCount int
	r.DB().QueryRow("SELECT count(*) FROM plink WHERE pid=? AND cid=?", rid1, rid2).Scan(&plinkCount)
	if plinkCount != 1 {
		t.Fatal("plink missing")
	}
}

func TestGetManifest(t *testing.T) {
	r := setupTestRepo(t)
	rid, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "test commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	})
	d, err := GetManifest(r, rid)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if d.C != "test commit" {
		t.Fatalf("C = %q", d.C)
	}
	if d.Type != deck.Checkin {
		t.Fatalf("Type = %d", d.Type)
	}
	if len(d.F) != 1 || d.F[0].Name != "hello.txt" {
		t.Fatalf("F = %+v", d.F)
	}
}

func TestListFilesBaseline(t *testing.T) {
	r := setupTestRepo(t)
	rid, _, _ := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("aaa")},
			{Name: "b.txt", Content: []byte("bbb")},
		},
		Comment: "initial",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	files, err := ListFiles(r, rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("count = %d", len(files))
	}
}

func TestLogMultiple(t *testing.T) {
	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "first", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v2")}},
		Comment: "second", User: "testuser", Parent: rid1,
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	rid3, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v3")}},
		Comment: "third", User: "testuser", Parent: rid2,
		Time: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
	})
	entries, err := Log(r, LogOpts{Start: rid3})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("count = %d", len(entries))
	}
	if entries[0].Comment != "third" || entries[2].Comment != "first" {
		t.Fatalf("order: %q %q %q", entries[0].Comment, entries[1].Comment, entries[2].Comment)
	}
}

func TestLogWithLimit(t *testing.T) {
	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "first", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v2")}},
		Comment: "second", User: "testuser", Parent: rid1,
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	entries, _ := Log(r, LogOpts{Start: rid2, Limit: 1})
	if len(entries) != 1 {
		t.Fatalf("count = %d", len(entries))
	}
}

func TestCheckinDelta(t *testing.T) {
	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("aaa")},
			{Name: "b.txt", Content: []byte("bbb")},
		},
		Comment: "baseline", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	rid2, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("aaa-modified")},
			{Name: "b.txt", Content: []byte("bbb")},
		},
		Comment: "delta", User: "testuser", Parent: rid1, Delta: true,
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin delta: %v", err)
	}
	d, _ := GetManifest(r, rid2)
	if d.B == "" {
		t.Fatal("delta should have B-card")
	}
	if len(d.F) != 1 || d.F[0].Name != "a.txt" {
		t.Fatalf("delta F = %+v, want only a.txt", d.F)
	}
	files, _ := ListFiles(r, rid2)
	if len(files) != 2 {
		t.Fatalf("ListFiles = %d, want 2", len(files))
	}
}

func TestCheckinDeltaFossilRebuild(t *testing.T) {
	if !testutil.HasFossil() {
		t.Skip("fossil not in PATH")
	}
	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("aaa")},
			{Name: "b.txt", Content: []byte("bbb")},
		},
		Comment: "baseline", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	_, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("aaa-v2")},
			{Name: "b.txt", Content: []byte("bbb")},
			{Name: "c.txt", Content: []byte("new file")},
		},
		Comment: "delta with add", User: "testuser", Parent: rid1, Delta: true,
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	r.Close()
	if err := testutil.FossilRebuild(r.Path()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
}

func TestCheckinWithCustomTags(t *testing.T) {
	r := setupTestRepo(t)

	// Initial checkin (gets default trunk tags)
	rid1, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "file.txt", Content: []byte("v1")}},
		Comment: "initial",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("first checkin: %v", err)
	}

	// Second checkin with custom tags (branch creation)
	rid2, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "file.txt", Content: []byte("v2")}},
		Comment: "branch commit",
		User:    "testuser",
		Parent:  rid1,
		Time:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
		Tags: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feature-x"},
			{Type: deck.TagSingleton, Name: "sym-feature-x", UUID: "*"},
		},
	})
	if err != nil {
		t.Fatalf("second checkin: %v", err)
	}

	d, err := GetManifest(r, rid2)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if len(d.T) != 2 {
		t.Fatalf("expected 2 tags, got %d: %+v", len(d.T), d.T)
	}

	// Verify the propagating branch tag
	found := false
	for _, tag := range d.T {
		if tag.Type == deck.TagPropagating && tag.Name == "branch" && tag.Value == "feature-x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing propagating branch tag in %+v", d.T)
	}

	// Verify the singleton sym tag
	found = false
	for _, tag := range d.T {
		if tag.Type == deck.TagSingleton && tag.Name == "sym-feature-x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing singleton sym-feature-x tag in %+v", d.T)
	}
}

func BenchmarkCheckin(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.fossil")
	r, _ := repo.Create(path, "bench", simio.CryptoRand{}, "")
	defer r.Close()
	files := make([]File, 10)
	for i := range files {
		files[i] = File{Name: fmt.Sprintf("src/file%03d.go", i), Content: bytes.Repeat([]byte("x"), 1000)}
	}
	b.ResetTimer()
	var lastRid libfossil.FslID
	for i := 0; i < b.N; i++ {
		rid, _, _ := Checkin(r, CheckinOpts{
			Files: files, Comment: "bench", User: "bench",
			Parent: lastRid, Time: time.Now().UTC(),
		})
		lastRid = rid
	}
}

func BenchmarkListFiles(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.fossil")
	r, _ := repo.Create(path, "bench", simio.CryptoRand{}, "")
	defer r.Close()
	files := make([]File, 50)
	for i := range files {
		files[i] = File{Name: fmt.Sprintf("src/file%03d.go", i), Content: []byte(fmt.Sprintf("c-%d", i))}
	}
	rid, _, _ := Checkin(r, CheckinOpts{
		Files: files, Comment: "bench", User: "bench", Time: time.Now().UTC(),
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ListFiles(r, rid)
	}
}

func TestCrosslinkInlineTCards(t *testing.T) {
	r := setupTestRepo(t)

	rid, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Clear event and tagxref to simulate post-clone state (blobs exist, but not crosslinked).
	r.DB().Exec("DELETE FROM event")
	r.DB().Exec("DELETE FROM plink")
	r.DB().Exec("DELETE FROM leaf")
	r.DB().Exec("DELETE FROM mlink")
	r.DB().Exec("DELETE FROM tagxref")

	// Run Crosslink.
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	t.Logf("crosslinked %d artifacts", n)

	// Verify inline T-cards were processed.
	// Initial checkin adds: *branch trunk, +sym-trunk
	var branchCount int
	r.DB().QueryRow(
		"SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='branch' AND rid=?", rid,
	).Scan(&branchCount)
	if branchCount != 1 {
		t.Errorf("branch tagxref count=%d, want 1", branchCount)
	}

	var symCount int
	r.DB().QueryRow(
		"SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='sym-trunk' AND rid=?", rid,
	).Scan(&symCount)
	if symCount != 1 {
		t.Errorf("sym-trunk tagxref count=%d, want 1", symCount)
	}
}

func TestCrosslinkControlArtifact(t *testing.T) {
	r := setupTestRepo(t)

	rid, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Create a control artifact via AddTag.
	_, err = tag.AddTag(r, tag.TagOpts{
		TargetRID: rid,
		TagName:   "testlabel",
		TagType:   tag.TagSingleton,
		Value:     "myvalue",
		User:      "testuser",
		Time:      time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("AddTag: %v", err)
	}

	// Clear tagxref to simulate post-clone.
	r.DB().Exec("DELETE FROM tagxref")

	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	t.Logf("crosslinked %d artifacts", n)

	// Verify control artifact's tag was re-applied.
	var count int
	r.DB().QueryRow(
		"SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='testlabel' AND rid=?", rid,
	).Scan(&count)
	if count != 1 {
		t.Errorf("testlabel tagxref count=%d, want 1", count)
	}
}

func TestCrosslinkControlEventRow(t *testing.T) {
	r := setupTestRepo(t)

	// Create a checkin to target
	rid, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Create a control artifact via AddTag
	controlRID, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: rid,
		TagName:   "testlabel",
		TagType:   tag.TagSingleton,
		Value:     "myvalue",
		User:      "testuser",
		Time:      time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("AddTag: %v", err)
	}

	// Clear tagxref and event to simulate post-clone
	r.DB().Exec("DELETE FROM tagxref")
	r.DB().Exec("DELETE FROM event WHERE objid=?", controlRID)

	// Run Crosslink
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 artifact crosslinked, got %d", n)
	}

	// Verify event row (type='g') exists for the control artifact
	var eventType, comment string
	err = r.DB().QueryRow(
		"SELECT type, comment FROM event WHERE objid=?", controlRID,
	).Scan(&eventType, &comment)
	if err != nil {
		t.Fatalf("event query: %v", err)
	}
	if eventType != "g" {
		t.Errorf("event type=%q, want 'g'", eventType)
	}
	if comment == "" {
		t.Errorf("event comment is empty, expected descriptive text")
	}
	t.Logf("event comment: %q", comment)
}

func TestDiscoveryQueryIdempotent(t *testing.T) {
	r := setupTestRepo(t)

	// Create a checkin.
	rid, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Clear event and tagxref to simulate post-clone state.
	r.DB().Exec("DELETE FROM event")
	r.DB().Exec("DELETE FROM plink")
	r.DB().Exec("DELETE FROM leaf")
	r.DB().Exec("DELETE FROM mlink")
	r.DB().Exec("DELETE FROM tagxref")

	// First run should link artifacts.
	n1, err := Crosslink(r)
	if err != nil {
		t.Fatalf("first Crosslink: %v", err)
	}
	if n1 < 1 {
		t.Fatalf("first run linked %d, want >= 1", n1)
	}
	t.Logf("first run linked %d artifacts", n1)

	// Verify checkin was crosslinked.
	var eventCount int
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("event count=%d, want 1", eventCount)
	}

	// Second run should link nothing (idempotent).
	n2, err := Crosslink(r)
	if err != nil {
		t.Fatalf("second Crosslink: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second run linked %d, want 0 (idempotent)", n2)
	}
	t.Logf("second run linked %d artifacts (idempotent)", n2)
}

func TestCrosslinkCherrypick(t *testing.T) {
	r := setupTestRepo(t)

	// Create initial checkin
	rid1, uuid1, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "file.txt", Content: []byte("v1")}},
		Comment: "first",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("first Checkin: %v", err)
	}

	// Create second checkin (parent of cherrypick target)
	rid2, uuid2, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "file.txt", Content: []byte("v2")}},
		Comment: "second",
		User:    "testuser",
		Parent:  rid1,
		Time:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("second Checkin: %v", err)
	}

	// Manually create a manifest with Q-card (cherrypick)
	// Since Checkin doesn't support Q-cards, we construct the deck manually
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "cherrypick commit",
		D:    time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
		U:    "testuser",
		P:    []string{uuid1}, // parent is rid1
		Q: []deck.CherryPick{
			{Target: uuid2, IsBackout: false}, // cherrypick rid2
		},
		F: []deck.FileCard{
			{Name: "file.txt", UUID: uuid2}, // use file from rid2
		},
	}

	// Compute R-card (manifest hash)
	d.R = "0000000000000000000000000000000000000000" // simplified for test

	// Marshal and store the manifest blob
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rid3, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Clear crosslink tables to simulate post-clone
	r.DB().Exec("DELETE FROM event WHERE objid=?", rid3)
	r.DB().Exec("DELETE FROM plink WHERE cid=?", rid3)
	r.DB().Exec("DELETE FROM mlink WHERE mid=?", rid3)
	r.DB().Exec("DELETE FROM cherrypick WHERE childid=?", rid3)

	// Run Crosslink
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 artifact crosslinked, got %d", n)
	}

	// Verify cherrypick table
	var count int
	r.DB().QueryRow(
		"SELECT count(*) FROM cherrypick WHERE parentid=? AND childid=? AND isExclude=0",
		rid2, rid3,
	).Scan(&count)
	if count != 1 {
		t.Errorf("cherrypick count=%d, want 1", count)
	}
}

func TestCrosslinkWiki(t *testing.T) {
	r := setupTestRepo(t)

	// Manually create a wiki manifest
	wikiTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	wikiContent := []byte("This is a test wiki page")
	d := &deck.Deck{
		Type: deck.Wiki,
		L:    "TestPage",
		U:    "testuser",
		W:    wikiContent,
		D:    wikiTime,
	}

	// Marshal and store the wiki manifest blob
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rid, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Clear crosslink tables to simulate post-clone
	r.DB().Exec("DELETE FROM event WHERE objid=?", rid)
	r.DB().Exec("DELETE FROM tagxref WHERE rid=?", rid)

	// Run Crosslink
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 artifact crosslinked, got %d", n)
	}

	// Verify event row (type='w')
	var eventType, comment string
	err = r.DB().QueryRow(
		"SELECT type, comment FROM event WHERE objid=?", rid,
	).Scan(&eventType, &comment)
	if err != nil {
		t.Fatalf("event query: %v", err)
	}
	if eventType != "w" {
		t.Errorf("event type=%q, want 'w'", eventType)
	}
	if comment != "+TestPage" {
		t.Errorf("event comment=%q, want '+TestPage'", comment)
	}

	// Verify wiki-TestPage tag in tagxref
	var tagCount int
	r.DB().QueryRow(`
		SELECT count(*) FROM tagxref
		JOIN tag USING(tagid)
		WHERE tagname='wiki-TestPage' AND rid=?
	`, rid).Scan(&tagCount)
	if tagCount != 1 {
		t.Errorf("wiki-TestPage tag count=%d, want 1", tagCount)
	}
}

func TestCrosslinkTicket(t *testing.T) {
	r := setupTestRepo(t)

	// Manually create a ticket manifest
	ticketTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	ticketUUID := "0123456789abcdef0123456789abcdef01234567"
	d := &deck.Deck{
		Type: deck.Ticket,
		K:    ticketUUID,
		U:    "testuser",
		D:    ticketTime,
		J: []deck.TicketField{
			{Name: "title", Value: "Test ticket"},
			{Name: "status", Value: "Open"},
		},
	}

	// Marshal and store the ticket manifest blob
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rid, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Clear crosslink tables to simulate post-clone
	r.DB().Exec("DELETE FROM tagxref WHERE rid=?", rid)

	// Run Crosslink
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 artifact crosslinked, got %d", n)
	}

	// Verify tkt-* tag in tagxref
	var tagCount int
	r.DB().QueryRow(`
		SELECT count(*) FROM tagxref
		JOIN tag USING(tagid)
		WHERE tagname=? AND rid=?
	`, fmt.Sprintf("tkt-%s", ticketUUID), rid).Scan(&tagCount)
	if tagCount != 1 {
		t.Errorf("tkt-%s tag count=%d, want 1", ticketUUID, tagCount)
	}
}

func TestCrosslinkEvent(t *testing.T) {
	r := setupTestRepo(t)

	// Manually create an event (technote) manifest
	eventTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	eventUUID := "fedcba9876543210fedcba9876543210fedcba98"
	eventBody := []byte("This is a technote body")
	d := &deck.Deck{
		Type: deck.Event,
		E: &deck.EventCard{
			Date: eventTime,
			UUID: eventUUID,
		},
		U: "testuser",
		D: eventTime,
		W: eventBody,
		C: "Test technote",
	}

	// Marshal and store the event manifest blob
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rid, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Clear crosslink tables to simulate post-clone
	r.DB().Exec("DELETE FROM event WHERE objid=?", rid)
	r.DB().Exec("DELETE FROM tagxref WHERE rid=?", rid)

	// Run Crosslink
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 artifact crosslinked, got %d", n)
	}

	// Verify event row (type='e')
	var eventType, comment string
	err = r.DB().QueryRow(
		"SELECT type, comment FROM event WHERE objid=?", rid,
	).Scan(&eventType, &comment)
	if err != nil {
		t.Fatalf("event query: %v", err)
	}
	if eventType != "e" {
		t.Errorf("event type=%q, want 'e'", eventType)
	}
	if comment != "Test technote" {
		t.Errorf("event comment=%q, want 'Test technote'", comment)
	}

	// Verify event-* tag in tagxref
	var tagCount int
	r.DB().QueryRow(`
		SELECT count(*) FROM tagxref
		JOIN tag USING(tagid)
		WHERE tagname=? AND rid=?
	`, fmt.Sprintf("event-%s", eventUUID), rid).Scan(&tagCount)
	if tagCount != 1 {
		t.Errorf("event-%s tag count=%d, want 1", eventUUID, tagCount)
	}
}

func TestCrosslinkAttachment(t *testing.T) {
	r := setupTestRepo(t)

	// Manually create an attachment manifest with wiki target
	attachTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	d := &deck.Deck{
		Type: deck.Attachment,
		A: &deck.AttachmentCard{
			Filename: "test.txt",
			Target:   "TestPage",
			Source:   "abc123",
		},
		U: "testuser",
		D: attachTime,
		C: "Attach test file",
	}

	// Marshal and store the attachment manifest blob
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rid, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Clear crosslink tables to simulate post-clone
	r.DB().Exec("DELETE FROM attachment WHERE attachid=?", rid)
	r.DB().Exec("DELETE FROM event WHERE objid=?", rid)

	// Run Crosslink
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 artifact crosslinked, got %d", n)
	}

	// Verify attachment table row
	var target, filename, src string
	var isLatestRaw any
	err = r.DB().QueryRow(
		"SELECT target, filename, src, isLatest FROM attachment WHERE attachid=?", rid,
	).Scan(&target, &filename, &src, &isLatestRaw)
	if err != nil {
		t.Fatalf("attachment query: %v", err)
	}
	if target != "TestPage" {
		t.Errorf("target=%q, want 'TestPage'", target)
	}
	if filename != "test.txt" {
		t.Errorf("filename=%q, want 'test.txt'", filename)
	}
	if src != "abc123" {
		t.Errorf("src=%q, want 'abc123'", src)
	}
	isLatest, _ := db.ScanInt(isLatestRaw)
	if isLatest != 1 {
		t.Errorf("isLatest=%d, want 1", isLatest)
	}

	// Verify event row (type='w' for wiki attachment)
	var eventType, comment string
	err = r.DB().QueryRow(
		"SELECT type, comment FROM event WHERE objid=?", rid,
	).Scan(&eventType, &comment)
	if err != nil {
		t.Fatalf("event query: %v", err)
	}
	if eventType != "w" {
		t.Errorf("event type=%q, want 'w'", eventType)
	}
	if comment != "Add attachment test.txt to wiki page TestPage" {
		t.Errorf("event comment=%q", comment)
	}
}

func TestCrosslinkCluster(t *testing.T) {
	r := setupTestRepo(t)

	// Store 2 blobs that will be members of the cluster
	blob1Data := []byte("test blob 1")
	rid1, uuid1, err := blob.Store(r.DB(), blob1Data)
	if err != nil {
		t.Fatalf("Store blob1: %v", err)
	}

	blob2Data := []byte("test blob 2")
	rid2, uuid2, err := blob.Store(r.DB(), blob2Data)
	if err != nil {
		t.Fatalf("Store blob2: %v", err)
	}

	// Verify both blobs are initially in unclustered table
	r.DB().Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid1)
	r.DB().Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid2)

	var count1, count2 int
	r.DB().QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid1).Scan(&count1)
	r.DB().QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid2).Scan(&count2)
	if count1 != 1 || count2 != 1 {
		t.Fatalf("blobs not in unclustered: rid1=%d rid2=%d", count1, count2)
	}

	// Create a cluster manifest with these two blobs as members
	clusterTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	d := &deck.Deck{
		Type: deck.Cluster,
		M:    []string{uuid1, uuid2},
		D:    clusterTime,
	}

	// Marshal and store the cluster manifest blob
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rid, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Clear crosslink tables to simulate post-clone
	r.DB().Exec("DELETE FROM tagxref WHERE rid=?", rid)

	// Run Crosslink
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 artifact crosslinked, got %d", n)
	}

	// Verify cluster tag
	var tagCount int
	r.DB().QueryRow(`
		SELECT count(*) FROM tagxref
		JOIN tag USING(tagid)
		WHERE tagname='cluster' AND rid=?
	`, rid).Scan(&tagCount)
	if tagCount != 1 {
		t.Errorf("cluster tag count=%d, want 1", tagCount)
	}

	// Verify both blobs were removed from unclustered table
	r.DB().QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid1).Scan(&count1)
	r.DB().QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid2).Scan(&count2)
	if count1 != 0 {
		t.Errorf("rid1 still in unclustered, count=%d", count1)
	}
	if count2 != 0 {
		t.Errorf("rid2 still in unclustered, count=%d", count2)
	}
}

func TestCrosslinkForum(t *testing.T) {
	r := setupTestRepo(t)

	// Create a forum post (thread starter)
	forumTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	forumContent := []byte("This is a test forum post")
	d := &deck.Deck{
		Type: deck.ForumPost,
		H:    "Discussion about sync",
		U:    "testuser",
		W:    forumContent,
		D:    forumTime,
	}

	// Marshal and store the forum manifest blob
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rid, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Clear crosslink tables to simulate post-clone
	r.DB().Exec("DELETE FROM event WHERE objid=?", rid)
	r.DB().Exec("DELETE FROM forumpost WHERE fpid=?", rid)

	// Run Crosslink
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 artifact crosslinked, got %d", n)
	}

	// Verify forumpost row with froot=self (thread starter)
	var froot int64
	var fprev, firt interface{}
	var fmtime float64
	err = r.DB().QueryRow(
		"SELECT froot, fprev, firt, fmtime FROM forumpost WHERE fpid=?", rid,
	).Scan(&froot, &fprev, &firt, &fmtime)
	if err != nil {
		t.Fatalf("forumpost query: %v", err)
	}
	if froot != int64(rid) {
		t.Errorf("froot=%d, want %d (self)", froot, rid)
	}
	if fprev != nil {
		t.Errorf("fprev=%v, want nil (no previous edit)", fprev)
	}
	if firt != nil {
		t.Errorf("firt=%v, want nil (thread starter)", firt)
	}

	// Verify event row (type='f')
	var eventType, comment string
	err = r.DB().QueryRow(
		"SELECT type, comment FROM event WHERE objid=?", rid,
	).Scan(&eventType, &comment)
	if err != nil {
		t.Fatalf("event query: %v", err)
	}
	if eventType != "f" {
		t.Errorf("event type=%q, want 'f'", eventType)
	}
	if comment != "Post: Discussion about sync" {
		t.Errorf("event comment=%q, want 'Post: Discussion about sync'", comment)
	}
}

func TestCrosslinkTwoPass(t *testing.T) {
	r := setupTestRepo(t)

	// Store a wiki manifest blob (Type=Wiki, L="TwoPassPage", U="test", W=content, D=time).
	wikiTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	wikiContent := []byte("Two-pass wiki content")
	wikiDeck := &deck.Deck{
		Type: deck.Wiki,
		L:    "TwoPassPage",
		U:    "testuser",
		W:    wikiContent,
		D:    wikiTime,
	}
	wikiBytes, err := wikiDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal wiki: %v", err)
	}
	wikiRid, _, err := blob.Store(r.DB(), wikiBytes)
	if err != nil {
		t.Fatalf("Store wiki: %v", err)
	}

	// Create a checkin via Checkin().
	checkinRid, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "file.txt", Content: []byte("checkin content")}},
		Comment: "test checkin",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Clear all event and tagxref rows to simulate post-clone state.
	r.DB().Exec("DELETE FROM event")
	r.DB().Exec("DELETE FROM tagxref")
	r.DB().Exec("DELETE FROM plink")
	r.DB().Exec("DELETE FROM leaf")
	r.DB().Exec("DELETE FROM mlink")

	// Call Crosslink once.
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected n >= 2 artifacts crosslinked, got %d", n)
	}
	t.Logf("crosslinked %d artifacts", n)

	// Verify: wiki event type='w' exists.
	var wikiEventType string
	err = r.DB().QueryRow("SELECT type FROM event WHERE objid=?", wikiRid).Scan(&wikiEventType)
	if err != nil {
		t.Errorf("wiki event query: %v", err)
	} else if wikiEventType != "w" {
		t.Errorf("wiki event type=%q, want 'w'", wikiEventType)
	}

	// Verify: checkin event type='ci' exists.
	var checkinEventType string
	err = r.DB().QueryRow("SELECT type FROM event WHERE objid=?", checkinRid).Scan(&checkinEventType)
	if err != nil {
		t.Errorf("checkin event query: %v", err)
	} else if checkinEventType != "ci" {
		t.Errorf("checkin event type=%q, want 'ci'", checkinEventType)
	}
}
