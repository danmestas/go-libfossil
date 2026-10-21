package checkout

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

func TestSparseCheckoutOpensWithElidedSubtree(t *testing.T) {
	f := newSparseRepo(t, true)
	d, err := manifest.GetManifest(f.repo, f.c1)
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.ListFiles(f.repo, f.c1)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{files[0].Name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.repo.DB(), c); err != nil {
		t.Fatal(err)
	}
	if len(d.S) < 2 {
		t.Fatal("fixture needs at least two leaves")
	}
	outside := d.S[len(d.S)-1].UUID
	for _, query := range []string{
		"UPDATE blob SET size=-1,content=NULL WHERE uuid=?",
		"INSERT INTO phantom(rid) SELECT rid FROM blob WHERE uuid=?",
	} {
		if _, err := f.repo.DB().Exec(query, outside); err != nil {
			t.Fatal(err)
		}
	}
	co := f.open(t, c, f.c1)
	got := onDisk(t, co)
	if len(got) != 1 || got[0] != files[0].Name {
		t.Fatalf("materialized %v, want %s only", got, files[0].Name)
	}
	if _, err := manifest.ListFiles(f.repo, f.c1); err == nil {
		t.Fatal("dense enumeration accepted missing subtree")
	}
	if dense, err := Create(f.repo, t.TempDir(), CreateOpts{}); err == nil {
		dense.Close()
		t.Fatal("dense checkout silently accepted sparse file set")
	}
}
