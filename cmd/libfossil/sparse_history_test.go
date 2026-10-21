package main

import (
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

func TestLogPartialTreeRefusesIncompleteHistory(t *testing.T) {
	path := newSnapshotRepo(t)
	stdout, stderr, code := runCLI(t, "-R", path, "repo", "log", "--path", "missing")
	if code != exitNotFound {
		t.Fatalf("dense unknown history exit=%d stdout=%q stderr=%q, want %d", code, stdout, stderr, exitNotFound)
	}
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	put := func(data []byte) string {
		_, uuid, err := blob.Store(r.DB(), data)
		if err != nil {
			t.Fatal(err)
		}
		return uuid
	}
	putDeck := func(d *deck.Deck) string {
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return put(data)
	}
	inside := put([]byte("inside"))
	outside := put([]byte("outside"))
	left := putDeck(&deck.Deck{F: []deck.FileCard{{Name: "src/a", UUID: inside}}})
	right := putDeck(&deck.Deck{F: []deck.FileCard{{Name: "vendor/a", UUID: outside}}})
	putDeck(&deck.Deck{C: "partial", D: time.Now().UTC(), U: deck.User("tester"),
		S: []deck.SubmanifestCard{{Name: "src/a", UUID: left}, {Name: "vendor/a", UUID: right}}})
	for _, query := range []string{"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", "INSERT INTO phantom(rid) SELECT rid FROM blob WHERE uuid=?"} {
		if _, err := r.DB().Exec(query, right); err != nil {
			t.Fatal(err)
		}
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runCLI(t, "-R", path, "repo", "log", "--json", "--path", "src/a")
	if code == 0 || code == exitNotFound || strings.TrimSpace(stdout) != "" || !strings.Contains(stderr, "derivation is incomplete") {
		t.Fatalf("partial history exit=%d stdout=%q stderr=%q; want incomplete error, not absence", code, stdout, stderr)
	}
}
