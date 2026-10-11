package sync

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The handler sends repository content only to a requester entitled to read
// it, as fossil's page_xfer does: gimme is answered only once a pull (or a
// clone) was accepted, and the unversioned catalog and files only to a
// requester with Read. These tests set nobody's capabilities directly, so
// every request is anonymous.

func pullCard() *xfer.PullCard { return &xfer.PullCard{ServerCode: "s", ProjectCode: testPC} }

func sentFile(resp *xfer.Message, uuid string) bool {
	for _, f := range findCards[*xfer.FileCard](resp) {
		if f.UUID == uuid {
			return true
		}
	}
	return false
}

func writeUV(t *testing.T, r *repo.Repo, name, data string, mtime int64) {
	t.Helper()
	if err := uv.EnsureSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := uv.Write(r.DB(), name, []byte(data), mtime); err != nil {
		t.Fatalf("uv.Write: %v", err)
	}
}

func hasPragma(resp *xfer.Message, name string) bool {
	for _, p := range findCards[*xfer.PragmaCard](resp) {
		if p.Name == name {
			return true
		}
	}
	return false
}

func TestGimmeWithoutPullSendsNothing(t *testing.T) {
	r := setupSyncTestRepo(t)
	uuid := storeTestBlob(t, r, []byte("secret artifact"))
	for _, caps := range []string{"", "o", "y"} {
		setNobodyCaps(t, r, caps)
		resp := handleReq(t, r, &xfer.GimmeCard{UUID: uuid})
		if sentFile(resp, uuid) {
			t.Errorf("caps %q: gimme without a pull card sent the artifact", caps)
		}
	}
}

func TestGimmeWithoutReadSendsNothing(t *testing.T) {
	r := setupSyncTestRepo(t)
	uuid := storeTestBlob(t, r, []byte("secret artifact"))
	setNobodyCaps(t, r, "")
	resp := handleReq(t, r, pullCard(), &xfer.GimmeCard{UUID: uuid})
	if sentFile(resp, uuid) {
		t.Fatal("gimme from a requester without Read sent the artifact")
	}
}

func TestGimmeInCloneSessionIsServed(t *testing.T) {
	r := setupSyncTestRepo(t)
	uuid := storeTestBlob(t, r, []byte("cloned artifact"))
	setNobodyCaps(t, r, "g")
	resp := handleReq(t, r, &xfer.CloneCard{}, &xfer.GimmeCard{UUID: uuid})
	if !sentFile(resp, uuid) {
		t.Fatal("a clone session counts as pulling; gimme should be answered")
	}
}

// TestUnversionedReadInCloneSession: fossil's page_xfer() promotes Clone to
// Read for the whole session, so a requester allowed only to clone reads the
// unversioned catalog and its files (issue #312, requirement SP-7.3-12).
func TestUnversionedReadInCloneSession(t *testing.T) {
	r := setupSyncTestRepo(t)
	writeUV(t, r, "clone.txt", "unversioned clone", 100)
	setNobodyCaps(t, r, "g")
	resp := handleReq(t, r,
		&xfer.CloneCard{},
		&xfer.PragmaCard{Name: "uv-hash", Values: []string{"wrong"}},
		versionPragma("client-version"),
		&xfer.UVGimmeCard{Name: "clone.txt"},
	)
	if n := len(findCards[*xfer.UVIGotCard](resp)); n != 1 {
		t.Errorf("clone session got %d uvigot cards, want 1", n)
	}
	if !hasPragma(resp, "uv-pull-only") && !hasPragma(resp, "uv-push-ok") {
		t.Error("clone session got no uv catalog pragma")
	}
	if n := len(findCards[*xfer.UVFileCard](resp)); n != 1 {
		t.Errorf("clone session got %d uvfile cards, want 1", n)
	}
}

func TestUVHashWithoutReadSendsNoCatalog(t *testing.T) {
	r := setupSyncTestRepo(t)
	writeUV(t, r, "secret.txt", "unversioned secret", 100)
	setNobodyCaps(t, r, "")
	resp := handleReq(t, r, &xfer.PragmaCard{Name: "uv-hash", Values: []string{"wrong"}})
	if n := len(findCards[*xfer.UVIGotCard](resp)); n != 0 {
		t.Errorf("uv-hash without Read sent %d uvigot cards", n)
	}
	if hasPragma(resp, "uv-push-ok") || hasPragma(resp, "uv-pull-only") {
		t.Error("uv-hash without Read answered with a uv pragma")
	}
}

func TestUVGimmeWithoutReadSendsNothing(t *testing.T) {
	r := setupSyncTestRepo(t)
	writeUV(t, r, "secret.txt", "unversioned secret", 100)
	setNobodyCaps(t, r, "")
	resp := handleReq(t, r, &xfer.UVGimmeCard{Name: "secret.txt"})
	if n := len(findCards[*xfer.UVFileCard](resp)); n != 0 {
		t.Fatalf("uvgimme without Read sent %d uvfile cards", n)
	}
}

func TestUVIGotWithoutReadSendsNothing(t *testing.T) {
	r := setupSyncTestRepo(t)
	writeUV(t, r, "secret.txt", "unversioned secret", 500)
	setNobodyCaps(t, r, "")
	// An older client copy makes the server's copy newer: it would send it.
	resp := handleReq(t, r, &xfer.UVIGotCard{Name: "secret.txt", MTime: 100, Hash: "-", Size: 0})
	if n := len(findCards[*xfer.UVFileCard](resp)); n != 0 {
		t.Fatalf("uvigot without Read sent %d uvfile cards", n)
	}
}

func TestUVIGotMtimeOnlyNeedsUVWrite(t *testing.T) {
	r := setupSyncTestRepo(t)
	writeUV(t, r, "f.txt", "same content", 100)
	_, _, h, err := uv.Read(r.DB(), "f.txt")
	if err != nil {
		t.Fatalf("uv.Read: %v", err)
	}
	setNobodyCaps(t, r, "o")
	// Same hash, newer client mtime: status 2 rewrites the server's mtime.
	handleReq(t, r, &xfer.UVIGotCard{Name: "f.txt", MTime: 900, Hash: h, Size: 12})
	_, mtime, _, err := uv.Read(r.DB(), "f.txt")
	if err != nil {
		t.Fatalf("uv.Read: %v", err)
	}
	if mtime != 100 {
		t.Fatalf("uvigot without y rewrote mtime to %d", mtime)
	}
}

func TestUVPushOKOnlyWithUVWrite(t *testing.T) {
	r := setupSyncTestRepo(t)
	writeUV(t, r, "a.txt", "aaa", 100)
	uvHash := &xfer.PragmaCard{Name: "uv-hash", Values: []string{"wrong"}}

	setNobodyCaps(t, r, "o")
	resp := handleReq(t, r, uvHash)
	if hasPragma(resp, "uv-push-ok") {
		t.Error("uv-push-ok sent to a requester without y")
	}
	if !hasPragma(resp, "uv-pull-only") {
		t.Error("expected uv-pull-only for a reader without y")
	}
	if n := len(findCards[*xfer.UVIGotCard](resp)); n != 1 {
		t.Errorf("reader got %d uvigot cards, want 1", n)
	}

	setNobodyCaps(t, r, "oy")
	resp = handleReq(t, r, uvHash)
	if !hasPragma(resp, "uv-push-ok") {
		t.Error("expected uv-push-ok for a requester with y")
	}
}

func setupKVTable(t *testing.T, r *repo.Repo) string {
	t.Helper()
	def := repo.TableDef{
		Columns:  []repo.ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "val", Type: "text"}},
		Conflict: "mtime-wins",
	}
	if err := repo.EnsureSyncSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSyncSchema: %v", err)
	}
	if err := repo.RegisterSyncedTable(r.DB(), "kv", def, 100); err != nil {
		t.Fatalf("RegisterSyncedTable: %v", err)
	}
	row := map[string]any{"id": "k", "val": "secret row"}
	if err := repo.UpsertXRow(r.DB(), "kv", row, 200); err != nil {
		t.Fatalf("UpsertXRow: %v", err)
	}
	return repo.PKHash([]repo.ColumnDef{{Name: "id", Type: "text", PK: true}}, row)
}

func TestXGimmeWithoutPullSendsNothing(t *testing.T) {
	r := setupSyncTestRepo(t)
	pk := setupKVTable(t, r)
	setNobodyCaps(t, r, "")
	resp := handleReq(t, r, &xfer.XGimmeCard{Table: "kv", PKHash: pk})
	if n := len(findCards[*xfer.XRowCard](resp)); n != 0 {
		t.Fatalf("xgimme without a pull card sent %d rows", n)
	}
}

func TestXTableHashWithoutReadSendsNothing(t *testing.T) {
	r := setupSyncTestRepo(t)
	setupKVTable(t, r)
	setNobodyCaps(t, r, "")
	resp := handleReq(t, r, &xfer.PragmaCard{Name: "xtable-hash", Values: []string{"kv", "wrong"}})
	if n := len(findCards[*xfer.XIGotCard](resp)); n != 0 {
		t.Fatalf("xtable-hash without Read sent %d xigot cards", n)
	}
}

// TestXTableHashInCloneSessionSendsRows pins the same Clone-implies-Read rule
// for table sync, whose catalog fossil also gates on Read (issue #312).
func TestXTableHashInCloneSessionSendsRows(t *testing.T) {
	r := setupSyncTestRepo(t)
	setupKVTable(t, r)
	setNobodyCaps(t, r, "g")
	resp := handleReq(t, r,
		&xfer.CloneCard{},
		&xfer.PragmaCard{Name: "xtable-hash", Values: []string{"kv", "wrong"}},
	)
	if n := len(findCards[*xfer.XIGotCard](resp)); n != 1 {
		t.Fatalf("clone session got %d xigot cards, want 1", n)
	}
}

func TestCkinLockNeedsWrite(t *testing.T) {
	r := setupSyncTestRepo(t)
	uuid := "lock000000000000000000000000000000000000"
	insertLeafBlob(t, r, uuid)
	setNobodyCaps(t, r, "o")
	handleReq(t, r, &xfer.PragmaCard{Name: "ci-lock", Values: []string{uuid, "client-A"}})
	var n int
	err := r.DB().QueryRow(
		"SELECT count(*) FROM config WHERE name GLOB 'libfossil-ci-lock-*'").Scan(&n)
	if err != nil {
		t.Fatalf("count locks: %v", err)
	}
	if n != 0 {
		t.Fatal("ci-lock from a requester without Write took a lock")
	}
}

// TestReaderGetsEverything is the positive control: a requester with Read
// who pulls gets artifacts, the unversioned catalog and unversioned files.
func TestReaderGetsEverything(t *testing.T) {
	r := setupSyncTestRepo(t)
	uuid := storeTestBlob(t, r, []byte("public artifact"))
	writeUV(t, r, "pub.txt", "unversioned public", 100)
	setNobodyCaps(t, r, "o")
	// The unversioned file is named by SHA3, which a client that names no
	// release would be refused (§7.6).
	resp := handleReq(t, r,
		versionPragma("client-version"),
		pullCard(),
		&xfer.PragmaCard{Name: "uv-hash", Values: []string{"wrong"}},
		&xfer.GimmeCard{UUID: uuid},
		&xfer.UVGimmeCard{Name: "pub.txt"},
	)
	if !sentFile(resp, uuid) {
		t.Error("reader did not get the artifact")
	}
	if n := len(findCards[*xfer.UVIGotCard](resp)); n != 1 {
		t.Errorf("reader got %d uvigot cards, want 1", n)
	}
	if n := len(findCards[*xfer.UVFileCard](resp)); n != 1 {
		t.Errorf("reader got %d uvfile cards, want 1", n)
	}
}
