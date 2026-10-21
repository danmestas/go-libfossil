package sync

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestConePragmasEscaping(t *testing.T) {
	c, err := cone.New([]string{"src space", "literal*"}, []string{"src space/private"})
	if err != nil {
		t.Fatal(err)
	}
	cards := conePragmas(c)
	got := make([]string, 0, len(cards))
	for _, card := range cards {
		p := card.(*xfer.PragmaCard)
		got = append(got, p.Name+" "+strings.Join(p.Values, " "))
	}
	want := []string{`cone literal*`, `cone src\sspace`, `cone-not src\sspace/private`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cards = %q, want %q", got, want)
	}
	round, err := requestCone(cards)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round.Serialize(), c.Serialize()) {
		t.Fatal("cone changed across pragmas")
	}
}

func TestSparseCloneIgnorantPeerFiltersLocally(t *testing.T) {
	r := setupSyncTestRepo(t)
	kept := storeAndLink(t, r, []byte("covered content"))
	out := storeAndLink(t, r, []byte("uncovered content"))
	data, err := (&deck.Deck{C: "local filtering", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "out/a", UUID: out}, {Name: "src/a", UUID: kept}, {Name: "src/shared", UUID: out}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, r, data)
	extra := storeAndLink(t, r, []byte("unshared excluded content"))
	data, err = (&deck.Deck{C: "next", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "out/b", UUID: extra}, {Name: "src/a", UUID: kept}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, r, data)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		var cards []xfer.Card
		for _, card := range req.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && (p.Name == "cone" || p.Name == "cone-not") {
				continue
			}
			cards = append(cards, card)
		}
		resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}}
	got, res, err := Clone(context.Background(), filepath.Join(t.TempDir(), "sparse.fossil"), transport, CloneOpts{StoreCone: c, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if _, ok := content.AvailableByUUID(got.DB(), kept); !ok {
		t.Fatal("covered blob unavailable")
	}
	if _, ok := content.AvailableByUUID(got.DB(), out); !ok {
		t.Fatal("shared blob unavailable")
	}
	if _, ok := content.AvailableByUUID(got.DB(), extra); ok {
		t.Fatal("surplus excluded content retained")
	}
	a, err := content.LoadAbsences(got.DB())
	if err != nil {
		t.Fatal(err)
	}
	if a.Count(content.Elided) != 1 {
		t.Fatalf("elided count=%d", a.Count(content.Elided))
	}
	if res.Elided != 1 {
		t.Fatalf("reported elided=%d", res.Elided)
	}
}

func TestSparseReceiveFiltersDeltaPair(t *testing.T) {
	r := setupSyncTestRepo(t)
	base := storeAndLink(t, r, []byte(strings.Repeat("base content\n", 1000)))
	child := storeAndLink(t, r, []byte(strings.Repeat("base content\n", 999)+"changed\n"))
	baseRID, _ := content.AvailableByUUID(r.DB(), base)
	childRID, _ := content.AvailableByUUID(r.DB(), child)
	if err := r.WithTx(func(tx *db.Tx) error { _, err := content.Deltify(tx, childRID, baseRID); return err }); err != nil {
		t.Fatal(err)
	}
	data, err := (&deck.Deck{C: "delta filtering", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "out/a", UUID: base}, {Name: "out/b", UUID: child}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, r, data)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	if err := r.WithTx(func(tx *db.Tx) error {
		_, err := filterSparseReceived(context.Background(), tx, map[string]bool{base: true, child: true}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{base, child} {
		if _, ok := content.AvailableByUUID(r.DB(), uuid); ok {
			t.Fatalf("excluded delta artifact retained: %s", uuid)
		}
	}
}

func TestSparseServerExpandsExcludedDeltaBase(t *testing.T) {
	r := setupSyncTestRepo(t)
	base := storeAndLink(t, r, []byte(strings.Repeat("base content\n", 1000)))
	child := storeAndLink(t, r, []byte(strings.Repeat("base content\n", 999)+"changed\n"))
	baseRID, _ := content.AvailableByUUID(r.DB(), base)
	childRID, _ := content.AvailableByUUID(r.DB(), child)
	if err := r.WithTx(func(tx *db.Tx) error { _, err := content.Deltify(tx, childRID, baseRID); return err }); err != nil {
		t.Fatal(err)
	}
	var source int64
	if err := r.DB().QueryRow("SELECT srcid FROM delta WHERE rid=?", childRID).Scan(&source); err != nil || source != int64(baseRID) {
		t.Fatalf("delta fixture: source=%d err=%v", source, err)
	}
	data, err := (&deck.Deck{C: "delta boundary", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "src/a", UUID: child}, {Name: "zzz/a", UUID: base}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, r, data)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cards := []xfer.Card{versionPragma("client-version"), sendSubmanifests(), &xfer.CloneCard{Version: 3, SeqNo: 1}}
	cards = append(cards, conePragmas(c)...)
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, card := range findCards[*xfer.CFileCard](resp) {
		if card.UUID == base {
			t.Fatal("sent excluded delta base")
		}
		if card.UUID == child {
			seen = true
			if card.DeltaSrc != "" {
				t.Fatalf("cross-cone delta source %s", card.DeltaSrc)
			}
		}
	}
	if !seen {
		t.Fatal("omitted retained delta child")
	}
}

func TestSparseCloneRefusalFatal(t *testing.T) {
	r := setupSyncTestRepo(t)
	cs := &cloneSession{repo: r, seqno: 1}
	refusal := &xfer.Message{Cards: []xfer.Card{
		&xfer.ErrorCard{Message: "Sparse cone refuses clone"},
	}}
	if err := cs.processResponse(context.Background(), 0, refusal); err != nil {
		t.Fatalf("first-round cleanup was bypassed: %v", err)
	}
	if !cs.refused || cs.serverErr != "Sparse cone refuses clone" {
		t.Fatal("no-progress refusal was not retained")
	}
	err := cs.processResponse(context.Background(), 1, &xfer.Message{})
	if err == nil || !strings.Contains(err.Error(), "Sparse cone refuses clone") {
		t.Fatalf("refused clone returned %v after cleanup", err)
	}
}

func TestSparseServerTreeClone(t *testing.T) {
	r := setupSyncTestRepo(t)
	kept := storeAndLink(t, r, []byte("covered tree file"))
	out := storeAndLink(t, r, []byte("uncovered tree file"))
	marshal := func(d *deck.Deck) string {
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return storeAndLink(t, r, data)
	}
	outsideLeaf := marshal(&deck.Deck{F: []deck.FileCard{{Name: "zzz/a", UUID: out}}})
	insideLeaf := marshal(&deck.Deck{F: []deck.FileCard{{Name: "src/a", UUID: kept}}})
	spine := marshal(&deck.Deck{C: "tree sparse", D: time.Now().UTC(), U: deck.User("u"), S: []deck.SubmanifestCard{{Name: "src/a", UUID: insideLeaf}, {Name: "zzz/a", UUID: outsideLeaf}}})
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cards := []xfer.Card{versionPragma("client-version"), sendSubmanifests(), &xfer.CloneCard{Version: 3, SeqNo: 1}}
	cards = append(cards, conePragmas(c)...)
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, card := range findCards[*xfer.CFileCard](resp) {
		seen[card.UUID] = true
	}
	if seen[out] || seen[outsideLeaf] {
		t.Fatalf("sent out-of-cone tree artifacts: %v", seen)
	}
	if !seen[kept] || !seen[insideLeaf] || !seen[spine] {
		t.Fatalf("omitted tree keep set: %v", seen)
	}
	for _, ignorant := range []bool{false, true} {
		name := "aware"
		if ignorant {
			name = "ignorant"
		}
		t.Run(name, func(t *testing.T) {
			transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
				if ignorant {
					var cards []xfer.Card
					for _, card := range req.Cards {
						if p, ok := card.(*xfer.PragmaCard); ok && (p.Name == "cone" || p.Name == "cone-not") {
							continue
						}
						cards = append(cards, card)
					}
					req = &xfer.Message{Cards: cards}
				}
				resp, err := HandleSync(context.Background(), r, req)
				if err != nil {
					t.Fatal(err)
				}
				return resp
			}}
			got, res, err := Clone(context.Background(), filepath.Join(t.TempDir(), "tree.fossil"), transport, CloneOpts{StoreCone: c, PullPhantoms: true})
			if err != nil {
				t.Fatal(err)
			}
			defer got.Close()
			for _, uuid := range []string{kept, insideLeaf, spine} {
				if _, ok := content.AvailableByUUID(got.DB(), uuid); !ok {
					t.Fatalf("sparse tree clone lost %s", uuid)
				}
			}
			if _, ok := content.AvailableByUUID(got.DB(), out); ok {
				t.Fatalf("sparse tree clone retained %s", out)
			}
			// A received F-bearing leaf remains as raw proof after cache rebuild.
			// An aware peer need not send that leaf in the first place.
			if _, ok := content.AvailableByUUID(got.DB(), outsideLeaf); ok != ignorant {
				t.Fatalf("outside leaf held=%v, want %v", ok, ignorant)
			}
			if res.Elided == 0 {
				t.Fatal("sparse tree clone reported no elision")
			}
			nextFile := storeAndLink(t, r, []byte("tree pull "+name))
			nextLeaf := marshal(&deck.Deck{F: []deck.FileCard{{Name: "src/a", UUID: nextFile}}})
			nextSpine := marshal(&deck.Deck{C: "tree pull " + name, D: time.Now().UTC(), U: deck.User("u"), P: []string{spine}, S: []deck.SubmanifestCard{{Name: "src/a", UUID: nextLeaf}, {Name: "zzz/a", UUID: outsideLeaf}}})
			pulled, err := Sync(context.Background(), got, transport, SyncOpts{Pull: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(pulled.Errors) > 0 {
				t.Fatalf("sparse pull errors: %v", pulled.Errors)
			}
			for _, uuid := range []string{nextFile, nextLeaf, nextSpine} {
				if _, ok := content.AvailableByUUID(got.DB(), uuid); !ok {
					t.Fatalf("sparse pull lost %s", uuid)
				}
			}
			if _, ok := content.AvailableByUUID(got.DB(), outsideLeaf); ok != ignorant {
				t.Fatalf("outside leaf after pull held=%v, want %v", ok, ignorant)
			}
		})
	}
}

func TestSparseServerFlatClone(t *testing.T) {
	r := setupSyncTestRepo(t)
	kept := storeAndLink(t, r, []byte("keep me"))
	out := storeAndLink(t, r, []byte("elide me"))
	data, err := (&deck.Deck{C: "flat sparse", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "out/a", UUID: out}, {Name: "src/a", UUID: kept}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	spine := storeAndLink(t, r, data)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cards := []xfer.Card{versionPragma("client-version"), sendSubmanifests(), &xfer.CloneCard{Version: 3, SeqNo: 1}}
	cards = append(cards, conePragmas(c)...)
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, card := range findCards[*xfer.CFileCard](resp) {
		seen[card.UUID] = true
	}
	if seen[out] {
		t.Fatal("server sent out-of-cone file")
	}
	if !seen[kept] || !seen[spine] {
		t.Fatalf("server omitted kept artifacts: %v", seen)
	}
	h := &handler{repo: r, cone: c, ctx: context.Background()}
	if err := h.emitIGots(); err != nil {
		t.Fatal(err)
	}
	for _, card := range findCards[*xfer.IGotCard](&xfer.Message{Cards: h.resp}) {
		if card.UUID == out {
			t.Fatal("ordinary pull advertised out-of-cone content")
		}
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	dense, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards[:3]})
	if err != nil {
		t.Fatal(err)
	}
	// The refusal is page_xfer's: the push card naming the repository, then
	// the error, then the closing comment, and no content.
	if len(findCards[*xfer.ErrorCard](dense)) != 1 || len(findCards[*xfer.FileCard](dense)) != 0 {
		t.Fatalf("sparse origin did not refuse dense clone: %v", dense.Cards)
	}
	if _, ok := dense.Cards[0].(*xfer.PushCard); !ok {
		t.Fatalf("refusal does not lead with the push card: %v", dense.Cards)
	}
}

func TestSparseCloneFromRealFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	path := filepath.Join(t.TempDir(), "dense.fossil")
	if out, err := exec.Command(bin, "new", path).CombinedOutput(); err != nil {
		t.Fatalf("fossil new: %v\n%s", err, out)
	}
	source, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.DB().Exec("UPDATE user SET cap='s' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	inside := storeAndLink(t, source, []byte("inside fossil source"))
	outside := storeAndLink(t, source, []byte("outside fossil source"))
	data, err := (&deck.Deck{C: "fossil sparse", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "src/a", UUID: inside}, {Name: "zzz/a", UUID: outside}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, source, data)
	url := startFossilServer(t, path)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, res, err := Clone(ctx, filepath.Join(t.TempDir(), "sparse.fossil"), &HTTPTransport{URL: url}, CloneOpts{StoreCone: c, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, ok := content.AvailableByUUID(r.DB(), inside); !ok {
		t.Fatal("real fossil clone lost inside file")
	}
	if _, ok := content.AvailableByUUID(r.DB(), outside); ok {
		t.Fatal("real fossil clone retained outside file")
	}
	if res.Elided != 1 {
		t.Fatalf("real fossil clone elided %d, want 1", res.Elided)
	}
}

func TestConePragmasBounds(t *testing.T) {
	for _, cards := range [][]xfer.Card{
		{&xfer.PragmaCard{Name: "cone", Values: []string{"../escape"}}},
		{&xfer.PragmaCard{Name: "cone", Values: []string{strings.Repeat("a", cone.MaxMemberLen+1)}}},
		{&xfer.PragmaCard{Name: "cone-not", Values: []string{"src"}}},
		{&xfer.PragmaCard{Name: "cone", Values: []string{"src", "extra"}}},
	} {
		if _, err := requestCone(cards); err == nil {
			t.Fatalf("accepted invalid cone %v", cards)
		}
	}
	cards := make([]xfer.Card, cone.MaxMembers+1)
	for i := range cards {
		cards[i] = &xfer.PragmaCard{Name: "cone", Values: []string{"src"}}
	}
	if _, err := requestCone(cards); err == nil {
		t.Fatal("accepted excessive cone members")
	}
	c, err := requestCone([]xfer.Card{&xfer.PragmaCard{Name: "unknown", Values: []string{"anything"}}})
	if err != nil || c != nil {
		t.Fatalf("unknown pragma: cone=%v err=%v", c, err)
	}
}
