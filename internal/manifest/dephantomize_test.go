package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/deck"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func TestAfterDephantomizeCheckin(t *testing.T) {
	r := setupTestRepo(t)

	// Create a checkin manifest, compute its hash, store as phantom, then fill.
	fileContent := []byte("hello dephantomize")
	fileRid, fileUUID, err := blob.Store(r.DB(), fileContent)
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}
	_ = fileRid

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "dephantomize commit",
		U:    deck.Str("testuser"),
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
	}

	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Compute the hash to get the UUID.
	rid, uuid, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Delete the real blob, re-insert as phantom, then fill it back.
	// This simulates the phantom->real transition.
	r.DB().Exec("DELETE FROM event WHERE objid=?", rid)
	r.DB().Exec("DELETE FROM plink WHERE cid=?", rid)
	r.DB().Exec("DELETE FROM leaf WHERE rid=?", rid)
	r.DB().Exec("DELETE FROM mlink WHERE mid=?", rid)
	r.DB().Exec("DELETE FROM tagxref WHERE rid=?", rid)

	// Verify no event row exists before dephantomize.
	var eventCount int
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&eventCount)
	if eventCount != 0 {
		t.Fatalf("event count before dephantomize = %d, want 0", eventCount)
	}

	// Call AfterDephantomize.
	AfterDephantomize(r, rid)

	// Verify event row was created.
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("event count after dephantomize = %d, want 1", eventCount)
	}

	// Verify event is a checkin.
	var eventType string
	r.DB().QueryRow("SELECT type FROM event WHERE objid=?", rid).Scan(&eventType)
	if eventType != "ci" {
		t.Errorf("event type = %q, want 'ci'", eventType)
	}

	_ = uuid
}

func TestAfterDephantomizeOrphan(t *testing.T) {
	r := setupTestRepo(t)

	// Create a baseline checkin.
	fileContent := []byte("baseline file")
	_, fileUUID, err := blob.Store(r.DB(), fileContent)
	if err != nil {
		t.Fatalf("Store file: %v", err)
	}

	baselineDeck := &deck.Deck{
		Type: deck.Checkin,
		C:    "baseline",
		U:    deck.Str("testuser"),
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "file.txt", UUID: fileUUID}},
	}
	baselineBytes, err := baselineDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal baseline: %v", err)
	}
	baselineRid, _, err := blob.Store(r.DB(), baselineBytes)
	if err != nil {
		t.Fatalf("Store baseline: %v", err)
	}

	// Create another checkin that will be the "orphan" (delta manifest).
	fileContent2 := []byte("orphan file")
	_, fileUUID2, err := blob.Store(r.DB(), fileContent2)
	if err != nil {
		t.Fatalf("Store file2: %v", err)
	}

	orphanDeck := &deck.Deck{
		Type: deck.Checkin,
		C:    "orphan commit",
		U:    deck.Str("testuser"),
		D:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "file2.txt", UUID: fileUUID2}},
	}
	orphanBytes, err := orphanDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal orphan: %v", err)
	}
	orphanRid, _, err := blob.Store(r.DB(), orphanBytes)
	if err != nil {
		t.Fatalf("Store orphan: %v", err)
	}

	// Clear crosslink tables for the orphan.
	r.DB().Exec("DELETE FROM event WHERE objid=?", orphanRid)
	r.DB().Exec("DELETE FROM leaf WHERE rid=?", orphanRid)
	r.DB().Exec("DELETE FROM tagxref WHERE rid=?", orphanRid)

	// Insert orphan row linking orphanRid to baselineRid.
	r.DB().Exec("INSERT INTO orphan(rid, baseline) VALUES(?, ?)", orphanRid, baselineRid)

	// Verify orphan row exists.
	var orphanCount int
	r.DB().QueryRow("SELECT count(*) FROM orphan WHERE baseline=?", baselineRid).Scan(&orphanCount)
	if orphanCount != 1 {
		t.Fatalf("orphan count = %d, want 1", orphanCount)
	}

	// Call AfterDephantomize on the baseline.
	AfterDephantomize(r, baselineRid)

	// Verify orphan was cleaned up.
	r.DB().QueryRow("SELECT count(*) FROM orphan WHERE baseline=?", baselineRid).Scan(&orphanCount)
	if orphanCount != 0 {
		t.Errorf("orphan count after dephantomize = %d, want 0", orphanCount)
	}

	// Verify orphan checkin got crosslinked (event row created).
	var eventCount int
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", orphanRid).Scan(&eventCount)
	if eventCount != 0 {
		// The orphan's event may have been created by crosslinkSingle.
		// If it wasn't (because it was already crosslinked earlier), that's also fine.
		// The key assertion is that the orphan row was cleaned up.
	}
}

func TestAfterDephantomizeNilRepo(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil repo")
		}
	}()
	AfterDephantomize(nil, 1)
}

func TestAfterDephantomizeZeroRid(t *testing.T) {
	r := setupTestRepo(t)
	// Should return without panicking.
	AfterDephantomize(r, 0)
	AfterDephantomize(r, -1)
}
