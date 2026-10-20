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
	compactDeck(t, d, &deck.Deck{Type: deck.Checkin, D: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), F: []deck.FileCard{{Name: "docs/base", UUID: baseName}, {Name: "docs/drop", UUID: dropName}, {Name: "docs/middle", UUID: middleName}, {Name: "src/target", UUID: targetName}}})
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
	cancel context.CancelFunc
	polls  int
}

func (c *compactCancelContext) Err() error {
	c.polls++
	if c.polls == 6 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCompactStoreCancellationRollsBackWrites(t *testing.T) {
	d, out, _, _ := compactFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polled := &compactCancelContext{Context: ctx, cancel: cancel}
	if n, err := CompactStore(polled, d); !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("cancel after writes = %d, %v", n, err)
	}
	if polled.polls != 6 {
		t.Fatalf("polls = %d, want 6", polled.polls)
	}
	if _, err := Expand(d, out); err != nil {
		t.Fatal("cancel lost content", err)
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
