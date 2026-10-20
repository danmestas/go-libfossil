package manifest

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

func TestSparseTreeMetadataCompletesAfterAllNodesArrive(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "reverse"}[reverse], func(t *testing.T) {
			f := newTreeFixture(t)
			refs := []deck.SubmanifestCard{f.leaf(f.file("src/a")), f.leaf(f.file("vendor/a")), f.leaf(f.file("z/a"))}
			root := treeDeck(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), refs)
			root.T = []deck.TagCard{{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}}
			uuid := f.store(root)
			rid, _ := content.AvailableByUUID(f.r.DB(), uuid)
			var payloads [][]byte
			for _, ref := range refs[1:] {
				node, _ := content.AvailableByUUID(f.r.DB(), ref.UUID)
				data, err := content.Expand(f.r.DB(), node)
				if err != nil {
					t.Fatal(err)
				}
				payloads = append(payloads, data)
				elideWriterNode(t, f.r, ref.UUID)
			}
			c, err := cone.New([]string{"src"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := content.SetStoreCone(f.r.DB(), c); err != nil {
				t.Fatal(err)
			}
			if _, err := Crosslink(f.r); err != nil {
				t.Fatal(err)
			}
			assertSparseRows(t, f, int64(rid), 1, 0, 1)
			if _, err := FileHistory(f.r, FileHistoryOpts{Path: "src/a"}); err == nil || errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("partial file history = %v; want incomplete derivation, not absence", err)
			}
			if err := f.r.WithTx(func(tx *db.Tx) error {
				replayed, err := parsedArtifactReplayed(tx, rid, root)
				if replayed {
					t.Error("partial metadata falsely proves full receive completion")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := Crosslink(f.r); err != nil || n != 0 {
				t.Fatalf("unchanged partial sweep = %d, %v; want no retry", n, err)
			}
			for i := 0; i < 2; i++ {
				j := i
				if reverse {
					j = 1 - i
				}
				f.storeBytes(payloads[j])
				if _, err := Crosslink(f.r); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					assertSparseRows(t, f, int64(rid), 1, 0, 1)
				}
			}
			assertSparseRows(t, f, int64(rid), 1, 3, 0)
			if history, err := FileHistory(f.r, FileHistoryOpts{Path: "src/a"}); err != nil || len(history) != 1 {
				t.Fatalf("completed file history = %+v, %v", history, err)
			}
			if _, err := Crosslink(f.r); err != nil {
				t.Fatal(err)
			}
			assertSparseRows(t, f, int64(rid), 1, 3, 0)
		})
	}
}

func assertSparseRows(t *testing.T, f *treeFixture, rid int64, event, mlink, orphan int) {
	t.Helper()
	for _, check := range []struct {
		query string
		want  int
	}{
		{"SELECT count(*) FROM event WHERE objid=?", event},
		{"SELECT count(*) FROM mlink WHERE mid=?", mlink},
		{"SELECT count(*) FROM orphan WHERE rid=?", orphan},
		{"SELECT count(*) FROM tagxref x JOIN tag t ON t.tagid=x.tagid WHERE x.rid=? AND t.tagname='branch' AND x.value='trunk'", 1},
		{"SELECT count(*) FROM leaf WHERE rid=?", 1},
	} {
		var got int
		if err := f.r.DB().QueryRow(check.query, rid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != check.want {
			t.Fatalf("%s = %d, want %d", check.query, got, check.want)
		}
	}
}
