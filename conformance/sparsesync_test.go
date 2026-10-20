package conformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func scSyncCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.6.1.wire-members", Req: "SC-6.1-1"}, scSyncMembers},
		{Case{ID: "sparsecone.8.wire-bounds", Req: "SC-8-1"}, scSyncBounds},
		{Case{ID: "sparsecone.5.3.origin-refusal", Req: "SC-5.3-2"}, scSyncOrigin},
		{Case{ID: "sparsecone.5.3.clone-fatal", Req: "SC-5.3-3"}, scSyncFatal},
		{Case{ID: "sparsecone.5.3.auth-challenge", Req: "SC-5.3-3"}, scSyncAuth},
		{Case{ID: "sparsecone.6.2.full-delta", Req: "SC-6.2-2"}, scSyncDelta},
		{Case{ID: "sparsecone.6.1.aware-clone-pull", Req: "SC-6.1-2"}, scSyncAware},
		{Case{ID: "sparsecone.6.3.ignored-clone-pull", Req: "SC-6.3-1"}, scSyncIgnored},
		{Case{ID: "sparsecone.6.4.explicit-peer", Req: "SC-6.4-1"}, scSyncWiden},
		{Case{ID: "sparsecone.6.4.no-peer", Req: "SC-6.4-2"}, scSyncNoPeer},
	}
}

// Run the cases directly as well as through the shared suite: no deviation
// metadata may turn a protocol assertion failure into a passing test here.
func TestSparseSync(t *testing.T) {
	for _, c := range scSyncCases() {
		t.Run(c.ID, func(t *testing.T) {
			if err := c.run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Member expectations are literal wire strings, not derived with the encoder
// under test. Holdings distinguish a literal '*' from a wildcard.
func scSyncMembers(t *testing.T) error {
	origin := newSCSyncRepo(t, map[string]string{
		"literal*/a": "star\n", "literalX/a": "not star\n",
		"src space/a": "space\n", "src space/private/a": "private\n",
	})
	c := scCone(t, []string{"src space", "literal*"}, []string{"src space/private"})
	peer := &scSyncPeer{r: origin.r}
	r, _ := scSyncClone(t, peer, sync.CloneOpts{StoreCone: c, PullPhantoms: true})
	want := []string{`cone literal*`, `cone src\sspace`, `cone-not src\sspace/private`}
	for round, req := range peer.requests {
		var got []string
		for _, card := range req.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && (p.Name == "cone" || p.Name == "cone-not") {
				got = append(got, p.Name+" "+strings.Join(p.Values, " "))
			}
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("round %d pragmas=%q, want %q", round, got, want)
		}
	}
	for text, want := range map[string]bool{
		"star\n": true, "not star\n": false, "space\n": true, "private\n": false,
	} {
		if err := scSyncHolding(r, text, want); err != nil {
			return err
		}
	}
	return nil
}

// Drive the server through decoded wire cards at and beyond both bounds.
func scSyncBounds(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	members := func(n int) []xfer.Card {
		cards := make([]xfer.Card, 0, n+1)
		for i := 0; i < n; i++ {
			cards = append(cards, &xfer.PragmaCard{Name: "cone", Values: []string{fmt.Sprintf("p%03d", i)}})
		}
		return cards
	}
	for _, tc := range []struct {
		name  string
		cards []xfer.Card
		valid bool
	}{
		{"max-count", members(255), true},
		{"over-count", members(256), false},
		{"max-length", []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{strings.Repeat("a", 4096)}}}, true},
		{"over-length", []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{strings.Repeat("a", 4097)}}}, false},
		{"encoded-bound", []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{strings.Repeat("a", 8193)}}}, false},
		{"arity", []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{"src", "extra"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cards := append(tc.cards, &xfer.CloneCard{Version: 3, SeqNo: 1})
			peer := &scSyncPeer{r: origin.r}
			resp, err := peer.Exchange(context.Background(), &xfer.Message{Cards: cards})
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, card := range resp.Cards {
				if e, ok := card.(*xfer.ErrorCard); ok {
					n++
					if !strings.HasPrefix(e.Message, "sparse cone:") {
						t.Fatalf("unexpected error %q", e.Message)
					}
				}
			}
			if tc.valid && n != 0 || !tc.valid && (n != 1 || len(scSyncPayloads(resp)) != 0) {
				t.Fatalf("valid=%v errors=%d payloads=%v", tc.valid, n, scSyncPayloads(resp))
			}
		})
	}
	return nil
}

const scSyncOriginRefusal = "Sparse cone does not cover requested clone; configure a partial mirror to allow it."

func scSyncOrigin(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	scSparsify(t, origin.r, scCone(t, []string{"src"}, nil))
	peer := &scSyncPeer{r: origin.r}
	resp, err := peer.Exchange(context.Background(), &xfer.Message{
		Cards: []xfer.Card{&xfer.CloneCard{Version: 3, SeqNo: 1}},
	})
	if err != nil {
		return err
	}
	if err := scSyncRefused(resp, scSyncOriginRefusal); err != nil {
		return err
	}
	peer.opts.PartialMirror = true
	r, result := scSyncClone(t, peer, sync.CloneOpts{})
	if result.BlobsRecvd == 0 {
		return fmt.Errorf("explicit partial mirror made no clone progress")
	}
	if err := scSyncHolding(r, "inside\n", true); err != nil {
		return err
	}
	return scSyncHolding(r, "outside\n", false)
}

func scSyncFatal(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	scSparsify(t, origin.r, scCone(t, []string{"src"}, nil))
	peer := &scSyncPeer{r: origin.r}
	path := filepath.Join(t.TempDir(), "refused.fossil")
	r, result, err := sync.Clone(context.Background(), path, peer, sync.CloneOpts{})
	if r != nil {
		r.Close()
		return fmt.Errorf("refused clone returned a repository")
	}
	if err == nil || !strings.Contains(err.Error(), scSyncOriginRefusal) {
		return fmt.Errorf("clone refusal lost: %v", err)
	}
	if result == nil || result.BlobsRecvd != 0 || result.Rounds > 2 {
		return fmt.Errorf("refused clone progressed or kept retrying: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return fmt.Errorf("failed clone left database: %v", err)
	}
	for _, resp := range peer.replies {
		if err := scSyncRefused(resp, scSyncOriginRefusal); err != nil {
			return err
		}
	}
	for _, req := range peer.requests {
		for _, card := range req.Cards {
			if _, ok := card.(*xfer.GimmeCard); ok {
				return fmt.Errorf("refused clone requested content")
			}
		}
	}
	return nil
}

// A real authentication challenge still teaches the project code before the
// second request authenticates; the sparse refusal must not break this path.
func scSyncAuth(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	if _, err := origin.r.DB().Exec("UPDATE user SET cap='' WHERE login='nobody'"); err != nil {
		return err
	}
	code, err := origin.r.Config("project-code")
	if err != nil {
		return err
	}
	if err := auth.CreateUser(origin.r.DB(), code, "cone-reader", "test-password", "go"); err != nil {
		return err
	}
	peer := &scSyncPeer{r: origin.r}
	r, _ := scSyncClone(t, peer, sync.CloneOpts{User: "cone-reader", Password: "test-password"})
	if len(peer.requests) < 2 {
		return fmt.Errorf("authentication challenge did not retry")
	}
	if err := scSyncRefused(peer.replies[0], "not authorized to clone"); err != nil {
		return err
	}
	login := false
	for _, card := range peer.requests[1].Cards {
		if c, ok := card.(*xfer.LoginCard); ok && c.User == "cone-reader" {
			login = true
		}
	}
	if !login {
		return fmt.Errorf("challenge retry omitted login")
	}
	return scSyncHolding(r, "outside\n", true)
}

// The database really stores child as a delta before the protocol is tested.
// The wire must omit the excluded base and send child without DeltaSrc.
func scSyncDelta(t *testing.T) error {
	base := strings.Repeat("base content\n", 1000)
	child := strings.Repeat("base content\n", 999) + "changed\n"
	origin := newSCSyncRepo(t, map[string]string{"src/a": child, "zzz/a": base})
	baseUUID, childUUID := hash.SHA1([]byte(base)), hash.SHA1([]byte(child))
	baseRID, ok := content.AvailableByUUID(origin.r.DB(), baseUUID)
	if !ok {
		return fmt.Errorf("base fixture missing")
	}
	childRID, ok := content.AvailableByUUID(origin.r.DB(), childUUID)
	if !ok {
		return fmt.Errorf("child fixture missing")
	}
	if err := origin.r.WithTx(func(tx *db.Tx) error {
		_, err := content.Deltify(tx, childRID, baseRID)
		return err
	}); err != nil {
		return err
	}
	var source int64
	if err := origin.r.DB().QueryRow("SELECT srcid FROM delta WHERE rid=?", childRID).Scan(&source); err != nil {
		return err
	}
	if source != int64(baseRID) {
		return fmt.Errorf("fixture is not a delta over excluded base")
	}
	peer := &scSyncPeer{r: origin.r}
	r, _ := scSyncClone(t, peer, sync.CloneOpts{
		StoreCone: scCone(t, []string{"src"}, nil), PullPhantoms: true,
	})
	seen := false
	for _, resp := range peer.replies {
		payloads := scSyncPayloads(resp)
		if _, ok := payloads[baseUUID]; ok {
			return fmt.Errorf("server sent excluded delta base")
		}
		if delta, ok := payloads[childUUID]; ok {
			seen = true
			if delta != "" {
				return fmt.Errorf("server sent child delta over %s", delta)
			}
		}
	}
	if !seen {
		return fmt.Errorf("server omitted retained child")
	}
	if err := scSyncNoGimme(peer.requests, baseUUID); err != nil {
		return err
	}
	return scSyncHolding(r, child, true)
}

func scSyncAware(t *testing.T) error   { return scSyncClonePull(t, false) }
func scSyncIgnored(t *testing.T) error { return scSyncClonePull(t, true) }

// Independent SHA1 expectations cover both clone and a later pull, including
// an artifact shared by an in-cone and an out-of-cone path in each round.
func scSyncClonePull(t *testing.T, ignore bool) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	peer := &scSyncPeer{r: origin.r, ignore: ignore}
	r, result := scSyncClone(t, peer, sync.CloneOpts{
		StoreCone: scCone(t, []string{"src"}, nil), PullPhantoms: true,
	})
	if result.Elided != 1 {
		return fmt.Errorf("clone elided=%d, want 1", result.Elided)
	}
	for text, held := range map[string]bool{"inside\n": true, "shared\n": true, "outside\n": false} {
		if err := scSyncHolding(r, text, held); err != nil {
			return err
		}
	}
	files := scSyncFiles()
	files["src/new"], files["zzz/new-shared"] = "new shared\n", "new shared\n"
	files["zzz/new"] = "new outside\n"
	origin.commit(t, files, origin.c1)
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if pulled.Elided != 2 {
		return fmt.Errorf("pull elided=%d, want 2", pulled.Elided)
	}
	for text, held := range map[string]bool{"new shared\n": true, "new outside\n": false} {
		if err := scSyncHolding(r, text, held); err != nil {
			return err
		}
	}
	if err := scSyncNoGimme(peer.requests, hash.SHA1([]byte("outside\n"))); err != nil {
		return err
	}
	stable := len(peer.requests)
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	for _, text := range []string{"outside\n", "new outside\n"} {
		uuid := hash.SHA1([]byte(text))
		// A legacy inventory may introduce an unknown UUID before its manifest.
		// Once classified, neither it nor an older elision may be requested.
		requests := peer.requests
		if ignore {
			requests = requests[stable:]
		}
		if err := scSyncNoGimme(requests, uuid); err != nil {
			return err
		}
		seen := false
		for _, resp := range peer.replies {
			if _, ok := scSyncPayloads(resp)[uuid]; ok {
				seen = true
			}
		}
		if seen != ignore {
			return fmt.Errorf("excluded payload %s seen=%v, legacy=%v", uuid, seen, ignore)
		}
	}
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		return err
	}
	if a.Count(content.Elided) != 2 || a.Count(content.Transit) != 0 {
		return fmt.Errorf("actual absences: elided=%d transit=%d", a.Count(content.Elided), a.Count(content.Transit))
	}
	return nil
}

func scSyncWiden(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	peer := &scSyncPeer{r: origin.r}
	r, _ := scSyncClone(t, peer, sync.CloneOpts{
		StoreCone: scCone(t, []string{"src"}, nil), PullPhantoms: true,
	})
	if err := scSyncHolding(r, "outside\n", false); err != nil {
		return err
	}
	before := len(peer.requests)
	result, err := sync.FetchCone(context.Background(), r, peer, nil, sync.SyncOpts{})
	if err != nil {
		return err
	}
	if result == nil || result.FilesRecvd == 0 || result.Elided != 0 || len(peer.requests) == before {
		return fmt.Errorf("explicit peer widening did not fetch: %+v", result)
	}
	if c, err := content.StoreCone(r.DB()); err != nil || c != nil {
		return fmt.Errorf("widened cone=%v error=%v", c, err)
	}
	return scSyncHolding(r, "outside\n", true)
}

func scSyncNoPeer(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	peer := &scSyncPeer{r: origin.r}
	c := scCone(t, []string{"src"}, nil)
	r, _ := scSyncClone(t, peer, sync.CloneOpts{StoreCone: c, PullPhantoms: true})
	before := len(peer.requests)
	_, err := sync.FetchCone(context.Background(), r, nil, nil, sync.SyncOpts{})
	want := "sparse cone: 1 elided artifact(s); no configured peer to fetch 1 newly covered artifact(s)"
	if err == nil || err.Error() != want {
		return fmt.Errorf("no-peer error=%v, want %q", err, want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sync.FetchCone(ctx, r, peer, nil, sync.SyncOpts{}); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("cancelled fetch=%v", err)
	}
	if len(peer.requests) != before {
		return fmt.Errorf("no-peer/cancelled operation made network progress")
	}
	got, err := content.StoreCone(r.DB())
	if err != nil || got == nil || !got.Equal(c) {
		return fmt.Errorf("failed demand changed store cone: %v %v", got, err)
	}
	return scSyncHolding(r, "outside\n", false)
}
