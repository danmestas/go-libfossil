package sync

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

func TestSparseReceiveRetainsProtectedHoldings(t *testing.T) {
	r := setupSyncTestRepo(t)
	uuids := map[string]string{}
	for _, name := range []string{"corrupt", "existing", "private", "structural", "unsent", "wanted-drop"} {
		uuids[name] = storeAndLink(t, r, []byte("receive safety "+name))
	}
	var files []deck.FileCard
	for _, name := range []string{"corrupt", "existing", "private", "structural", "unsent", "wanted-drop"} {
		files = append(files, deck.FileCard{Name: "zzz/" + name, UUID: uuids[name]})
	}
	data, err := (&deck.Deck{C: "protected receive", D: time.Now().UTC(), U: deck.User("u"), F: files, P: []string{uuids["structural"]}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, r, data)
	unknown := storeAndLink(t, r, []byte("unreferenced ordinary bytes"))
	for _, table := range []string{"private", "unsent"} {
		if _, err := r.DB().Exec("INSERT OR IGNORE INTO "+table+"(rid) SELECT rid FROM blob WHERE uuid=?", uuids[table]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.DB().Exec("UPDATE blob SET content=x'00' WHERE uuid=?", uuids["corrupt"]); err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	received := map[string]bool{unknown: true}
	for name, uuid := range uuids {
		if name != "existing" {
			received[uuid] = true
		}
	}
	elided := 0
	if err := r.WithTx(func(tx *db.Tx) error {
		var err error
		elided, err = filterSparseReceived(context.Background(), tx, received, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if elided != 1 {
		t.Fatalf("final elision count=%d, want only the one demoted payload", elided)
	}
	for name, uuid := range uuids {
		_, held := content.AvailableByUUID(r.DB(), uuid)
		if held != (name != "wanted-drop") {
			t.Fatalf("%s held=%v", name, held)
		}
	}
	if _, held := content.AvailableByUUID(r.DB(), unknown); !held {
		t.Fatal("unknown artifact discarded")
	}
}

type sparseCancelAfterDemotion struct {
	db.Querier
	cancel  context.CancelFunc
	demoted bool
}

func (q *sparseCancelAfterDemotion) Exec(query string, args ...any) (sql.Result, error) {
	res, err := q.Querier.Exec(query, args...)
	if err == nil && strings.HasPrefix(query, "UPDATE blob SET size=-1, content=NULL") {
		q.demoted = true
		q.cancel()
	}
	return res, err
}

func TestSparseReceiveCancellationRollsBackDemotion(t *testing.T) {
	r := setupSyncTestRepo(t)
	first := storeAndLink(t, r, []byte("first rollback payload"))
	second := storeAndLink(t, r, []byte("second rollback payload"))
	data, err := (&deck.Deck{C: "rollback receive", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{
		{Name: "zzz/a", UUID: first}, {Name: "zzz/b", UUID: second},
	}}).Marshal()
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var demoted bool
	err = r.WithTx(func(tx *db.Tx) error {
		q := &sparseCancelAfterDemotion{Querier: tx, cancel: cancel}
		_, err := filterSparseReceived(ctx, q, map[string]bool{first: true, second: true}, nil)
		demoted = q.demoted
		return err
	})
	if !demoted || !errors.Is(err, context.Canceled) {
		t.Fatalf("demoted=%v, err=%v; want actual mutation followed by cancellation", demoted, err)
	}
	for _, uuid := range []string{first, second} {
		rid, held := content.AvailableByUUID(r.DB(), uuid)
		if !held {
			t.Fatalf("canceled receive lost %s", uuid)
		}
		if _, err := content.Expand(r.DB(), rid); err != nil {
			t.Fatal(err)
		}
		var phantoms int
		if err := r.DB().QueryRow("SELECT count(*) FROM phantom WHERE rid=?", rid).Scan(&phantoms); err != nil || phantoms != 0 {
			t.Fatalf("canceled receive phantom=%d, err=%v", phantoms, err)
		}
	}
}
