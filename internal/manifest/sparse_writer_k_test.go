package manifest

import (
	"reflect"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// A manifest carries subtree names, not the k that constructed them. Current
// mutable settings cannot certify that inherited shape is canonical today.
func TestSparseWriterCurrentPinDoesNotProveParentPartition(t *testing.T) {
	cards := sparseBoundaryCandidates(t)[:17]
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "1")
	var files []File
	for _, f := range cards {
		files = append(files, File{Name: f.Name, Content: []byte(f.Name)})
	}
	rid, parent := commit(t, r, CheckinOpts{Files: files})
	if len(parent.S) != 3 {
		t.Fatalf("parent at k=1 has %d S cards, want three", len(parent.S))
	}
	if err := RetuneSubmanifests(r, 2); err != nil {
		t.Fatal(err)
	}
	settings, err := readSubmanifestSettings(r.DB())
	if err != nil || settings.k != 2 {
		t.Fatalf("current settings k=%d, err=%v; want 2", settings.k, err)
	}
	c, err := cone.New([]string{cards[0].Name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	for _, s := range parent.S[1:] {
		elideWriterNode(t, r, s.UUID)
	}
	changed := append(cards[:0:0], cards...)
	changed[0].UUID, changed[0].Perm = hash.SHA3([]byte("replacement\n")), "x"
	want := mustBuildTree(t, changed, settings.k)
	if len(want.S) != 2 {
		t.Fatalf("same filenames at k=2 have %d S cards, want two", len(want.S))
	}
	stored, err := GetManifest(r, rid)
	if err != nil || !reflect.DeepEqual(stored.S, parent.S) {
		t.Fatalf("retune unexpectedly rewrote the parent: %v", err)
	}
	if reflect.DeepEqual(stored.S[1:], want.S[1:]) {
		t.Fatal("preserving missing sibling S unexpectedly reproduced the current-k canonical construction")
	}
	t.Log("same filename set: inherited k=1 has leaf sizes 8/8/1; current k=2 requires 16/1")
}
