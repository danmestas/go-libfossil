package sync

import (
	"bytes"
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
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

// A Clone-only requester has Read by derivation, but it asked for no pull:
// xtable-hash lists keys and mtimes that xgimme, pull-gated, would refuse to
// fetch. Both cards use the pull gate (#351).
func TestXTableHashNeedsPull(t *testing.T) {
	r := setupSyncTestRepo(t)
	setupKVTable(t, r)
	setNobodyCaps(t, r, "g")
	resp := handleReq(t, r,
		&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true},
		&xfer.PragmaCard{Name: "xtable-hash", Values: []string{"kv", "wrong"}})
	if n := len(findCards[*xfer.XIGotCard](resp)); n != 0 {
		t.Fatalf("xtable-hash without a pull card sent %d xigot cards", n)
	}

	// A requester whose pull card is accepted gets the inventory.
	setNobodyCaps(t, r, "go")
	resp = handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: testPC},
		&xfer.PragmaCard{Name: "xtable-hash", Values: []string{"kv", "wrong"}})
	if n := len(findCards[*xfer.XIGotCard](resp)); n == 0 {
		t.Fatal("xtable-hash with a pull card sent no xigot cards")
	}

	// A push-only client with Read gets it too: it queues newer rows from it.
	setNobodyCaps(t, r, "io")
	resp = handleReq(t, r,
		&xfer.PushCard{ServerCode: "s", ProjectCode: testPC},
		&xfer.PragmaCard{Name: "xtable-hash", Values: []string{"kv", "wrong"}})
	if n := len(findCards[*xfer.XIGotCard](resp)); n == 0 {
		t.Fatal("xtable-hash with a push card sent no xigot cards")
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
		"SELECT count(*) FROM config WHERE name GLOB 'ci-lock-*'").Scan(&n)
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

// TestCloneImpliesRead pins that Clone counts as Read at every read gate, as
// fossil's page_xfer grants it: a requester with only g gets the unversioned
// catalog, a uvgimme'd file and read-gated
// configuration. Pull still needs o, so its pull card is refused.
func TestCloneImpliesRead(t *testing.T) {
	r := setupSyncTestRepo(t)
	writeUV(t, r, "pub.txt", "unversioned public", 100)
	setupKVTable(t, r)
	if _, err := r.DB().Exec(
		"REPLACE INTO config(name, value, mtime) VALUES('project-name', 'p', 0)",
	); err != nil {
		t.Fatalf("set project-name: %v", err)
	}
	setNobodyCaps(t, r, "g")

	resp := handleReq(t, r,
		versionPragma("client-version"),
		&xfer.PragmaCard{Name: "uv-hash", Values: []string{"wrong"}},
		&xfer.UVGimmeCard{Name: "pub.txt"},
		&xfer.PragmaCard{Name: "xtable-hash", Values: []string{"kv", "wrong"}},
		&xfer.ReqConfigCard{Name: "/project"},
	)
	if n := len(findCards[*xfer.UVIGotCard](resp)); n != 1 {
		t.Errorf("clone-only requester got %d uvigot cards, want 1", n)
	}
	if n := len(findCards[*xfer.UVFileCard](resp)); n != 1 {
		t.Errorf("clone-only requester got %d uvfile cards, want 1", n)
	}
	// xtable-hash takes xgimme's pull gate, which Clone alone does not
	// meet (#351), so no inventory of rows the requester cannot fetch.
	if n := len(findCards[*xfer.XIGotCard](resp)); n != 0 {
		t.Errorf("clone-only requester got %d xigot cards, want 0", n)
	}
	if n := len(findCards[*xfer.ConfigCard](resp)); n == 0 {
		t.Error("clone-only requester got no configuration")
	}

	pull := handleReq(t, r, pullCard())
	if errs := findCards[*xfer.ErrorCard](pull); len(errs) != 1 {
		t.Errorf("clone-only pull: error cards %v, want the pull refused", errs)
	}
}

// TestLoginCloneDoesNotImplyRead pins that a Clone granted by a login card
// does not become Read, as fossil derives Read from Clone only for nobody's
// capabilities, ahead of the login.
func TestLoginCloneDoesNotImplyRead(t *testing.T) {
	r := setupSyncTestRepo(t)
	writeUV(t, r, "pub.txt", "unversioned public", 100)
	setNobodyCaps(t, r, "")
	if err := auth.CreateUser(r.DB(), testPC, "bob", "pw", "g"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rest := []xfer.Card{
		versionPragma("client-version"),
		&xfer.UVGimmeCard{Name: "pub.txt"},
		randomComment(simio.CryptoRand{}),
	}
	payload, err := encodeCards(rest)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	login := computeLogin("bob", "pw", testPC, payload)
	req, err := xfer.DecodeUncompressed(append(encodeLine(t, login), payload...))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if errs := findCards[*xfer.ErrorCard](resp); len(errs) != 0 {
		t.Fatalf("login refused: %v", errs)
	}
	if n := len(findCards[*xfer.UVFileCard](resp)); n != 0 {
		t.Fatalf("a login-granted Clone got %d uvfile cards, want 0", n)
	}
}

// encodeLine encodes one card as its wire line.
func encodeLine(t *testing.T, c xfer.Card) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := xfer.EncodeCard(&buf, c); err != nil {
		t.Fatalf("encode %T: %v", c, err)
	}
	return buf.Bytes()
}
