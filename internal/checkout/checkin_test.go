package checkout

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	libdb "github.com/danmestas/libfossil/db"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/simio"
)

func TestCommitFullCycle(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid1, uuid1, _ := co.Version()

	// Extract to MemStorage
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Modify a file
	if err := mem.WriteFile("/checkout/hello.txt", []byte("modified content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Commit
	rid2, uuid2, err := co.Commit(CommitOpts{
		Message: "edit hello.txt",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// New version should be different
	if rid2 == rid1 {
		t.Fatal("new RID should differ from old")
	}
	if uuid2 == uuid1 {
		t.Fatal("new UUID should differ from old")
	}
	if rid2 <= 0 {
		t.Fatal("new RID should be positive")
	}

	// Checkout version should be updated
	currentRID, currentUUID, _ := co.Version()
	if currentRID != rid2 || currentUUID != uuid2 {
		t.Fatal("checkout version not updated after commit")
	}

	// Verify the new checkin has correct parent
	var parentRID int64
	err = r.DB().QueryRow("SELECT pid FROM plink WHERE cid=? AND isprim=1", int64(rid2)).Scan(&parentRID)
	if err != nil {
		t.Fatalf("query parent: %v", err)
	}
	if parentRID != int64(rid1) {
		t.Fatalf("parent = %d, want %d", parentRID, rid1)
	}
}
func TestCommitFromUnbornCheckout(t *testing.T) {
	r, cleanup := newTestEmptyRepo(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, uuid, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if rid != 0 || uuid != "" {
		t.Fatalf("unborn version = (%d, %q), want (0, empty)", rid, uuid)
	}
	if err := co.ValidateFingerprint(); err != nil {
		t.Fatalf("unborn fingerprint: %v", err)
	}

	mem := simio.NewMemStorage()
	if err := mem.MkdirAll("/checkout", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/checkout/new.txt", []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	counts, err := co.Manage(ManageOpts{Paths: []string{"new.txt"}})
	if err != nil {
		t.Fatalf("Manage: %v", err)
	}
	if counts.Added != 1 {
		t.Fatalf("Manage added = %d, want 1", counts.Added)
	}
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatalf("ScanChanges: %v", err)
	}
	newRID, newUUID, err := co.Commit(CommitOpts{Message: "initial", User: "test"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if newRID <= 0 || newUUID == "" {
		t.Fatalf("commit version = (%d, %q), want positive/non-empty", newRID, newUUID)
	}
	currentRID, currentUUID, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if currentRID != newRID || currentUUID != newUUID {
		t.Fatalf("checkout version = (%d,%q), want (%d,%q)", currentRID, currentUUID, newRID, newUUID)
	}
	var plinkParents int
	if err := r.DB().QueryRow("SELECT count(*) FROM plink WHERE cid=?", int64(newRID)).Scan(&plinkParents); err != nil {
		t.Fatal(err)
	}
	if plinkParents != 0 {
		t.Fatalf("initial checkin has %d plink parent rows, want 0", plinkParents)
	}
	var vfileVID int64
	if err := co.db.QueryRow("SELECT DISTINCT vid FROM vfile").Scan(&vfileVID); err != nil {
		t.Fatal(err)
	}
	if vfileVID != int64(newRID) {
		t.Fatalf("vfile vid = %d, want %d", vfileVID, newRID)
	}
}

func TestLegacyBooleanVFileFlagsCommitCompat(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	var parentRID int64
	var parentUUID string
	if err := r.DB().QueryRow(`
		SELECT l.rid, b.uuid FROM leaf l
		JOIN blob b ON b.rid = l.rid
		LIMIT 1
	`).Scan(&parentRID, &parentUUID); err != nil {
		t.Fatal(err)
	}
	files, err := manifest.ListFiles(r, libfossil.FslID(parentRID))
	if err != nil {
		t.Fatal(err)
	}
	var helloUUID string
	for _, f := range files {
		if f.Name == "hello.txt" {
			helloUUID = f.UUID
			break
		}
	}
	if helloUUID == "" {
		t.Fatal("hello.txt not found in parent manifest")
	}
	var helloRID int64
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", helloUUID).Scan(&helloRID); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), ".fslckout")
	ckdb, err := libdb.OpenSQL(dbPath, libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ckdb.Close()
	stmts := []string{
		`CREATE TABLE vvar(name TEXT PRIMARY KEY, value CLOB) WITHOUT ROWID`,
		`CREATE TABLE vfile(id INTEGER PRIMARY KEY, vid INTEGER, chnged INT DEFAULT 0,
			deleted BOOLEAN DEFAULT 0, isexe BOOLEAN, islink BOOLEAN, rid INTEGER,
			mrid INTEGER, mtime INTEGER, pathname TEXT, origname TEXT, mhash TEXT,
			UNIQUE(pathname, vid))`,
		`CREATE TABLE vmerge(id INTEGER, merge INTEGER, mhash TEXT)`,
		`INSERT INTO vvar(name, value) VALUES('checkout', ?), ('checkout-hash', ?)`,
		`INSERT INTO vfile(vid, pathname, rid, mrid, chnged, deleted, isexe, islink, mhash)
			VALUES(?, 'hello.txt', ?, ?, TRUE, FALSE, TRUE, FALSE, ?)`,
	}
	for i, stmt := range stmts {
		var execErr error
		switch i {
		case 3:
			_, execErr = ckdb.Exec(stmt, parentRID, parentUUID)
		case 4:
			_, execErr = ckdb.Exec(stmt, parentRID, helloRID, helloRID, helloUUID)
		default:
			_, execErr = ckdb.Exec(stmt)
		}
		if execErr != nil {
			t.Fatalf("stmt %d: %v", i, execErr)
		}
	}

	mem := simio.NewMemStorage()
	if err := mem.MkdirAll("/checkout", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/checkout/hello.txt", []byte("legacy boolean change\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	co := &Checkout{
		db:   ckdb,
		repo: r,
		env:  &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}},
		obs:  nopObserver{},
		dir:  "/checkout",
	}

	var visited ChangeEntry
	if err := co.VisitChanges(libfossil.FslID(parentRID), false, func(e ChangeEntry) error {
		visited = e
		return nil
	}); err != nil {
		t.Fatalf("VisitChanges: %v", err)
	}
	if visited.Name != "hello.txt" || visited.Change != ChangeModified || !visited.IsExec {
		t.Fatalf("VisitChanges entry = %+v, want modified executable hello.txt", visited)
	}

	newRID, _, err := co.Commit(CommitOpts{Message: "legacy boolean", User: "test"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var committedUUID string
	if err := r.DB().QueryRow(`
		SELECT b.uuid FROM mlink m
		JOIN filename f ON f.fnid = m.fnid
		JOIN blob b ON b.rid = m.fid
		WHERE m.mid = ? AND f.name = 'hello.txt'
	`, int64(newRID)).Scan(&committedUUID); err != nil {
		t.Fatal(err)
	}
	if committedUUID == helloUUID {
		t.Fatal("commit kept old hello.txt UUID; BOOLEAN chnged flag was not honored")
	}
}

func TestCommitWithEnqueue(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid1, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Modify two files
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/checkout/README.md", []byte("changed2"), 0644); err != nil {
		t.Fatal(err)
	}

	// Only enqueue one
	if err := co.Enqueue(EnqueueOpts{Paths: []string{"hello.txt"}}); err != nil {
		t.Fatal(err)
	}

	// Commit should only include hello.txt changes (but all files in manifest)
	rid2, _, err := co.Commit(CommitOpts{Message: "partial", User: "test"})
	if err != nil {
		t.Fatal(err)
	}

	// Verify that README.md still has the old content in the new checkin
	// (because it wasn't enqueued)
	// We'll check by reading the new manifest and verifying README.md content
	// matches the original
	var readmeUUID string
	err = r.DB().QueryRow(`
		SELECT b.uuid FROM mlink m
		JOIN filename f ON f.fnid = m.fnid
		JOIN blob b ON b.rid = m.fid
		WHERE m.mid = ? AND f.name = 'README.md'
	`, rid2).Scan(&readmeUUID)
	if err != nil {
		t.Fatalf("query README.md uuid in new checkin: %v", err)
	}

	// The original README.md hash
	var originalReadmeUUID string
	err = r.DB().QueryRow(`
		SELECT b.uuid FROM mlink m
		JOIN filename f ON f.fnid = m.fnid
		JOIN blob b ON b.rid = m.fid
		WHERE m.mid = ? AND f.name = 'README.md'
	`, rid1).Scan(&originalReadmeUUID)
	if err != nil {
		t.Fatalf("query original README.md uuid: %v", err)
	}

	if readmeUUID != originalReadmeUUID {
		t.Fatalf("README.md should be unchanged in commit (enqueue filtered it out), got %s, want %s", readmeUUID, originalReadmeUUID)
	}
}

func TestCommitNoChanges(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{
		Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{},
	}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}

	// Commit with no modifications — manifest.Checkin panics with
	// empty Files list, so buildCommitFiles returns the full
	// unchanged set. The commit succeeds but produces an identical
	// manifest. This documents current behavior.
	_, _, err = co.Commit(CommitOpts{
		Message: "empty commit", User: "test",
	})
	// The commit should succeed (all files are still present,
	// just unchanged). If the project later decides to reject
	// no-change commits, this test should be updated.
	if err != nil {
		t.Fatalf("commit with no changes failed: %v", err)
	}
}

func TestCommitWithDelete(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid1, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{
		Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{},
	}
	co.dir = "/checkout"
	if err := co.Extract(rid1, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}

	// Unmanage hello.txt (marks deleted=1)
	if err := co.Unmanage(UnmanageOpts{
		Paths: []string{"hello.txt"},
	}); err != nil {
		t.Fatal(err)
	}

	// Commit — hello.txt should not appear in the new manifest.
	rid2, _, err := co.Commit(CommitOpts{
		Message: "delete hello.txt", User: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify hello.txt is absent from the new checkin's mlink entries.
	var count int
	err = r.DB().QueryRow(`
		SELECT count(*) FROM mlink m
		JOIN filename f ON f.fnid = m.fnid
		WHERE m.mid = ? AND f.name = 'hello.txt'
	`, rid2).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf(
			"hello.txt should be absent from new checkin, got %d mlink rows",
			count,
		)
	}
}

func TestDequeueAll(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if err := co.Enqueue(EnqueueOpts{Paths: []string{"a", "b", "c"}}); err != nil {
		t.Fatal(err)
	}

	enqueued, _ := co.IsEnqueued("a")
	if !enqueued {
		t.Fatal("a should be enqueued")
	}

	// Dequeue all (empty paths = set queue to nil)
	if err := co.Dequeue(DequeueOpts{}); err != nil {
		t.Fatal(err)
	}

	// After dequeue all (nil queue), IsEnqueued returns true (implicit all)
	enqueued, _ = co.IsEnqueued("a")
	if !enqueued {
		t.Fatal("nil queue should mean all implicitly enqueued")
	}
}

func TestEnqueueDequeue(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Initially, empty queue means all implicitly enqueued
	enqueued, _ := co.IsEnqueued("hello.txt")
	if !enqueued {
		t.Fatal("empty queue should return true for any file")
	}

	// Enqueue one file
	if err := co.Enqueue(EnqueueOpts{Paths: []string{"hello.txt"}}); err != nil {
		t.Fatal(err)
	}

	// Now only hello.txt is enqueued
	enqueued, _ = co.IsEnqueued("hello.txt")
	if !enqueued {
		t.Fatal("hello.txt should be enqueued")
	}

	enqueued, _ = co.IsEnqueued("README.md")
	if enqueued {
		t.Fatal("README.md should NOT be enqueued")
	}

	// Dequeue hello.txt
	if err := co.Dequeue(DequeueOpts{Paths: []string{"hello.txt"}}); err != nil {
		t.Fatal(err)
	}

	enqueued, _ = co.IsEnqueued("hello.txt")
	if enqueued {
		t.Fatal("hello.txt should NOT be enqueued after dequeue")
	}
}

func TestDiscardQueue(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Enqueue some files
	if err := co.Enqueue(EnqueueOpts{Paths: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}

	enqueued, _ := co.IsEnqueued("c")
	if enqueued {
		t.Fatal("c should not be enqueued")
	}

	// Discard queue
	if err := co.DiscardQueue(); err != nil {
		t.Fatal(err)
	}

	// Now all implicitly enqueued
	enqueued, _ = co.IsEnqueued("c")
	if !enqueued {
		t.Fatal("after DiscardQueue, all files should be implicitly enqueued")
	}
}

func TestPreCommitCheck_Abort(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer co.Close()

	rid1, _, _ := co.Version()
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	co.env.Storage.WriteFile(co.dir+"/abort.txt", []byte("abort"), 0644)

	wantErr := fmt.Errorf("blocked by policy")
	_, _, err = co.Commit(CommitOpts{
		Message:        "should not commit",
		User:           "test",
		PreCommitCheck: func() error { return wantErr },
	})
	if err == nil {
		t.Fatal("Commit succeeded, want error from PreCommitCheck")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
}

func TestPreCommitCheck_Nil(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer co.Close()

	rid1, _, _ := co.Version()
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	co.env.Storage.WriteFile(co.dir+"/ok.txt", []byte("ok"), 0644)

	_, _, err = co.Commit(CommitOpts{
		Message: "normal commit",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("Commit with nil PreCommitCheck failed: %v", err)
	}
}
