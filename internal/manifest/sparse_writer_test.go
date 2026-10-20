package manifest

import (
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// MAX cuts depend on the number of preceding entries, not just the next
// filename. An insertion can change a disjoint subtree's canonical bytes.
func TestSparseWriterCanonicalBoundaryPropagation(t *testing.T) {
	var candidates []deck.FileCard
	for i := 0; i < 256 && len(candidates) < 18; i++ {
		name := fmt.Sprintf("f%04d", i)
		if zeroBits(name) == 0 {
			candidates = append(candidates, deck.FileCard{Name: name, UUID: hash.SHA3([]byte(name))})
		}
	}
	if len(candidates) != 18 {
		t.Fatal("bounded search found too few non-boundary names")
	}
	inserted := candidates[1]
	t.Logf("insert %s into first old range", inserted.Name)
	oldFiles := append([]deck.FileCard{candidates[0]}, candidates[2:]...)
	oldTree := mustBuildTree(t, oldFiles, 1)
	newTree := mustBuildTree(t, candidates, 1)
	if len(oldTree.S) != 3 || len(newTree.S) != 3 {
		t.Fatalf("S counts old=%d new=%d, want three leaves", len(oldTree.S), len(newTree.S))
	}
	if deck.Compare(inserted.Name, oldTree.S[1].Name) >= 0 {
		t.Fatal("insertion is not confined to the first old subtree")
	}
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "1")
	var files []File
	for _, f := range oldFiles {
		files = append(files, File{Name: f.Name, Content: []byte(f.Name)})
	}
	rid, parent := commit(t, r, CheckinOpts{Files: files})
	if len(parent.S) != len(oldTree.S) {
		t.Fatalf("parent S count %d, want %d", len(parent.S), len(oldTree.S))
	}
	var included []string
	for _, f := range candidates {
		if deck.Compare(f.Name, oldTree.S[1].Name) < 0 {
			included = append(included, f.Name)
		}
	}
	c, err := cone.New(included, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(oldTree.S); i++ {
		if parent.S[i] != oldTree.S[i] {
			t.Fatalf("stored parent differs from canonical tree at %d", i)
		}
		for _, sql := range []string{
			"UPDATE blob SET size=-1, content=NULL WHERE uuid=?",
			"INSERT INTO phantom(rid) SELECT rid FROM blob WHERE uuid=?",
		} {
			if _, err := r.DB().Exec(sql, parent.S[i].UUID); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok := content.AvailableByUUID(r.DB(), parent.S[i].UUID); ok {
			t.Fatal("out-of-cone submanifest was not actually removed")
		}
		for _, next := range newTree.S {
			if next.UUID == oldTree.S[i].UUID {
				t.Fatalf("old untouched subtree %s unexpectedly survived the canonical insertion", oldTree.S[i].Name)
			}
		}
		oldLeaf, err := deck.Parse(oldTree.Subs[i])
		if err != nil {
			t.Fatal(err)
		}
		newLeaf, err := deck.Parse(newTree.Subs[i])
		if err != nil {
			t.Fatal(err)
		}
		oldSize, newSize := 8, 8
		if i == 2 {
			oldSize, newSize = 1, 2
		}
		if len(oldLeaf.F) != oldSize || len(newLeaf.F) != newSize {
			t.Fatalf("leaf %d sizes old=%d new=%d, want %d/%d", i, len(oldLeaf.F), len(newLeaf.F), oldSize, newSize)
		}
		t.Logf("untouched old range %s: %d entries, hash %s; canonical replacement starts %s: %d entries, hash %s",
			oldTree.S[i].Name, len(oldLeaf.F), oldTree.S[i].UUID,
			newTree.S[i].Name, len(newLeaf.F), newTree.S[i].UUID)
	}
	visible, err := ListFilesFiltered(r, rid, c)
	if err != nil || len(visible) != 8 {
		t.Fatalf("in-cone enumeration after eliding S nodes = %d files, %v; want 8, nil", len(visible), err)
	}
	if _, err := ListFiles(r, rid); err == nil {
		t.Fatal("full enumeration unexpectedly succeeded without out-of-cone S nodes")
	} else if uuid, ok := MissingSubmanifest(err); !ok || uuid != parent.S[1].UUID {
		t.Fatalf("full enumeration error = %v; want missing %s", err, parent.S[1].UUID)
	}
	_, hi := partitionLimits(1)
	if len(included) != hi+1 {
		t.Fatalf("preserving old sibling hashes leaves %d in-cone entries, want MAX+1=%d", len(included), hi+1)
	}
}
