package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

type sparseCheckoutClone struct {
	source, clone              *repo.Repo
	cone                       *cone.Cone
	root, tip, inside, outside string
}

func newSparseCheckoutClone(t *testing.T) sparseCheckoutClone {
	t.Helper()
	r := setupSyncTestRepo(t)
	put := func(d *deck.Deck) string {
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return storeAndLink(t, r, data)
	}
	inside := storeAndLink(t, r, []byte("covered\n"))
	outside := storeAndLink(t, r, []byte("outside\n"))
	left := put(&deck.Deck{F: []deck.FileCard{{Name: "src/a", UUID: inside}}})
	right := put(&deck.Deck{F: []deck.FileCard{{Name: "vendor/a", UUID: outside}}})
	spine := put(&deck.Deck{C: "sparse root", D: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), U: deck.User("u"),
		T: []deck.TagCard{{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}},
		S: []deck.SubmanifestCard{{Name: "src/a", UUID: left}, {Name: "vendor/a", UUID: right}}})
	tip := put(&deck.Deck{C: "sparse child", D: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), U: deck.User("u"), P: []string{spine},
		S: []deck.SubmanifestCard{{Name: "src/a", UUID: left}, {Name: "vendor/a", UUID: right}}})
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), r, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}}
	got, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"), transport, CloneOpts{StoreCone: c, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { got.Close() })
	return sparseCheckoutClone{source: r, clone: got, cone: c, root: spine, tip: tip, inside: left, outside: right}
}

func TestSparseTreeCloneMetadataAndCheckout(t *testing.T) {
	f := newSparseCheckoutClone(t)
	got := f.clone
	if _, ok := content.AvailableByUUID(got.DB(), f.outside); ok {
		t.Fatal("outside S was not actually elided")
	}
	rid, ok := content.AvailableByUUID(got.DB(), f.tip)
	if !ok {
		t.Fatal("clone lost spine")
	}
	log, err := manifest.Log(got, manifest.LogOpts{Start: rid})
	if err != nil || len(log) != 2 || log[0].UUID != f.tip || log[1].UUID != f.root {
		t.Fatalf("metadata history = %+v, %v", log, err)
	}
	assertSparseCloneHealthy(t, got)
	if _, err := verify.Rebuild(got); err != nil {
		t.Fatal(err)
	}
	assertSparseCloneHealthy(t, got)
	var n int
	if err := got.DB().QueryRow("SELECT count(*) FROM mlink WHERE mid=?", rid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("partial clone derived %d mlinks", n)
	}
	co, err := checkout.Create(got, t.TempDir(), checkout.CreateOpts{Cone: f.cone})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if err := co.Extract(rid, checkout.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(co.Dir(), "src", "a")); err != nil || string(data) != "covered\n" {
		t.Fatalf("covered file = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), "vendor")); !os.IsNotExist(err) {
		t.Fatalf("outside checkout directory: %v", err)
	}
	f.fill(t, f.outside)
	if _, err := manifest.Crosslink(got); err != nil {
		t.Fatal(err)
	}
	if err := got.DB().QueryRow("SELECT count(*) FROM orphan").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("completed clone retains %d completion gaps", n)
	}
	assertSparseCloneHealthy(t, got)
	if _, err := verify.Rebuild(got); err != nil {
		t.Fatal(err)
	}
	assertSparseCloneHealthy(t, got)
}

func assertSparseCloneHealthy(t *testing.T, r *repo.Repo) {
	t.Helper()
	if report, err := verify.Verify(r); err != nil || !report.OK() {
		t.Fatalf("healthy partial clone verification = %+v, %v", report, err)
	}
}

func (f sparseCheckoutClone) fill(t *testing.T, uuid string) {
	t.Helper()
	rid, ok := content.AvailableByUUID(f.source.DB(), uuid)
	if !ok {
		t.Fatal("source lost node")
	}
	data, err := content.Expand(f.source.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, f.clone, data)
}

func (f sparseCheckoutClone) elide(t *testing.T, uuid string) {
	t.Helper()
	for _, query := range []string{"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", "INSERT INTO phantom(rid) SELECT rid FROM blob WHERE uuid=?"} {
		if _, err := f.clone.DB().Exec(query, uuid); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSparseCloneVerifyRejectsRequiredGaps(t *testing.T) {
	for _, kind := range []string{"covered", "covered-payload", "unknown", "structural-alias"} {
		t.Run(kind, func(t *testing.T) {
			f := newSparseCheckoutClone(t)
			if kind == "covered" {
				f.elide(t, f.inside)
			} else if kind == "covered-payload" {
				rid, _ := content.AvailableByUUID(f.source.DB(), f.inside)
				data, err := content.Expand(f.source.DB(), rid)
				if err != nil {
					t.Fatal(err)
				}
				d, err := deck.Parse(data)
				if err != nil {
					t.Fatal(err)
				}
				f.elide(t, d.F[0].UUID)
			} else if kind == "unknown" {
				if err := content.SetStoreCone(f.clone.DB(), nil); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := (&deck.Deck{D: time.Now().UTC(), U: deck.User("u"),
					T: []deck.TagCard{{Type: deck.TagSingleton, Name: "sym-structural", UUID: f.outside}}}).Marshal()
				if err != nil {
					t.Fatal(err)
				}
				storeAndLink(t, f.clone, data)
			}
			report, err := verify.Verify(f.clone)
			if err != nil {
				t.Fatal(err)
			}
			if report.OK() || report.MissingRefs == 0 || report.Phantoms == 0 {
				t.Fatalf("required gap was hidden: %+v", report)
			}
		})
	}
}
