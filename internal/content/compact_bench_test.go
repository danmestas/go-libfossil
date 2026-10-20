package content

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
)

// BenchmarkCompactStore measures reclamation on an isolated 256-file, 1 MiB
// store copy. Fixture restoration is outside the timed compaction interval.
func BenchmarkCompactStore(b *testing.B) {
	d, err := db.Open(filepath.Join(b.TempDir(), "compact.fossil"))
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	if err := db.CreateRepoSchema(d); err != nil {
		b.Fatal(err)
	}
	var files []deck.FileCard
	err = d.WithTx(func(tx *db.Tx) error {
		for i := 0; i < 256; i++ {
			prefix := "docs"
			if i < 128 {
				prefix = "src"
			}
			data := []byte(fmt.Sprintf("%04d\n", i) + strings.Repeat("representative file content line\n", 128))
			_, uuid, err := blob.Store(tx, data)
			if err != nil {
				return err
			}
			files = append(files, deck.FileCard{Name: fmt.Sprintf("%s/%04d", prefix, i), UUID: uuid})
		}
		slices.SortFunc(files, func(a, b deck.FileCard) int { return strings.Compare(a.Name, b.Name) })
		data, err := (&deck.Deck{Type: deck.Checkin, D: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), F: files}).Marshal()
		if err != nil {
			return err
		}
		_, _, err = blob.Store(tx, data)
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := d.Exec("CREATE TABLE compact_restore AS SELECT * FROM blob"); err != nil {
		b.Fatal(err)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		b.Fatal(err)
	}
	if err := SetStoreCone(d, c); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(256 * 4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := CompactStore(context.Background(), d)
		if err != nil || n != 128 {
			b.Fatalf("compact = %d, %v", n, err)
		}
		b.StopTimer()
		err = d.WithTx(func(tx *db.Tx) error {
			if _, err := tx.Exec("UPDATE blob SET size=r.size, content=r.content FROM compact_restore r WHERE blob.rid=r.rid"); err != nil {
				return err
			}
			_, err := tx.Exec("DELETE FROM phantom")
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}
