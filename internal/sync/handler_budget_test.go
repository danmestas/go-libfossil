package sync

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

const testBudget = 4096

// randomBytes is n incompressible bytes, so a blob holding them stores
// larger than n.
func randomBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// pushBudgeted sends cards in one push round to r under a store budget.
func pushBudgeted(t *testing.T, r *repo.Repo, limit int, cards ...xfer.Card) *xfer.Message {
	t.Helper()
	req := &xfer.Message{Cards: append([]xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: testPC}}, cards...)}
	resp, err := HandleSyncWithOpts(context.Background(), r, req,
		HandleOpts{MaxStoredBytes: limit})
	if err != nil {
		t.Fatalf("HandleSyncWithOpts: %v", err)
	}
	return resp
}

// onlyError returns the text of resp's error card, failing unless the reply
// is that one card: a refused round answers with nothing else.
func onlyError(t *testing.T, resp *xfer.Message) string {
	t.Helper()
	var msgs []string
	for _, c := range resp.Cards {
		switch c := c.(type) {
		case *xfer.ErrorCard:
			msgs = append(msgs, c.Message)
		case *xfer.CommentCard:
		default:
			t.Fatalf("refused round replied with a %T card: %+v", c, resp.Cards)
		}
	}
	if len(msgs) != 1 {
		t.Fatalf("want one error card, got %q", msgs)
	}
	return msgs[0]
}

func blobCount(t *testing.T, r *repo.Repo) int {
	t.Helper()
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func storedLen(t *testing.T, data []byte) int {
	t.Helper()
	c, err := blob.Compress(data)
	if err != nil {
		t.Fatal(err)
	}
	return len(c)
}

func budgetText(uuid string, n int) string {
	return fmt.Sprintf("artifact %s is %d bytes stored; this repository stores at most %d bytes per artifact",
		uuid, n, testBudget)
}

func requireAbsent(t *testing.T, r *repo.Repo, uuids ...string) {
	t.Helper()
	for _, u := range uuids {
		if _, ok := blob.Exists(r.DB(), u); ok && !isPhantom(t, r, u) {
			t.Fatalf("%s was stored by a refused round", u)
		}
	}
}

func TestBudgetRefusesRoundWithOversizeFile(t *testing.T) {
	r := setupPushableTestRepo(t)
	small := []byte("small file")
	big := randomBytes(1, 2*testBudget)
	smallUUID, bigUUID := hash.SHA1(small), hash.SHA1(big)
	before := blobCount(t, r)

	resp := pushBudgeted(t, r, testBudget,
		&xfer.FileCard{UUID: smallUUID, Content: small},
		&xfer.FileCard{UUID: bigUUID, Content: big})

	if got, want := onlyError(t, resp), budgetText(bigUUID, storedLen(t, big)); got != want {
		t.Fatalf("error card:\n got %q\nwant %q", got, want)
	}
	requireAbsent(t, r, smallUUID, bigUUID)
	if after := blobCount(t, r); after != before {
		t.Fatalf("blob rows %d -> %d across a refused round", before, after)
	}
}

// A file whose length fits but whose zlib form does not is measured by what
// it stores, not by its length plus the size prefix.
func TestBudgetMeasuresIncompressibleExpansion(t *testing.T) {
	r := setupPushableTestRepo(t)
	big := randomBytes(2, testBudget-4)
	uuid := hash.SHA1(big)
	n := storedLen(t, big)
	if n <= testBudget {
		t.Fatalf("fixture: %d random bytes store as %d, not over %d", len(big), n, testBudget)
	}
	resp := pushBudgeted(t, r, testBudget, &xfer.FileCard{UUID: uuid, Content: big})
	if got, want := onlyError(t, resp), budgetText(uuid, n); got != want {
		t.Fatalf("error card:\n got %q\nwant %q", got, want)
	}
	requireAbsent(t, r, uuid)
}

// A long file that compresses under the budget is stored.
func TestBudgetStoresCompressibleLongFile(t *testing.T) {
	r := setupPushableTestRepo(t)
	long := bytes.Repeat([]byte("line of text\n"), 4*testBudget)
	uuid := hash.SHA1(long)
	resp := pushBudgeted(t, r, testBudget, &xfer.FileCard{UUID: uuid, Content: long})
	requireNoError(t, resp)
	if _, ok := blob.Exists(r.DB(), uuid); !ok {
		t.Fatal("a file storing under the budget was not stored")
	}
}

func requireNoError(t *testing.T, resp *xfer.Message) {
	t.Helper()
	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			t.Fatalf("unexpected error card: %q", e.Message)
		}
	}
}

// A check-in already received names the phantom it lists, so the refusal
// says which file of which check-in is too big.
func TestBudgetNamesPathAndCheckin(t *testing.T) {
	r := setupPushableTestRepo(t)
	big := randomBytes(3, 2*testBudget)
	bigUUID := hash.SHA1(big)
	mBytes, mUUID := buildCheckinManifest(t, "adds a big file",
		time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), bigUUID, big, nil)
	requireNoError(t, pushBudgeted(t, r, testBudget,
		&xfer.FileCard{UUID: mUUID, Content: mBytes}))

	resp := pushBudgeted(t, r, testBudget, &xfer.FileCard{UUID: bigUUID, Content: big})

	want := budgetText(bigUUID, storedLen(t, big)) +
		", named hello.txt by check-in " + mUUID
	if got := onlyError(t, resp); got != want {
		t.Fatalf("error card:\n got %q\nwant %q", got, want)
	}
	requireAbsent(t, r, bigUUID)
	if _, ok := blob.Exists(r.DB(), mUUID); !ok {
		t.Fatal("the check-in an earlier round stored is gone")
	}
}

// A cfile card is measured by the stored form it carries.
func TestBudgetRefusesOversizeCFile(t *testing.T) {
	r := setupPushableTestRepo(t)
	big := randomBytes(4, 2*testBudget)
	uuid := hash.SHA1(big)
	stored, err := blob.Compress(big)
	if err != nil {
		t.Fatal(err)
	}
	resp := pushBudgeted(t, r, testBudget, &xfer.CFileCard{
		UUID: uuid, USize: len(big), Content: big, StoredBlob: stored})
	if got, want := onlyError(t, resp), budgetText(uuid, len(stored)); got != want {
		t.Fatalf("error card:\n got %q\nwant %q", got, want)
	}
	requireAbsent(t, r, uuid)
}

// A delta is measured by the delta it stores, not the artifact it expands to.
func TestBudgetMeasuresDeltaAsStored(t *testing.T) {
	r := setupPushableTestRepo(t)
	base := []byte("base text\n")
	baseUUID := hash.SHA1(base)
	requireNoError(t, pushBudgeted(t, r, testBudget,
		&xfer.FileCard{UUID: baseUUID, Content: base}))

	big := append(append([]byte{}, base...), randomBytes(5, 2*testBudget)...)
	d, err := delta.Create(base, big)
	if err != nil {
		t.Fatal(err)
	}
	uuid := hash.SHA1(big)
	resp := pushBudgeted(t, r, testBudget,
		&xfer.FileCard{UUID: uuid, DeltaSrc: baseUUID, Content: d})
	if got, want := onlyError(t, resp), budgetText(uuid, storedLen(t, d)); got != want {
		t.Fatalf("error card:\n got %q\nwant %q", got, want)
	}
	requireAbsent(t, r, uuid)

	small := append(append([]byte{}, base...), "one more line\n"...)
	sd, err := delta.Create(base, small)
	if err != nil {
		t.Fatal(err)
	}
	smallUUID := hash.SHA1(small)
	requireNoError(t, pushBudgeted(t, r, testBudget,
		&xfer.FileCard{UUID: smallUUID, DeltaSrc: baseUUID, Content: sd}))
	if _, ok := blob.Exists(r.DB(), smallUUID); !ok {
		t.Fatal("a small delta was not stored")
	}
}

func TestBudgetZeroStoresAnySize(t *testing.T) {
	r := setupPushableTestRepo(t)
	big := randomBytes(6, 2*testBudget)
	uuid := hash.SHA1(big)
	requireNoError(t, pushBudgeted(t, r, 0, &xfer.FileCard{UUID: uuid, Content: big}))
	if _, ok := blob.Exists(r.DB(), uuid); !ok {
		t.Fatal("with no budget a big file was not stored")
	}
}

// The budget limits what the store receives; what it already holds still
// clones and pulls in full.
func TestBudgetLeavesCloneAndPullAlone(t *testing.T) {
	src := setupPushableTestRepo(t)
	big := randomBytes(7, 2*testBudget)
	if _, _, err := manifest.Checkin(src, manifest.CheckinOpts{
		Comment: "big", User: "testuser",
		Files: []manifest.File{{Name: "big.bin", Content: big}},
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	// A store a test creates names new artifacts with SHA3-256.
	bigUUID := hash.SHA3(big)
	handle := func(ctx context.Context, r *repo.Repo, req *xfer.Message) (*xfer.Message, error) {
		return HandleSyncWithOpts(ctx, r, req, HandleOpts{MaxStoredBytes: testBudget})
	}
	server := httptest.NewServer(XferHandler(src, handle))
	defer server.Close()
	serve := &HTTPTransport{URL: server.URL}

	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"),
		serve, CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer clone.Close()
	requireHeld(t, clone, bigUUID)

	pulled := setupSyncTestRepo(t)
	res, err := Sync(context.Background(), pulled, serve,
		SyncOpts{Pull: true, ProjectCode: testPC})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("pull errors: %v", res.Errors)
	}
	requireHeld(t, pulled, bigUUID)
}

func requireHeld(t *testing.T, r *repo.Repo, uuid string) {
	t.Helper()
	if _, ok := blob.Exists(r.DB(), uuid); !ok || isPhantom(t, r, uuid) {
		t.Fatalf("%s not held", uuid)
	}
}

// The bound that lets a small payload skip compression must hold for the
// worst input, random bytes, at every scale a budget is set at.
func TestCompressBoundHoldsForRandomBytes(t *testing.T) {
	for i, n := range []int{0, 1, 100, 16383, 16384, 65535, 65536, 1 << 20, 3 << 20} {
		if got, bound := storedLen(t, randomBytes(int64(i), n)), compressBound(n); got > bound {
			t.Fatalf("%d random bytes store as %d, over the bound %d", n, got, bound)
		}
	}
}
