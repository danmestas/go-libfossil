package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

func newCkinLockTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "test", simio.CryptoRand{}, testPC)
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
	// nobody holds no Write capability and still takes the lock (§9.4).

	transport := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, _ := HandleSync(context.Background(), serverRepo, req)
			return resp
		},
	}

	// The client holds the parent it locks, as a client committing on top
	// of it does. The server's copy has no content to send, and the error a
	// gimme for it would draw ends the reply before the ci-lock-fail that
	// follows it in card order.
	clientRepo := newCkinLockTestRepo(t)
	insertLeafBlob(t, clientRepo, parentUUID)
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
	// nobody's lock row has a null login, which ci-lock-fail names nobody.
	if result2.CkinLockFail.HeldBy != "nobody" {
		t.Fatalf("HeldBy = %q, want %q", result2.CkinLockFail.HeldBy, "nobody")
	}
}

// TestCkinLock_SharesFossilTable pins the lock table to fossil's: a row
// fossil's ci-lock wrote (config ci-lock-HASH, JSON login and clientid, the
// time in the mtime column) is the lock this server sees, and a live row is
// kept as it is, with no refresh, even for the client holding it (§9.4).
func TestCkinLock_SharesFossilTable(t *testing.T) {
	r := newCkinLockTestRepo(t)
	d := r.DB()
	uuid := "f0551100000000000000000000000000000000ab"
	insertLeafBlob(t, r, uuid)
	since := time.Now().Unix() - 30
	if _, err := d.Exec("INSERT INTO config(name, value, mtime) VALUES(?,"+
		" json_object('login', 'bob', 'clientid', 'client-A'), ?)",
		"ci-lock-"+uuid, since); err != nil {
		t.Fatal(err)
	}
	fail := processCkinLock(d, uuid, "client-B", "alice", DefaultCkinLockTimeout)
	if fail == nil || fail.HeldBy != "bob" || fail.Since.Unix() != since {
		t.Fatalf("fossil's lock: got %+v, want held by bob since %d", fail, since)
	}
	if fail := processCkinLock(d, uuid, "client-A", "alice", DefaultCkinLockTimeout); fail != nil {
		t.Fatalf("same client: got %+v, want no failure", fail)
	}
	var login string
	var mtime int64
	if err := d.QueryRow("SELECT json_extract(value, '$.login'), CAST(mtime AS INTEGER)"+
		" FROM config WHERE name=?", "ci-lock-"+uuid).Scan(&login, &mtime); err != nil {
		t.Fatal(err)
	}
	if login != "bob" || mtime != since {
		t.Fatalf("lock now held by %s since %d, want bob since %d", login, mtime, since)
	}
}

// TestCkinLock_DropsLegacyRows checks that lock rows under the old
// libfossil-ci-lock- key, which no sweep matches any longer, are cleared.
func TestCkinLock_DropsLegacyRows(t *testing.T) {
	r := newCkinLockTestRepo(t)
	d := r.DB()
	d.Exec("INSERT INTO config(name, value, mtime) VALUES(?, ?, ?)",
		"libfossil-ci-lock-aaaa", "bob|client-B", time.Now().Unix())
	uuid := "leg00000000000000000000000000000000000000"
	insertLeafBlob(t, r, uuid)

	if fail := processCkinLock(d, uuid, "client-A", "alice", DefaultCkinLockTimeout); fail != nil {
		t.Fatalf("unexpected conflict: %+v", fail)
	}
	var n int
	d.QueryRow("SELECT count(*) FROM config WHERE name GLOB 'libfossil-ci-lock-*'").Scan(&n)
	if n != 0 {
		t.Fatalf("%d legacy lock rows remain", n)
	}
}

// TestLockTimeout_ReadsConfig checks that the lock-timeout setting sets how
// long a lock lives, read as fossil's db_get_int reads it: a value that is
// not a number is 0, and 0 or less is kept, making every lock stale.
func TestLockTimeout_ReadsConfig(t *testing.T) {
	d := newCkinLockTestRepo(t).DB()
	if got := lockTimeout(d); got != DefaultCkinLockTimeout {
		t.Fatalf("unset: %v", got)
	}
	for v, want := range map[string]time.Duration{
		"300": 300 * time.Second, " 30": 30 * time.Second, "0": 0,
		"-5": -5 * time.Second, "soon": 0, "45s": 45 * time.Second,
	} {
		d.Exec("REPLACE INTO config(name, value, mtime) VALUES('lock-timeout', ?, 1)", v)
		if got := lockTimeout(d); got != want {
			t.Errorf("lock-timeout %q: %v, want %v", v, got, want)
		}
	}
}

// TestCkinLock_ClientDefossilizesHolder checks that the client reads
// ci-lock-fail's LOGIN as fossilized text, as fossil's client does with
// defossilize (§9.4), so a login with a space is reported whole.
func TestCkinLock_ClientDefossilizesHolder(t *testing.T) {
	transport := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			return &xfer.Message{Cards: []xfer.Card{&xfer.PragmaCard{
				Name: "ci-lock-fail", Values: []string{`bob\ssmith`, "1700000000"},
			}}}
		},
	}
	clientRepo := newCkinLockTestRepo(t)
	result, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true,
		CkinLock: &CkinLockReq{
			ParentUUID: "abcd0000000000000000000000000000000000ab",
			ClientID:   "client-1",
		},
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.CkinLockFail == nil || result.CkinLockFail.HeldBy != "bob smith" {
		t.Fatalf("CkinLockFail = %+v, want holder %q", result.CkinLockFail, "bob smith")
	}
}
