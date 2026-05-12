package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/xfer"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func newCkinLockTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// insertLeafBlob inserts a blob and marks it as a leaf, returning the UUID.
func insertLeafBlob(t *testing.T, r *repo.Repo, uuid string) {
	t.Helper()
	d := r.DB()
	d.Exec("INSERT INTO blob(uuid, size, content) VALUES(?, 0, X'')", uuid)
	var rid int64
	if err := d.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		t.Fatalf("insertLeafBlob: %v", err)
	}
	d.Exec("INSERT INTO leaf(rid) VALUES(?)", rid)
}

func TestCkinLock_Acquire(t *testing.T) {
	r := newCkinLockTestRepo(t)
	uuid := "acq00000000000000000000000000000000000000"
	insertLeafBlob(t, r, uuid)
	fail := processCkinLock(r.DB(), uuid, "client-A", "alice", DefaultCkinLockTimeout)
	if fail != nil {
		t.Fatalf("expected nil, got CkinLockFail{HeldBy: %q}", fail.HeldBy)
	}
}

func TestCkinLock_SameClient(t *testing.T) {
	r := newCkinLockTestRepo(t)
	d := r.DB()
	uuid := "same0000000000000000000000000000000000000"
	insertLeafBlob(t, r, uuid)
	fail := processCkinLock(d, uuid, "client-A", "alice", DefaultCkinLockTimeout)
	if fail != nil {
		t.Fatalf("first acquire failed: %+v", fail)
	}
	fail = processCkinLock(d, uuid, "client-A", "alice", DefaultCkinLockTimeout)
	if fail != nil {
		t.Fatalf("same-client re-acquire should succeed, got: %+v", fail)
	}
}

func TestCkinLock_Conflict(t *testing.T) {
	r := newCkinLockTestRepo(t)
	d := r.DB()
	uuid := "conflict0000000000000000000000000000000000"
	insertLeafBlob(t, r, uuid)

	fail := processCkinLock(d, uuid, "client-A", "alice", DefaultCkinLockTimeout)
	if fail != nil {
		t.Fatalf("first acquire failed: %+v", fail)
	}
	fail = processCkinLock(d, uuid, "client-B", "bob", DefaultCkinLockTimeout)
	if fail == nil {
		t.Fatal("expected CkinLockFail for different client, got nil")
	}
	if fail.HeldBy != "alice" {
		t.Errorf("HeldBy = %q, want %q", fail.HeldBy, "alice")
	}
}

func TestCkinLock_Expiry(t *testing.T) {
	r := newCkinLockTestRepo(t)
	d := r.DB()
	uuid := "exp00000000000000000000000000000000000000"
	insertLeafBlob(t, r, uuid)
	// Acquire lock for client-A.
	fail := processCkinLock(d, uuid, "client-A", "alice", DefaultCkinLockTimeout)
	if fail != nil {
		t.Fatalf("first acquire failed: %+v", fail)
	}
	// Backdate the lock's mtime by 120 seconds so it appears stale.
	key := configKey(uuid)
	stale := time.Now().Unix() - 120
	d.Exec("UPDATE config SET mtime=? WHERE name=?", stale, key)

	// Different client should now succeed because the lock is expired.
	fail = processCkinLock(d, uuid, "client-B", "bob", DefaultCkinLockTimeout)
	if fail != nil {
		t.Fatalf("expected expired lock to be reaped, got: %+v", fail)
	}
}

func TestCkinLock_ParentNotLeaf(t *testing.T) {
	r := newCkinLockTestRepo(t)
	d := r.DB()

	// Insert a blob that is NOT in the leaf table.
	uuid := "deadbeef00000000000000000000000000000000"
	d.Exec("INSERT INTO blob(uuid, size, content) VALUES(?, 0, X'')", uuid)

	// Acquire lock for client-A on this non-leaf blob.
	fail := processCkinLock(d, uuid, "client-A", "alice", DefaultCkinLockTimeout)
	if fail != nil {
		t.Fatalf("first acquire failed: %+v", fail)
	}

	// Different client should succeed because the parent is not a leaf.
	fail = processCkinLock(d, uuid, "client-B", "bob", DefaultCkinLockTimeout)
	if fail != nil {
		t.Fatalf("expected non-leaf parent lock to be cleaned, got: %+v", fail)
	}
}

func TestCkinLock_SyncRoundTrip(t *testing.T) {
	serverRepo := newCkinLockTestRepo(t)

	// Insert a leaf blob on the server so the lock doesn't get cleaned
	// up by expireStaleLocks (which removes locks for non-leaf parents).
	parentUUID := "abcd0000000000000000000000000000000000ab"
	insertLeafBlob(t, serverRepo, parentUUID)

	transport := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, _ := HandleSync(context.Background(), serverRepo, req)
			return resp
		},
	}

	clientRepo := newCkinLockTestRepo(t)
	result, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true,
		CkinLock: &CkinLockReq{
			ParentUUID: parentUUID,
			ClientID:   "client-1",
		},
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.CkinLockFail != nil {
		t.Fatalf("unexpected lock fail: %+v", result.CkinLockFail)
	}

	// Second client should get lock-fail.
	result2, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true,
		CkinLock: &CkinLockReq{
			ParentUUID: parentUUID,
			ClientID:   "client-2",
		},
	})
	if err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	if result2.CkinLockFail == nil {
		t.Fatal("expected lock fail for second client")
	}
	// Default user with no login card is "nobody"
	if result2.CkinLockFail.HeldBy != "nobody" {
		t.Fatalf("HeldBy = %q, want %q", result2.CkinLockFail.HeldBy, "nobody")
	}
}
