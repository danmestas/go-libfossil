package sync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestSparseReceiveProvenanceAfterRebuild(t *testing.T) {
	for _, ignorant := range []bool{false, true} {
		name := "aware"
		if ignorant {
			name = "ignorant"
		}
		t.Run(name, func(t *testing.T) {
			source := setupSyncTestRepo(t)
			put := func(d *deck.Deck) string {
				t.Helper()
				data, err := d.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				return storeAndLink(t, source, data)
			}
			inside := storeAndLink(t, source, []byte("rebuild inside"))
			outside := storeAndLink(t, source, []byte("rebuild outside"))
			inLeaf := put(&deck.Deck{F: []deck.FileCard{{Name: "src/a", UUID: inside}}})
			outLeaf := put(&deck.Deck{F: []deck.FileCard{{Name: "zzz/a", UUID: outside}}})
			outNode := put(&deck.Deck{S: []deck.SubmanifestCard{{Name: "zzz/a", UUID: outLeaf}}})
			missing := strings.Repeat("a", 40)
			missingChild := put(&deck.Deck{S: []deck.SubmanifestCard{{Name: "zzzz/a", UUID: missing}}})
			missingParent := put(&deck.Deck{S: []deck.SubmanifestCard{{Name: "zzzz/a", UUID: missingChild}}})
			root := put(&deck.Deck{C: "partial receive", D: time.Now().UTC(), U: deck.User("u"), S: []deck.SubmanifestCard{
				{Name: "src/a", UUID: inLeaf}, {Name: "zzz/a", UUID: outNode}, {Name: "zzzz/a", UUID: missingParent},
			}})
			c, err := cone.New([]string{"src"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			gimmes := map[string]bool{}
			transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
				var cards []xfer.Card
				for _, card := range req.Cards {
					if g, ok := card.(*xfer.GimmeCard); ok {
						gimmes[g.UUID] = true
					}
					if p, ok := card.(*xfer.PragmaCard); ignorant && ok && (p.Name == "cone" || p.Name == "cone-not") {
						continue
					}
					cards = append(cards, card)
				}
				resp, err := HandleSync(context.Background(), source, &xfer.Message{Cards: cards})
				if err != nil {
					t.Fatal(err)
				}
				return resp
			}}
			r, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "partial.fossil"), transport, CloneOpts{StoreCone: c, PullPhantoms: true})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			// Pull a new spine before clearing caches, exercising both receive paths.
			next := storeAndLink(t, source, []byte("rebuild next inside"))
			nextLeaf := put(&deck.Deck{F: []deck.FileCard{{Name: "src/a", UUID: next}}})
			nextOutside := storeAndLink(t, source, []byte("rebuild next outside"))
			nextOutLeaf := put(&deck.Deck{F: []deck.FileCard{{Name: "zzz/b", UUID: nextOutside}}})
			nextOutNode := put(&deck.Deck{S: []deck.SubmanifestCard{{Name: "zzz/a", UUID: outLeaf}, {Name: "zzz/b", UUID: nextOutLeaf}}})
			put(&deck.Deck{C: "partial pull", D: time.Now().UTC(), U: deck.User("u"), P: []string{root}, S: []deck.SubmanifestCard{
				{Name: "src/a", UUID: nextLeaf}, {Name: "zzz/a", UUID: nextOutNode}, {Name: "zzzz/a", UUID: missingParent},
			}})
			if res, err := Sync(context.Background(), r, transport, SyncOpts{Pull: true}); err != nil || len(res.Errors) != 0 {
				t.Fatalf("initial pull: %v, %v", res, err)
			}
			if ignorant {
				for _, uuid := range []string{outLeaf, nextOutLeaf, missingChild, missingParent} {
					if _, ok := content.AvailableByUUID(r.DB(), uuid); !ok {
						t.Fatalf("lost raw provenance node %s", uuid)
					}
				}
			}
			for _, uuid := range []string{outside, outNode, nextOutside, nextOutNode} {
				if _, ok := content.AvailableByUUID(r.DB(), uuid); ok {
					t.Fatalf("retained reclaimable outside artifact %s", uuid)
				}
			}
			// A stale disposable link must not be needed to reconstruct elision.
			if _, err := r.DB().Exec("INSERT INTO mlink(mid,fid,fnid) VALUES(999999,0,0)"); err != nil {
				t.Fatal(err)
			}
			rebuilt, err := verify.Rebuild(r)
			if err != nil {
				t.Fatal(err)
			}
			if !rebuilt.OK() || rebuilt.Phantoms != 0 || rebuilt.Elided == 0 {
				t.Fatalf("rebuild report: %+v", rebuilt)
			}
			var stale int
			if err := r.DB().QueryRow("SELECT count(*) FROM mlink WHERE mid=999999").Scan(&stale); err != nil || stale != 0 {
				t.Fatalf("rebuild retained stale mlink: %d, %v", stale, err)
			}
			for round := 0; round < 2; round++ {
				gimmes = map[string]bool{}
				res, err := Sync(context.Background(), r, transport, SyncOpts{Pull: true})
				if err != nil || len(res.Errors) != 0 {
					t.Fatalf("post-rebuild pull %d: %v, %v", round, res, err)
				}
				for _, uuid := range []string{outside, outLeaf, outNode, nextOutside, nextOutLeaf, nextOutNode, missing, missingChild, missingParent} {
					if gimmes[uuid] {
						t.Fatalf("requested outside artifact after rebuild: %s", uuid)
					}
				}
				a, err := content.LoadAbsencesContext(context.Background(), r.DB())
				if err != nil {
					t.Fatal(err)
				}
				if a.Count(content.Transit) != 0 {
					t.Fatalf("post-rebuild transit phantoms=%d", a.Count(content.Transit))
				}
				elided := []string{outNode, nextOutNode}
				if ignorant {
					elided = append(elided, outside, nextOutside, missing)
				} else {
					elided = append(elided, missingParent)
				}
				for _, uuid := range elided {
					var rid libfossil.FslID
					if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
						t.Fatal(err)
					}
					if a.Class(rid) != content.Elided {
						t.Fatalf("outside node %s is not elided", uuid)
					}
				}
				if _, ok := content.AvailableByUUID(r.DB(), next); !ok {
					t.Fatal("lost newly pulled covered payload")
				}
			}
		})
	}
}
