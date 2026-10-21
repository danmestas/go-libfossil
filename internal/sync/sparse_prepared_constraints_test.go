package sync

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestSparseObserverOwnsConnection(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().SqlDB().SetMaxOpenConns(1)
	ctx := context.Background()
	observer, err := newSparseObserver(ctx, r.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := observer.Close(); err != nil {
			t.Error(err)
		}
	})
	before, err := observer.version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.DB().SqlDB().Stats().InUse != 0 {
		t.Fatal("observer borrowed a repository pool connection")
	}
	if err := r.WithBoundTx(func(bound *repo.Repo) error {
		_, err := bound.DB().Exec("INSERT INTO config(name,value) VALUES('observer-local-write','one')")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	after, err := observer.version(ctx)
	if err != nil || after == before {
		t.Fatalf("own-pool commit: before=%d after=%d err=%v", before, after, err)
	}
	foreign, err := db.Open(r.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if _, err := foreign.Exec("UPDATE config SET value='two' WHERE name='observer-local-write'"); err != nil {
		t.Fatal(err)
	}
	foreignVersion, err := observer.version(ctx)
	if err != nil || foreignVersion == after {
		t.Fatalf("foreign commit: before=%d after=%d err=%v", after, foreignVersion, err)
	}
}

// Both shipped drivers use BEGIN IMMEDIATE for WithBoundTx. A prepared
// generation checked inside that transaction cannot race a foreign commit;
// the commit must instead invalidate the generation before a later request.
func TestSparseObserverBoundSnapshotExcludesForeignCommit(t *testing.T) {
	r := setupSyncTestRepo(t)
	if _, err := r.DB().Exec("INSERT INTO config(name,value) VALUES('observer-snapshot','old')"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observer, err := newSparseObserver(ctx, r.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	foreign, err := db.Open(r.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	writer, err := foreign.SqlDB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(ctx, "PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	before, err := observer.version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WithBoundTx(func(bound *repo.Repo) error {
		var old string
		if err := bound.DB().QueryRow("SELECT value FROM config WHERE name='observer-snapshot'").Scan(&old); err != nil {
			return err
		}
		_, err := writer.ExecContext(ctx, "UPDATE config SET value='new' WHERE name='observer-snapshot'")
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "locked") && !strings.Contains(strings.ToLower(err.Error()), "busy") {
			t.Fatalf("foreign update inside bound snapshot: %v, want locked/busy", err)
		}
		after, err := observer.version(ctx)
		if err != nil {
			return err
		}
		if after != before {
			t.Fatal("blocked foreign write changed the observer generation")
		}
		var stillOld string
		if err := bound.DB().QueryRow("SELECT value FROM config WHERE name='observer-snapshot'").Scan(&stillOld); err != nil {
			return err
		}
		if old != "old" || stillOld != "old" {
			t.Fatalf("read transaction did not retain its snapshot: %q -> %q", old, stillOld)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, "UPDATE config SET value='new' WHERE name='observer-snapshot'"); err != nil {
		t.Fatal(err)
	}
	after, err := observer.version(ctx)
	if err != nil || after == before {
		t.Fatalf("post-snapshot commit: before=%d after=%d err=%v", before, after, err)
	}
}

// This establishes durability of the existing cluster machinery, not a
// prepared server's inventory budget or snapshot-invalidation contract.
func TestSparsePreparedClusterSurvivesRestart(t *testing.T) {
	r := setupSyncTestRepo(t)
	member := storeTestBlob(t, r, []byte("prepared cluster member"))
	cluster := &deck.Deck{M: []string{member}}
	data, err := cluster.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var uuid string
	if err := r.WithTx(func(tx *db.Tx) error {
		rid, name, err := blob.Store(tx, data)
		if err != nil {
			return err
		}
		uuid = name
		return manifest.CrosslinkCluster(tx, rid, cluster)
	}); err != nil {
		t.Fatal(err)
	}
	pull := &xfer.PullCard{ServerCode: "test", ProjectCode: testPC}
	resp := sparseConstraintRound(t, r, versionPragma("client-version"), pull)
	announced := false
	for _, card := range resp.Cards {
		if c, ok := card.(*xfer.IGotCard); ok && c.UUID == uuid {
			announced = true
		}
	}
	if !announced {
		t.Fatal("durable cluster root was not advertised")
	}
	storeTestBlob(t, r, []byte("generation change after IGOT"))
	path := r.Path()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resp = sparseConstraintRound(t, reopened, versionPragma("client-version"), pull, &xfer.GimmeCard{UUID: uuid})
	for _, card := range resp.Cards {
		if c, ok := card.(*xfer.FileCard); ok && c.UUID == uuid {
			if hash.SHA3(c.Content) != uuid || string(c.Content) != string(data) {
				t.Fatal("reopened cluster differs from advertised content hash")
			}
			return
		}
	}
	for _, card := range resp.Cards {
		t.Logf("reply card %T: %+v", card, card)
	}
	t.Fatal("advertised cluster became unavailable after generation change and restart")
}

func sparseConstraintRound(t *testing.T, r *repo.Repo, cards ...xfer.Card) *xfer.Message {
	t.Helper()
	wire, err := (&xfer.Message{Cards: cards}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	req, err := xfer.Decode(wire, xfer.ContentTypeCompressed)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatal(err)
	}
	wire, err = resp.Encode()
	if err != nil {
		t.Fatal(err)
	}
	resp, err = xfer.Decode(wire, xfer.ContentTypeCompressed)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
