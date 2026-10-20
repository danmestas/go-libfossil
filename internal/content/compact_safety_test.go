package content

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

func compactDeck(t *testing.T, d *db.DB, artifact *deck.Deck) libfossil.FslID {
	t.Helper()
	data, err := artifact.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	rid, _, err := blob.Store(d, data)
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

func compactName(t *testing.T, d *db.DB, rid libfossil.FslID) string {
	t.Helper()
	var uuid string
	if err := d.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	return uuid
}

func TestCompactStoreSharedHistoryAndControls(t *testing.T) {
	for _, kind := range []string{"shared", "history", "attachment", "tree-alias"} {
		t.Run(kind, func(t *testing.T) {
			d, out, _, _ := compactFixture(t)
			uuid := compactName(t, d, out)
			switch kind {
			case "shared", "history":
				compactDeck(t, d, &deck.Deck{Type: deck.Checkin, D: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), F: []deck.FileCard{{Name: "src/prior-name", UUID: uuid}}})
			case "attachment":
				compactDeck(t, d, &deck.Deck{Type: deck.Attachment, D: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), A: &deck.AttachmentCard{Filename: "a", Target: "wiki", Source: uuid}})
			case "tree-alias":
				node := compactDeck(t, d, &deck.Deck{Type: deck.Submanifest, F: []deck.FileCard{{Name: "src/leaf", UUID: uuid}}})
				compactDeck(t, d, &deck.Deck{Type: deck.Checkin, D: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), S: []deck.SubmanifestCard{{Name: "src/leaf", UUID: compactName(t, d, node)}}})
			}
			n, err := CompactStore(context.Background(), d)
			if err != nil || n != 0 {
				t.Fatalf("compact = %d, %v", n, err)
			}
			if _, err := Expand(d, out); err != nil {
				t.Fatal("shared reference lost", err)
			}
		})
	}
}

func TestCompactStoreDeltaClosure(t *testing.T) {
	for _, retainedBy := range []string{"in-cone", "private", "unsent"} {
		t.Run(retainedBy, func(t *testing.T) { compactDeltaClosure(t, retainedBy) })
	}
}

func compactDeltaClosure(t *testing.T, retainedBy string) {
	t.Helper()
	d := setupTestDB(t)
	baseData := bytes.Repeat([]byte("file content line\n"), 100)
	base, baseName, err := blob.Store(d, baseData)
	if err != nil {
		t.Fatal(err)
	}
	middleData := append(bytes.Clone(baseData), []byte("middle\n")...)
	middle, middleName, err := blob.StoreDelta(d, middleData, base)
	if err != nil {
		t.Fatal(err)
	}
	targetData := append(bytes.Clone(middleData), []byte("target\n")...)
	target, targetName, err := blob.StoreDelta(d, targetData, middle)
	if err != nil {
		t.Fatal(err)
	}
	drop, dropName, err := blob.StoreDelta(d, append(bytes.Clone(baseData), []byte("drop\n")...), base)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := "src/target"
	if retainedBy != "in-cone" {
		targetPath = "docs/target"
		if _, err := d.Exec("INSERT INTO "+retainedBy+"(rid) VALUES(?)", target); err != nil {
			t.Fatal(err)
		}
	}
	compactDeck(t, d, &deck.Deck{Type: deck.Checkin, D: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), F: []deck.FileCard{{Name: "docs/base", UUID: baseName}, {Name: "docs/drop", UUID: dropName}, {Name: "docs/middle", UUID: middleName}, {Name: targetPath, UUID: targetName}}})
	if err := SetStoreCone(d, mustCone(t, []string{"src"}, nil)); err != nil {
		t.Fatal(err)
	}
	if n, err := CompactStore(context.Background(), d); err != nil || n != 1 {
		t.Fatalf("compact = %d, %v", n, err)
	}
	for _, rid := range []libfossil.FslID{base, middle, target} {
		if _, err := Expand(d, rid); err != nil {
			t.Fatal("delta closure lost", err)
		}
	}
	var count int
	if err := d.QueryRow("SELECT count(*) FROM delta WHERE rid=?", drop).Scan(&count); err != nil || count != 0 {
		t.Fatalf("elided delta link = %d, %v", count, err)
	}
}

func TestCompactStoreRejectsCorruptionAndIncompleteSpine(t *testing.T) {
	for _, kind := range []string{"corrupt", "spine", "lost-policy"} {
		t.Run(kind, func(t *testing.T) {
			d, out, _, _ := compactFixture(t)
			switch kind {
			case "corrupt":
				if _, err := d.Exec("UPDATE blob SET content=x'00' WHERE rid=?", out); err != nil {
					t.Fatal(err)
				}
			case "spine":
				compactDeck(t, d, &deck.Deck{Type: deck.Checkin, D: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), S: []deck.SubmanifestCard{{Name: "src/a", UUID: "1111111111111111111111111111111111111111"}}})
			case "lost-policy":
				if _, err := d.Exec("UPDATE storecone SET cone=x'0099'"); err != nil {
					t.Fatal(err)
				}
			}
			n, err := CompactStore(context.Background(), d)
			if kind == "lost-policy" {
				if err != nil || n != 0 {
					t.Fatalf("lost policy = %d, %v", n, err)
				}
			} else if err == nil || n != 0 {
				t.Fatalf("unsafe compact = %d, %v", n, err)
			}
			var size int
			if err := d.QueryRow("SELECT size FROM blob WHERE rid=?", out).Scan(&size); err != nil || size < 0 {
				t.Fatalf("failure dropped content: %d, %v", size, err)
			}
		})
	}
}

type compactCancelContext struct {
	context.Context
	cancel  context.CancelFunc
	db      *db.DB
	rid     libfossil.FslID
	demoted bool
}

func (c *compactCancelContext) Err() error {
	var size int
	if err := c.db.QueryRow("SELECT size FROM blob WHERE rid=?", c.rid).Scan(&size); err != nil {
		return err
	}
	if size == -1 {
		c.demoted = true
		c.cancel()
	}
	return c.Context.Err()
}

func TestCompactStoreCancellationRollsBackWrites(t *testing.T) {
	d, out, _, _ := compactFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The bound connection sees uncommitted demotion. Cancel only after actually
	// observing it, then require compaction's savepoint to restore the payload.
	err := d.WithBoundTx(func(bound *db.DB) error {
		polled := &compactCancelContext{Context: ctx, cancel: cancel, db: bound, rid: out}
		if n, err := CompactStore(polled, bound); !errors.Is(err, context.Canceled) || n != 0 {
			t.Fatalf("cancel after writes = %d, %v", n, err)
		}
		if !polled.demoted {
			t.Fatal("cancelled before observing a demotion")
		}
		if _, err := Expand(bound, out); err != nil {
			t.Fatal("cancel lost content", err)
		}
		var phantoms int
		if err := bound.QueryRow("SELECT count(*) FROM phantom WHERE rid=?", out).Scan(&phantoms); err != nil {
			return err
		}
		if phantoms != 0 {
			t.Fatal("cancel left a phantom behind")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompactStoreRetainsUnpublishedContent(t *testing.T) {
	for _, table := range []string{"unsent", "private"} {
		t.Run(table, func(t *testing.T) {
			d, out, _, _ := compactFixture(t)
			if _, err := d.Exec("INSERT INTO "+table+"(rid) VALUES(?)", out); err != nil {
				t.Fatal(err)
			}
			if n, err := CompactStore(context.Background(), d); err != nil || n != 0 {
				t.Fatalf("unpublished content compacted: %d, %v", n, err)
			}
			if _, err := Expand(d, out); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A reader holds its old WAL snapshot while compaction commits. It sees the
// demotion only after ending that snapshot, never half a blob/phantom rewrite.
func TestCompactStoreReaderSnapshot(t *testing.T) {
	d, out, _, _ := compactFixture(t)
	pool, err := db.OpenSQL(d.Path(), db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	read := func() (int, int) {
		t.Helper()
		var size, phantoms int
		err := conn.QueryRowContext(context.Background(), `SELECT size,
   (SELECT count(*) FROM phantom WHERE rid=?) FROM blob WHERE rid=?`, out, out).Scan(&size, &phantoms)
		if err != nil {
			t.Fatal(err)
		}
		return size, phantoms
	}
	before, phantoms := read()
	if before < 0 || phantoms != 0 {
		t.Fatal("fixture is not held")
	}
	if n, err := CompactStore(context.Background(), d); err != nil || n != 1 {
		t.Fatalf("compact = %d, %v", n, err)
	}
	if size, phantoms := read(); size != before || phantoms != 0 {
		t.Fatalf("snapshot changed: %d, %d", size, phantoms)
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if size, phantoms := read(); size != -1 || phantoms != 1 {
		t.Fatalf("committed demotion = %d, %d", size, phantoms)
	}
}

func TestCompactStoreWriterExclusion(t *testing.T) {
	d, out, _, _ := compactFixture(t)
	other, err := db.OpenWith(d.Path(), db.OpenConfig{Pragmas: map[string]string{"busy_timeout": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := d.WithTx(func(tx *db.Tx) error {
		if n, err := CompactStore(context.Background(), other); err == nil || n != 0 {
			t.Fatalf("writer exclusion = %d, %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Expand(d, out); err != nil {
		t.Fatal("lock failure lost content", err)
	}
}
