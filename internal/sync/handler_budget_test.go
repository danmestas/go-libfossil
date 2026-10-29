package sync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/auth"
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

// pushBudgeted sends cards in one push round to r under a store budget. A
// refused round's RefusedError must carry the reply's one error card.
func pushBudgeted(t *testing.T, r *repo.Repo, limit int, cards ...xfer.Card) *xfer.Message {
	t.Helper()
	resp, err := pushBudgetedErr(r, limit, cards...)
	var refused *RefusedError
	if errors.As(err, &refused) {
		if got := onlyError(t, resp); got != refused.Message {
			t.Fatalf("RefusedError %q, error card %q", refused.Message, got)
		}
		return resp
	}
	if err != nil {
		t.Fatalf("HandleSyncWithOpts: %v", err)
	}
	return resp
}

func pushBudgetedErr(r *repo.Repo, limit int, cards ...xfer.Card) (*xfer.Message, error) {
	req := &xfer.Message{Cards: append([]xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: testPC}}, cards...)}
	return HandleSyncWithOpts(context.Background(), r, req,
		HandleOpts{MaxStoredBytes: limit})
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

// storeDigest summarises every table a received artifact can touch, so a
// refused round can be shown to have left all of them as they were.
func storeDigest(t *testing.T, r *repo.Repo) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		"SELECT group_concat(uuid || '/' || size || '/' || length(content), ',') FROM (SELECT * FROM blob ORDER BY uuid)",
		"SELECT group_concat(rid, ',') FROM (SELECT rid FROM unclustered ORDER BY rid)",
		"SELECT group_concat(rid, ',') FROM (SELECT rid FROM phantom ORDER BY rid)",
		"SELECT group_concat(rid || '>' || srcid, ',') FROM (SELECT * FROM delta ORDER BY rid)",
		"SELECT group_concat(mid || ':' || fid || ':' || fnid, ',') FROM (SELECT * FROM mlink ORDER BY mid, fnid)",
	} {
		var v sql.NullString
		if err := r.DB().QueryRow(q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		parts = append(parts, v.String)
	}
	return strings.Join(parts, "\n")
}

// requireUnchanged fails when r's store differs from the digest before.
func requireUnchanged(t *testing.T, r *repo.Repo, before string) {
	t.Helper()
	if after := storeDigest(t, r); after != before {
		t.Fatalf("store changed across a refused round:\nbefore %s\n after %s", before, after)
	}
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
	before := storeDigest(t, r)

	resp := pushBudgeted(t, r, testBudget,
		&xfer.FileCard{UUID: smallUUID, Content: small},
		&xfer.FileCard{UUID: bigUUID, Content: big})

	if got, want := onlyError(t, resp), budgetText(bigUUID, storedLen(t, big)); got != want {
		t.Fatalf("error card:\n got %q\nwant %q", got, want)
	}
	requireAbsent(t, r, smallUUID, bigUUID)
	requireUnchanged(t, r, before)
}

// A refused round returns its reply with a *RefusedError carrying the error
// card's text, so a host that aborts on any rollback can report the refusal
// without parsing the reply; a round that fits returns no error.
func TestBudgetRefusalReturnsRefusedError(t *testing.T) {
	r := setupPushableTestRepo(t)
	big := randomBytes(9, 2*testBudget)
	uuid := hash.SHA1(big)

	resp, err := pushBudgetedErr(r, testBudget, &xfer.FileCard{UUID: uuid, Content: big})

	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *RefusedError", err)
	}
	want := budgetText(uuid, storedLen(t, big))
	if refused.Message != want || refused.Error() != want {
		t.Fatalf("RefusedError: Message %q, Error() %q, want %q", refused.Message, refused.Error(), want)
	}
	if resp == nil || onlyError(t, resp) != want {
		t.Fatalf("refused round must still return its error-card reply: %+v", resp)
	}

	small := []byte("fits")
	resp, err = pushBudgetedErr(r, testBudget, &xfer.FileCard{UUID: hash.SHA1(small), Content: small})
	if err != nil {
		t.Fatalf("a push that fits returned %v", err)
	}
	requireNoError(t, resp)
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
// says which file of which check-in is too big, but only to a pusher who may
// read the repository, and never for a private check-in.
func TestBudgetNamesPathAndCheckin(t *testing.T) {
	for _, tc := range []struct {
		name, path, caps string
		private, named   bool
	}{
		{name: "reader", path: "hello.txt", caps: "gijoryz", named: true},
		{name: "private check-in", path: "hello.txt", caps: "gijoryz", private: true},
		{name: "card syntax in path", path: "a b\nerror x", caps: "gijoryz", named: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupPushableTestRepo(t)
			big := randomBytes(3, 2*testBudget)
			bigUUID := hash.SHA1(big)
			mBytes, mUUID := buildNamedCheckinManifest(t, tc.path, "adds a big file",
				time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), bigUUID, big, nil)
			requireNoError(t, pushBudgeted(t, r, testBudget,
				&xfer.FileCard{UUID: mUUID, Content: mBytes}))
			if tc.private {
				markPrivate(t, r, mUUID)
			}
			if err := auth.UpdateCaps(r.DB(), "nobody", tc.caps); err != nil {
				t.Fatal(err)
			}
			before := storeDigest(t, r)

			resp := pushBudgeted(t, r, testBudget, &xfer.FileCard{UUID: bigUUID, Content: big})

			want := budgetText(bigUUID, storedLen(t, big))
			if tc.named {
				want += ", named " + tc.path + " by check-in " + mUUID
			}
			if got := onlyError(t, resp); got != want {
				t.Fatalf("error card:\n got %q\nwant %q", got, want)
			}
			requireOneErrorLine(t, resp, want)
			requireUnchanged(t, r, before)
		})
	}
}

// Naming is gated on read, and every pusher reads: fossil's i implies o.
// If that ever changes, the gate in overBudget starts to matter.
func TestBudgetPushImpliesRead(t *testing.T) {
	if !auth.CanRead("i") {
		t.Fatal("push no longer implies read: add a push-only case to TestBudgetNamesPathAndCheckin")
	}
}

func markPrivate(t *testing.T, r *repo.Repo, uuid string) {
	t.Helper()
	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatalf("%s not held", uuid)
	}
	if _, err := r.DB().Exec("INSERT INTO private(rid) VALUES(?)", rid); err != nil {
		t.Fatal(err)
	}
}

// requireOneErrorLine fails unless resp goes out as a single error line,
// whatever the message holds, and decodes back to want.
func requireOneErrorLine(t *testing.T, resp *xfer.Message, want string) {
	t.Helper()
	raw, err := resp.EncodeUncompressed()
	if err != nil {
		t.Fatal(err)
	}
	var errLines int
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "error ") {
			errLines++
		}
	}
	if errLines != 1 {
		t.Fatalf("want one error line on the wire, got %d:\n%s", errLines, raw)
	}
	back, err := xfer.Decode(raw, xfer.ContentTypeUncompressed)
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if got := onlyError(t, back); got != want {
		t.Fatalf("decoded error card:\n got %q\nwant %q", got, want)
	}
}

// An artifact the store already holds in full stores nothing again, so the
// budget does not refuse it.
func TestBudgetSkipsArtifactHeldInFull(t *testing.T) {
	r := setupPushableTestRepo(t)
	big := randomBytes(8, 2*testBudget)
	uuid := hash.SHA1(big)
	requireNoError(t, pushBudgeted(t, r, 0, &xfer.FileCard{UUID: uuid, Content: big}))
	before := storeDigest(t, r)
	requireNoError(t, pushBudgeted(t, r, testBudget, &xfer.FileCard{UUID: uuid, Content: big}))
	requireUnchanged(t, r, before)
}

// A phantom no check-in names is refused like any other artifact, and stays
// a phantom.
func TestBudgetRefusesOversizePhantom(t *testing.T) {
	r := setupPushableTestRepo(t)
	big := randomBytes(9, 2*testBudget)
	uuid := hash.SHA1(big)
	if _, err := blob.StorePhantom(r.DB(), uuid); err != nil {
		t.Fatal(err)
	}
	before := storeDigest(t, r)
	resp := pushBudgeted(t, r, testBudget, &xfer.FileCard{UUID: uuid, Content: big})
	if got, want := onlyError(t, resp), budgetText(uuid, storedLen(t, big)); got != want {
		t.Fatalf("error card:\n got %q\nwant %q", got, want)
	}
	requireUnchanged(t, r, before)
	if !isPhantom(t, r, uuid) {
		t.Fatal("the refused phantom is no longer a phantom")
	}
}

// filesObserver records what the handler reports at the end of a request.
type filesObserver struct {
	nopObserver
	end *HandleEnd
}

func (o filesObserver) HandleCompleted(_ context.Context, info HandleEnd) { *o.end = info }

// A sync round, pull and igot cards beside the push, is refused whole too:
// one error card, nothing stored, and no file reported received.
func TestBudgetRefusesSyncRoundWhole(t *testing.T) {
	r := setupPushableTestRepo(t)
	small := []byte("small file")
	big := randomBytes(10, 2*testBudget)
	smallUUID, bigUUID := hash.SHA1(small), hash.SHA1(big)
	other := hash.SHA1([]byte("an artifact the client holds"))
	before := storeDigest(t, r)
	var end HandleEnd
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: testPC},
		&xfer.PullCard{ServerCode: "test", ProjectCode: testPC},
		&xfer.IGotCard{UUID: other},
		&xfer.FileCard{UUID: smallUUID, Content: small},
		&xfer.FileCard{UUID: bigUUID, Content: big},
	}}
	resp, err := HandleSyncWithOpts(context.Background(), r, req,
		HandleOpts{MaxStoredBytes: testBudget, Observer: filesObserver{end: &end}})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("HandleSyncWithOpts: %v, want *RefusedError", err)
	}
	if got, want := onlyError(t, resp), budgetText(bigUUID, storedLen(t, big)); got != want {
		t.Fatalf("error card:\n got %q\nwant %q", got, want)
	}
	if end.FilesReceived != 0 {
		t.Fatalf("FilesReceived = %d after a refused round, want 0", end.FilesReceived)
	}
	requireUnchanged(t, r, before)
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
