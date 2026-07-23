package shun_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/shun"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func setupDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.CreateRepoSchema(d); err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

const testUUID = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

func TestAddAndIsShunned(t *testing.T) {
	d := setupDB(t)

	ok, err := shun.IsShunned(d, testUUID)
	if err != nil {
		t.Fatalf("IsShunned: %v", err)
	}
	if ok {
		t.Fatal("expected not shunned before Add")
	}

	if err := shun.Add(d, testUUID, "test shun"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ok, err = shun.IsShunned(d, testUUID)
	if err != nil {
		t.Fatalf("IsShunned: %v", err)
	}
	if !ok {
		t.Fatal("expected shunned after Add")
	}
}

func TestRemove(t *testing.T) {
	d := setupDB(t)

	if err := shun.Add(d, testUUID, "to remove"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := shun.Remove(d, testUUID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	ok, err := shun.IsShunned(d, testUUID)
	if err != nil {
		t.Fatalf("IsShunned: %v", err)
	}
	if ok {
		t.Fatal("expected not shunned after Remove")
	}
}

func TestRemoveNoop(t *testing.T) {
	d := setupDB(t)
	// Remove non-existent UUID should not error.
	if err := shun.Remove(d, testUUID); err != nil {
		t.Fatalf("Remove (noop): %v", err)
	}
}

func TestAddInvalidUUID(t *testing.T) {
	d := setupDB(t)
	if err := shun.Add(d, "not-a-valid-hash", "bad"); err == nil {
		t.Fatal("expected error for invalid UUID")
	}
}

func TestAddIdempotent(t *testing.T) {
	d := setupDB(t)

	if err := shun.Add(d, testUUID, "first"); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	if err := shun.Add(d, testUUID, "second"); err != nil {
		t.Fatalf("Add second: %v", err)
	}

	entries, err := shun.List(d)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Comment != "second" {
		t.Errorf("comment = %q, want %q", entries[0].Comment, "second")
	}
}

func TestPurgeStandaloneBlob(t *testing.T) {
	d := setupDB(t)

	// Store a blob and shun it.
	rid, uuid, err := blob.Store(d, []byte("secret content"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if err := shun.Add(d, uuid, "purge test"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	result, err := shun.Purge(d)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}

	if result.BlobsDeleted != 1 {
		t.Errorf("BlobsDeleted = %d, want 1", result.BlobsDeleted)
	}

	// Verify blob is gone.
	var count int
	d.QueryRow("SELECT COUNT(*) FROM blob WHERE rid=?", rid).Scan(&count)
	if count != 0 {
		t.Error("blob row still exists after purge")
	}
}

func TestPurgeEmpty(t *testing.T) {
	d := setupDB(t)

	result, err := shun.Purge(d)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.BlobsDeleted != 0 || result.DeltasExpanded != 0 || result.PrivateCleaned != 0 {
		t.Errorf("expected zero counts, got %+v", result)
	}
}

func TestPurgeDeltaChain(t *testing.T) {
	d := setupDB(t)

	// Blob A (full) is delta source for blob B.
	contentA := []byte("this is the base content for blob A, long enough for delta")
	contentB := []byte("this is the base content for blob B, long enough for delta")

	ridA, uuidA, err := blob.Store(d, contentA)
	if err != nil {
		t.Fatalf("Store A: %v", err)
	}

	ridB, _, err := blob.StoreDelta(d, contentB, ridA)
	if err != nil {
		t.Fatalf("StoreDelta B: %v", err)
	}

	// Verify B is a delta before purge.
	var srcid int64
	err = d.QueryRow("SELECT srcid FROM delta WHERE rid=?", ridB).Scan(&srcid)
	if err != nil {
		t.Fatalf("B should be a delta: %v", err)
	}

	// Shun A and purge.
	if err := shun.Add(d, uuidA, "shun base"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	result, err := shun.Purge(d)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}

	if result.BlobsDeleted != 1 {
		t.Errorf("BlobsDeleted = %d, want 1", result.BlobsDeleted)
	}
	if result.DeltasExpanded != 1 {
		t.Errorf("DeltasExpanded = %d, want 1", result.DeltasExpanded)
	}

	// Verify A is gone.
	var count int
	d.QueryRow("SELECT COUNT(*) FROM blob WHERE rid=?", ridA).Scan(&count)
	if count != 0 {
		t.Error("blob A still exists after purge")
	}

	// Verify B is no longer a delta.
	err = d.QueryRow("SELECT srcid FROM delta WHERE rid=?", ridB).Scan(&srcid)
	if err == nil {
		t.Error("B is still a delta after purge, expected standalone")
	}

	// Verify B content is still readable.
	got, err := content.Expand(d, ridB)
	if err != nil {
		t.Fatalf("Expand B after purge: %v", err)
	}
	if !bytes.Equal(got, contentB) {
		t.Errorf("B content mismatch after purge: got %d bytes, want %d", len(got), len(contentB))
	}
}

func TestPurgeOrphanPrivate(t *testing.T) {
	d := setupDB(t)

	rid, uuid, err := blob.Store(d, []byte("private secret"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Mark as private.
	_, err = d.Exec("INSERT INTO private(rid) VALUES(?)", rid)
	if err != nil {
		t.Fatalf("insert private: %v", err)
	}

	// Shun and purge.
	if err := shun.Add(d, uuid, "private shun"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	result, err := shun.Purge(d)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}

	if result.PrivateCleaned != 1 {
		t.Errorf("PrivateCleaned = %d, want 1", result.PrivateCleaned)
	}

	// Verify private row is gone.
	var count int
	d.QueryRow("SELECT COUNT(*) FROM private WHERE rid=?", rid).Scan(&count)
	if count != 0 {
		t.Error("private row still exists after purge")
	}
}

func TestPurgeCorruptDeltaRollback(t *testing.T) {
	d := setupDB(t)

	// Store blob A (full).
	contentA := []byte("base content for corruption test, needs to be long enough")
	ridA, uuidA, err := blob.Store(d, contentA)
	if err != nil {
		t.Fatalf("Store A: %v", err)
	}

	// Store blob B as delta from A, then corrupt B's content.
	ridB, _, err := blob.StoreDelta(d, []byte("derived content for corruption test, also long"), ridA)
	if err != nil {
		t.Fatalf("StoreDelta B: %v", err)
	}

	// Corrupt B's blob content so delta expansion fails.
	_, err = d.Exec("UPDATE blob SET content=X'DEADBEEF' WHERE rid=?", ridB)
	if err != nil {
		t.Fatalf("corrupt B: %v", err)
	}

	// Shun A and attempt purge — should fail and roll back.
	if err := shun.Add(d, uuidA, "corrupt test"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err = shun.Purge(d)
	if err == nil {
		t.Fatal("expected Purge to fail with corrupt delta")
	}
	t.Logf("Purge error (expected): %v", err)

	// Verify both blobs still exist (transaction rolled back).
	var countA, countB int
	d.QueryRow("SELECT COUNT(*) FROM blob WHERE rid=?", ridA).Scan(&countA)
	d.QueryRow("SELECT COUNT(*) FROM blob WHERE rid=?", ridB).Scan(&countB)
	if countA != 1 {
		t.Error("blob A should still exist after failed purge")
	}
	if countB != 1 {
		t.Error("blob B should still exist after failed purge")
	}
}
