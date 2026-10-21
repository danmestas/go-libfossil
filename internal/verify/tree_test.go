package verify_test

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// A tree check-in whose sub-manifest has not arrived is an unresolved
// reference, not damage: Verify reports it as a missing reference, counts it,
// and does not report the event and plink rows the deferred check-in has not
// been given yet (draft-fossil-submanifests-00 §5.2, §7.3).
func TestVerify_TreeWithMissingSubmanifestIsAGap(t *testing.T) {
	r := newTestRepo(t)
	store := func(data []byte) string {
		var uuid string
		if err := r.WithTx(func(tx *db.Tx) error {
			var err error
			_, uuid, err = blob.Store(tx, data)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return uuid
	}
	marshal := func(d *deck.Deck) []byte {
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	fileUUID := store([]byte("a\n"))
	leaf := marshal(&deck.Deck{F: []deck.FileCard{{Name: "a.txt", UUID: fileUUID}}})
	leafUUID := hash.MustNamingFor(r.DB()).New.Hash(leaf) // never stored
	tree := &deck.Deck{C: "tree", D: time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC),
		S: []deck.SubmanifestCard{{Name: "a.txt", UUID: leafUUID}}, U: deck.User("u"),
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagPropagating, Name: "sym-trunk", UUID: "*"},
		}}
	treeUUID := store(marshal(tree))
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatal(err)
	}

	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	var gaps int
	for _, issue := range report.Issues {
		switch issue.Kind {
		case verify.IssueMissingReference:
			if issue.UUID != treeUUID {
				t.Errorf("missing reference reported for %s, want the tree %s", issue.UUID, treeUUID)
			}
			gaps++
		case verify.IssueEventMissing, verify.IssuePlinkMissing:
			t.Errorf("deferred tree reported as damage: %+v", issue)
		}
	}
	if gaps != 1 || report.MissingRefs != 1 {
		t.Fatalf("missing-reference issues %d, MissingRefs %d; want 1 and 1", gaps, report.MissingRefs)
	}

	// Once the sub-manifest arrives and links, the repository verifies clean.
	store(leaf)
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatal(err)
	}
	report, err = verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("issues after the sub-manifest arrived: %+v", report.Issues)
	}
}
