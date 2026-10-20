package sync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func requiredLeaf(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	data, err := (&deck.Deck{
		F: []deck.FileCard{{Name: name, UUID: hash.SHA3([]byte(name))}},
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return data, hash.SHA3(data)
}

// Exact file recovery is separate from structural closure traversal: arbitrary
// payload bytes need no deck type, and unsolicited inventory is still ignored.
func TestFetchRequiredFiles(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("newly covered payload")
	uuid := hash.SHA3(data)
	peer := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		return &xfer.Message{Cards: []xfer.Card{&xfer.FileCard{UUID: uuid, Content: data}}}
	}}
	result, err := FetchRequiredFiles(context.Background(), r, peer, []string{uuid}, SyncOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesRecvd != 1 {
		t.Fatalf("received %d files, want 1", result.FilesRecvd)
	}
	rid, held := content.AvailableByUUID(r.DB(), uuid)
	if !held {
		t.Fatal("required payload not held")
	}
	got, err := content.Expand(r.DB(), rid)
	if err != nil || string(got) != string(data) {
		t.Fatalf("payload recovery: %q, %v", got, err)
	}
}

// A malformed suffix must invalidate even a valid requested prefix; nothing
// staged from that response may be published as recovered content.
func TestFetchRequiredFilesMalformedReply(t *testing.T) {
	for name, malformed := range map[string]xfer.Card{
		"unknown": &xfer.UnknownCard{Command: "unexpected"},
		"command": &xfer.BadCommandCard{Line: "gimme too many arguments"},
		"file":    &xfer.BadFileCard{Message: "malformed file line"},
	} {
		t.Run(name, func(t *testing.T) {
			r := setupSyncTestRepo(t)
			data := []byte("requested valid prefix")
			uuid := hash.SHA3(data)
			peer := &MockTransport{Handler: func(*xfer.Message) *xfer.Message {
				return &xfer.Message{Cards: []xfer.Card{
					&xfer.FileCard{UUID: uuid, Content: data}, malformed,
				}}
			}}
			_, err := FetchRequiredFiles(context.Background(), r, peer, []string{uuid}, SyncOpts{})
			if err == nil {
				t.Fatal("accepted malformed reply after valid artifact")
			}
			if _, held := content.AvailableByUUID(r.DB(), uuid); held {
				t.Fatal("malformed reply published valid prefix")
			}
		})
	}
}

func TestFetchRequiredArtifactsNoPeer(t *testing.T) {
	r := setupSyncTestRepo(t)
	_, uuid := requiredLeaf(t, "outside/a")
	if _, err := blob.StorePhantom(r.DB(), uuid); err != nil {
		t.Fatal(err)
	}
	_, err := FetchRequiredArtifacts(context.Background(), r, nil, []string{uuid}, SyncOpts{})
	if err == nil {
		t.Fatal("no-peer recovery succeeded")
	}
	if !strings.Contains(err.Error(), "elided") || !strings.Contains(err.Error(), "no configured peer") {
		t.Fatalf("no-peer diagnostic: %v", err)
	}
	if strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("elision described as damage: %v", err)
	}
}

func TestFetchRequiredArtifactsExactDeltaClosure(t *testing.T) {
	r := setupSyncTestRepo(t)
	c, err := cone.New([]string{"inside"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	leaf, uuid := requiredLeaf(t, "outside/a")
	basis := []byte("a transfer-only basis, not structural content")
	basisUUID := hash.SHA3(basis)
	diff, err := delta.Create(basis, leaf)
	if err != nil {
		t.Fatal(err)
	}
	unwanted := []byte("outside file payload must never be stored")
	unwantedUUID := hash.SHA3(unwanted)
	calls := 0
	peer := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		calls++
		resp := &xfer.Message{Cards: []xfer.Card{&xfer.FileCard{UUID: unwantedUUID, Content: unwanted}}}
		for _, card := range req.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && (p.Name == "cone" || p.Name == "cone-not") {
				t.Fatal("targeted request inherited the store cone")
			}
			if g, ok := card.(*xfer.GimmeCard); ok {
				switch g.UUID {
				case uuid:
					resp.Cards = append(resp.Cards, &xfer.FileCard{UUID: uuid, DeltaSrc: basisUUID, Content: diff})
				case basisUUID:
					resp.Cards = append(resp.Cards, &xfer.FileCard{UUID: basisUUID, Content: basis})
				default:
					t.Fatalf("unrelated gimme %s", g.UUID)
				}
			}
		}
		return resp
	}}
	result, err := FetchRequiredArtifacts(context.Background(), r, peer, []string{uuid}, SyncOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.FilesRecvd != 1 {
		t.Fatalf("calls=%d received=%d, want 2/1", calls, result.FilesRecvd)
	}
	if rid, ok := content.AvailableByUUID(r.DB(), uuid); !ok {
		t.Fatal("required submanifest not recovered")
	} else if got, err := content.Expand(r.DB(), rid); err != nil || string(got) != string(leaf) {
		t.Fatalf("recovered content: %q, %v", got, err)
	}
	for _, outside := range []string{basisUUID, unwantedUUID} {
		if _, ok := blob.Exists(r.DB(), outside); ok {
			t.Fatalf("transfer-only or unrequested content escaped: %s", outside)
		}
	}
	if got, err := content.StoreCone(r.DB()); err != nil || got == nil || !got.Equal(c) {
		t.Fatalf("targeted recovery changed policy: %v, %v", got, err)
	}
}

func TestFetchRequiredArtifactsParentBaselineTree(t *testing.T) {
	source, target := setupSyncTestRepo(t), setupSyncTestRepo(t)
	inside, insideUUID := requiredLeaf(t, "inside/a")
	outside, outsideUUID := requiredLeaf(t, "outside/a")
	for _, data := range [][]byte{inside, outside} {
		if _, _, err := blob.Store(source.DB(), data); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := (&deck.Deck{
		C: "baseline tree", D: time.Unix(1, 0).UTC(), U: deck.User("test"),
		S: []deck.SubmanifestCard{
			{Name: "inside/a", UUID: insideUUID}, {Name: "outside/a", UUID: outsideUUID},
		},
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	_, baseUUID, err := blob.Store(source.DB(), baseline)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := (&deck.Deck{
		C: "delta parent", D: time.Unix(2, 0).UTC(), U: deck.User("test"), B: baseUUID,
		F: []deck.FileCard{{Name: "inside/a", UUID: hash.SHA3([]byte("edited"))}},
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parentRID, parentUUID, err := blob.Store(target.DB(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := blob.Store(target.DB(), inside); err != nil {
		t.Fatal(err)
	}
	if _, err := blob.StorePhantom(target.DB(), outsideUUID); err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"inside"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(target.DB(), c); err != nil {
		t.Fatal(err)
	}
	peer := requiredUnlockedPeer(t, source, target)
	result, err := FetchRequiredArtifacts(context.Background(), target, peer,
		[]string{parentUUID}, SyncOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesRecvd != 2 {
		t.Fatalf("received %d nodes, want baseline + absent S sibling", result.FilesRecvd)
	}
	files, err := manifest.ListFiles(target, parentRID)
	if err != nil || len(files) != 2 {
		t.Fatalf("delta-over-tree recovery: %+v, %v", files, err)
	}
	if files[0].UUID != hash.SHA3([]byte("edited")) ||
		files[1].UUID != hash.SHA3([]byte("outside/a")) {
		t.Fatalf("delta-over-tree files: %+v", files)
	}
	if _, held := content.AvailableByUUID(target.DB(), hash.SHA3([]byte("outside/a"))); held {
		t.Fatal("recovery fetched outside F payload")
	}
	if got, _ := content.StoreCone(target.DB()); got == nil || !got.Equal(c) {
		t.Fatal("recovery changed stored cone")
	}
}

func requiredUnlockedPeer(t *testing.T, source, target *repo.Repo) *MockTransport {
	t.Helper()
	return &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		// Fetch must not hold a publishing transaction during network I/O.
		if err := target.WithTx(func(tx *db.Tx) error {
			_, err := tx.Exec("SELECT count(*) FROM blob")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		reply, err := HandleSync(context.Background(), source, req)
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}}
}

func TestFetchRequiredArtifactsLocalDanglingDelta(t *testing.T) {
	r := setupSyncTestRepo(t)
	leaf, uuid := requiredLeaf(t, "outside/a")
	basis := []byte("absent local delta basis")
	basisUUID := hash.SHA3(basis)
	basisRID, err := blob.StorePhantom(r.DB(), basisUUID)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := delta.Create(basis, leaf)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := blob.StoreDeltaRaw(r.DB(), uuid, diff, basisRID, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer := &MockTransport{Handler: func(*xfer.Message) *xfer.Message {
		return &xfer.Message{Cards: []xfer.Card{&xfer.FileCard{UUID: uuid, Content: leaf}}}
	}}
	_, err = FetchRequiredArtifacts(context.Background(), r, peer, []string{uuid}, SyncOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if gotRID, held := content.AvailableByUUID(r.DB(), uuid); !held || gotRID != rid {
		t.Fatalf("dangling target identity not restored: %d/%d held=%v", gotRID, rid, held)
	}
	if _, held := content.AvailableByUUID(r.DB(), basisUUID); held {
		t.Fatal("full target recovery fetched irrelevant local basis")
	}
}

// A manifest visited as another root's baseline still needs its own B closure
// when it is independently requested as a full check-in root.
func TestFetchRequiredArtifactsBaselineAlsoRoot(t *testing.T) {
	source, target := setupSyncTestRepo(t), setupSyncTestRepo(t)
	base, err := (&deck.Deck{
		C: "flat baseline", D: time.Unix(1, 0).UTC(), U: deck.User("test"),
		F: []deck.FileCard{{Name: "outside/a", UUID: hash.SHA3([]byte("outside"))}},
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	_, baseUUID, err := blob.Store(source.DB(), base)
	if err != nil {
		t.Fatal(err)
	}
	roots := make([]string, 0, 2)
	baseline := baseUUID
	for i := 0; i < 2; i++ {
		data, err := (&deck.Deck{
			C: "delta root", D: time.Unix(int64(i+2), 0).UTC(),
			U: deck.User("test"), B: baseline,
		}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		_, uuid, err := blob.Store(target.DB(), data)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, uuid)
		baseline = uuid
	}
	peer := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		reply, err := HandleSync(context.Background(), source, req)
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}}
	_, err = FetchRequiredArtifacts(context.Background(), target, peer, roots, SyncOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, held := content.AvailableByUUID(target.DB(), baseUUID); !held {
		t.Fatal("baseline-only visitation suppressed an independently requested root's B closure")
	}
}

func TestFetchRequiredArtifactsWrongProject(t *testing.T) {
	r := setupSyncTestRepo(t)
	data, uuid := requiredLeaf(t, "outside/a")
	peer := &MockTransport{Handler: func(*xfer.Message) *xfer.Message {
		return &xfer.Message{Cards: []xfer.Card{
			&xfer.FileCard{UUID: uuid, Content: data}, &xfer.PushCard{ProjectCode: "wrong"},
		}}
	}}
	_, err := FetchRequiredArtifacts(context.Background(), r, peer, []string{uuid}, SyncOpts{})
	if err == nil || !strings.Contains(err.Error(), "wrong project") {
		t.Fatalf("wrong peer accepted: %v", err)
	}
	if _, held := blob.Exists(r.DB(), uuid); held {
		t.Fatal("wrong project leaked content")
	}
}

func TestFetchRequiredArtifactsAuthenticatedPrivate(t *testing.T) {
	for _, password := range []string{"secret", "wrong"} {
		t.Run(password, func(t *testing.T) {
			source, target := setupSyncTestRepo(t), setupSyncTestRepo(t)
			if err := auth.UpdateCaps(source.DB(), "nobody", ""); err != nil {
				t.Fatal(err)
			}
			if err := auth.CreateUser(source.DB(), testPC, "required-reader", "secret", "ox"); err != nil {
				t.Fatal(err)
			}
			leaf, _ := requiredLeaf(t, "private/a")
			rid, uuid, err := blob.Store(source.DB(), leaf)
			if err != nil {
				t.Fatal(err)
			}
			if err := content.MakePrivate(source.DB(), int64(rid)); err != nil {
				t.Fatal(err)
			}
			peer := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
				wire, err := req.Encode()
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := xfer.Decode(wire, xfer.ContentTypeCompressed)
				if err != nil {
					t.Fatal(err)
				}
				reply, err := HandleSync(context.Background(), source, decoded)
				if err != nil {
					t.Fatal(err)
				}
				return reply
			}}
			_, err = FetchRequiredArtifacts(context.Background(), target, peer, []string{uuid},
				SyncOpts{User: "required-reader", Password: password, Private: true})
			if password == "wrong" {
				if err == nil {
					t.Fatal("invalid password recovered private content")
				}
				if _, held := blob.Exists(target.DB(), uuid); held {
					t.Fatal("failed authentication leaked private content")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			heldRID, held := content.AvailableByUUID(target.DB(), uuid)
			if !held || !content.IsPrivate(target.DB(), int64(heldRID)) {
				t.Fatal("private recovery lost private visibility")
			}
			var count int
			err = target.DB().QueryRow(
				"SELECT count(*) FROM config WHERE value IN ('secret','wrong')").Scan(&count)
			if err != nil || count != 0 {
				t.Fatalf("recovery persisted credentials: count=%d err=%v", count, err)
			}
		})
	}
}

func TestFetchRequiredArtifactsFailureAtomic(t *testing.T) {
	for _, mode := range []string{"incomplete", "corrupt", "shunned", "private", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			r := setupSyncTestRepo(t)
			first, firstUUID := requiredLeaf(t, "outside/a")
			second, secondUUID := requiredLeaf(t, "outside/b")
			if mode == "shunned" {
				if _, err := r.DB().Exec("INSERT INTO shun(uuid) VALUES(?)", secondUUID); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			peer := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
				calls++
				cards := []xfer.Card{&xfer.FileCard{UUID: firstUUID, Content: first}}
				switch mode {
				case "corrupt":
					cards = append(cards, &xfer.FileCard{UUID: secondUUID, Content: []byte("not its hash")})
				case "private":
					cards = append(cards, &xfer.PrivateCard{}, &xfer.FileCard{UUID: secondUUID, Content: second})
				case "cancelled":
					cancel()
				}
				return &xfer.Message{Cards: cards}
			}}
			_, err := FetchRequiredArtifacts(ctx, r, peer, []string{firstUUID, secondUUID}, SyncOpts{})
			if err == nil {
				t.Fatal("invalid recovery succeeded")
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation not preserved: %v", err)
			}
			if mode == "shunned" && calls != 0 {
				t.Fatal("requested shunned content")
			}
			for _, uuid := range []string{firstUUID, secondUUID} {
				if _, ok := blob.Exists(r.DB(), uuid); ok {
					t.Fatalf("failed recovery leaked %s", uuid)
				}
			}
		})
	}
}
