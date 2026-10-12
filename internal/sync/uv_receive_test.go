package sync

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// nullRowHash names the content of an unversioned row stored without it.
const nullRowHash = "2aae6c35c94fcfb415dbe95f408b9ce91ee846ed"

// TestUVNullContentRelays pins that a row stored with NULL content, which
// fossil sends as "uvfile NAME MTIME HASH 0 0" with no payload, crosses from
// one go-libfossil to another: a card carrying no content has no HASH to
// check it against (§7.3).
func TestUVNullContentRelays(t *testing.T) {
	server, client := newPrivateTestPair(t, "oy")
	if err := uv.EnsureSchema(server.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := uv.Receive(server.DB(), "w.txt", 100, nullRowHash, nil); err != nil {
		t.Fatalf("uv.Receive: %v", err)
	}
	res := syncViaHandler(t, server, client, SyncOpts{Pull: true, UV: true})
	if len(res.Errors) != 0 {
		t.Fatalf("sync errors: %v", res.Errors)
	}
	var sz int
	var isNull bool
	err := client.DB().QueryRow(
		"SELECT sz, content IS NULL FROM unversioned WHERE name='w.txt' AND hash=?",
		nullRowHash).Scan(&sz, &isNull)
	if err != nil || sz != 0 || !isNull {
		t.Fatalf("client w.txt: sz %d, NULL %v, %v; want the NULL row relayed",
			sz, isNull, err)
	}
}

// TestUVDeletionCardApplied pins that a deletion card with no flags, HASH
// "-" and size 0, is applied by the client: it carries no content, so there
// is no HASH to check.
func TestUVDeletionCardApplied(t *testing.T) {
	s, r := newTestSession(t,
		SyncOpts{Pull: true, UV: true, ServerCode: "sc", ProjectCode: testPC})
	if err := uv.Write(r.DB(), "gone.txt", []byte("to delete"), 100); err != nil {
		t.Fatalf("uv.Write: %v", err)
	}
	card := &xfer.UVFileCard{Name: "gone.txt", MTime: 200, Hash: "-"}
	if err := s.handleUVFileCard(card); err != nil {
		t.Fatalf("handleUVFileCard: %v", err)
	}
	_, mtime, fileHash, err := uv.Read(r.DB(), "gone.txt")
	if err != nil || mtime != 200 || fileHash != "" {
		t.Fatalf("gone.txt: mtime %d, hash %q, %v; want a tombstone at 200",
			mtime, fileHash, err)
	}
}

// TestHandlerUVFileGarbageHashRefused pins that a uvfile whose HASH is not
// an artifact name is a malformed line: refused, and nothing stored.
func TestHandlerUVFileGarbageHashRefused(t *testing.T) {
	r := setupSyncTestRepo(t)
	if err := uv.EnsureSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	setNobodyCaps(t, r, "oy")
	req, err := xfer.DecodeUncompressed([]byte("uvfile n 100 garbage 0 4\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if errs := findCards[*xfer.ErrorCard](resp); len(errs) != 1 {
		t.Fatalf("error cards %v, want the malformed line refused", errs)
	}
	var n int
	err = r.DB().QueryRow("SELECT count(*) FROM unversioned WHERE name='n'").Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("rows for n: %d, %v; want none", n, err)
	}
}
