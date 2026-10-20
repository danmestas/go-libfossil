package manifest

import (
	"reflect"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
)

func sparseReplacementParent(t *testing.T) (*repo.Repo, libfossil.FslID, *deck.Deck) {
	t.Helper()
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	rid, parent := commit(t, r, CheckinOpts{Files: a1CheckinFiles()})
	c, err := cone.New([]string{"README.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	elideWriterNode(t, r, parent.S[1].UUID)
	return r, rid, parent
}

// This is a refusal diagnostic, not a sparse-write conformance claim. The
// writer must not silently reinterpret a partial Checkin.Files as a full set.
func TestSparseWriterRefusesUnprovenPartialReplacement(t *testing.T) {
	r, parentRID, parent := sparseReplacementParent(t)
	var before int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before); err != nil {
		t.Fatal(err)
	}
	rid, uuid, err := Checkin(r, CheckinOpts{Parent: parentRID, User: "drh", Time: writeTime,
		Files: []File{{Name: "README.md", Content: []byte("changed\n"), Perm: "x"}}})
	if err == nil || rid != 0 || uuid != "" {
		t.Fatalf("partial replacement = %d, %q, %v; want atomic refusal", rid, uuid, err)
	}
	if missing, ok := MissingSubmanifest(err); !ok || missing != parent.S[1].UUID {
		t.Fatalf("partial replacement error = %v; want absent sibling S %s", err, parent.S[1].UUID)
	}
	t.Logf("same-name sparse replacement remains unsupported: %v", err)
	var after int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&after); err != nil || after != before {
		t.Fatalf("blob rows after refusal = %d, %v; want unchanged %d", after, err, before)
	}
	if _, ok := content.AvailableByUUID(r.DB(), hash.SHA3([]byte("changed\n"))); ok {
		t.Fatal("failed sparse commit leaked new file content")
	}
	if _, ok := content.AvailableByUUID(r.DB(), parent.S[1].UUID); ok {
		t.Fatal("failed sparse commit filled untouched sibling S")
	}
	wantFiles := a1Files()
	wantFiles[0].UUID, wantFiles[0].Perm = hash.SHA3([]byte("changed\n")), "x"
	want := mustBuildTree(t, wantFiles, 3)
	if !reflect.DeepEqual(want.S[1:], parent.S[1:]) {
		t.Fatal("same-name edit at the original k unexpectedly changed an untouched sibling")
	}
}
