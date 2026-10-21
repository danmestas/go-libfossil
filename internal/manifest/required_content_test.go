package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

type requiredExpansionCounter struct {
	db.Querier
	loads map[string]int
}

func (q *requiredExpansionCounter) QueryRow(query string, args ...any) *sql.Row {
	if strings.HasPrefix(query, "SELECT content, size FROM blob") {
		q.loads[fmt.Sprint(args[0])]++
	}
	return q.Querier.QueryRow(query, args...)
}

// Visibility and relationship validation must share expanded structural bytes:
// a dense parent cannot pay for the entire S tree twice in a single preflight.
func TestRequireParentContentExpandsEachArtifactOnce(t *testing.T) {
	r := sha3Repo(t)
	for key, value := range map[string]string{
		SettingEnableSubmanifests: "on", SettingSubmanifestK: "1",
	} {
		if err := r.SetConfig(key, value); err != nil {
			t.Fatal(err)
		}
	}
	var files []File
	for _, card := range sparseBoundaryCandidates(t) {
		files = append(files, File{Name: card.Name, Content: []byte(card.Name)})
	}
	rid, parent := commit(t, r, CheckinOpts{Files: files})
	if len(parent.S) < 2 {
		t.Fatal("fixture requires a dense structural tree")
	}
	q := &requiredExpansionCounter{Querier: r.DB(), loads: make(map[string]int)}
	if err := RequireParentContent(context.Background(), q, []libfossil.FslID{rid}); err != nil {
		t.Fatal(err)
	}
	if len(q.loads) < 3 {
		t.Fatalf("did not observe root and sibling expansion reads: %+v", q.loads)
	}
	for artifact, count := range q.loads {
		if count != 1 {
			t.Fatalf("artifact %s expanded %d times in one preflight", artifact, count)
		}
	}
}

func TestRequireParentContentElidedSibling(t *testing.T) {
	files := sparseBoundaryCandidates(t)
	old := append(files[:1:1], files[2:]...)
	tree := mustBuildTree(t, old, 1)
	r, rid, _, _ := sparseBoundaryParent(t, files, old, tree)
	err := RequireParentContent(context.Background(), r.DB(), []libfossil.FslID{rid})
	var required *RequiredContentError
	if !errors.As(err, &required) {
		t.Fatalf("required-content refusal: %v", err)
	}
	if len(required.Roots) != 1 || len(required.Artifacts) != 1 {
		t.Fatalf("incomplete recovery instruction: %+v", required)
	}
	if required.Artifacts[0] != tree.S[1].UUID || required.Elided < 2 {
		t.Fatalf("incorrect recovery instruction: %+v", required)
	}
	if !strings.Contains(err.Error(), "elided") || !strings.Contains(err.Error(), "no configured peer") {
		t.Fatalf("incorrect absence diagnostic: %v", err)
	}
	if strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("elision described as damage: %v", err)
	}
	var before int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, _, err = Checkin(r, CheckinOpts{
		Files: []File{{Name: "new", Content: []byte("new bytes")}}, Parent: rid, User: "test",
	})
	if !errors.As(err, &required) {
		t.Fatalf("local-only writer did not refuse structurally incomplete parent: %v", err)
	}
	var after int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("refusal leaked new blobs: before=%d after=%d", before, after)
	}
}

func TestMergeParentFilesCarriesElidedPayload(t *testing.T) {
	r := sha3Repo(t)
	parentRID, parent := commit(t, r, CheckinOpts{Files: []File{
		{Name: "inside/a", Content: []byte("inside")},
		{Name: "outside/a", Content: []byte("outside")},
	}})
	c, err := cone.New([]string{"inside"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	outsideUUID := parent.F[1].UUID
	elideWriterNode(t, r, outsideUUID)
	files, err := MergeParentFiles(r, parentRID, []File{{Name: "inside/a", Content: []byte("edited")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[1].UUID != outsideUUID || files[1].Content != nil {
		t.Fatalf("unchanged outside entry was not carried verbatim: %+v", files)
	}
	_, _, err = Checkin(r, CheckinOpts{Files: files, Parent: parentRID, User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, held := content.AvailableByUUID(r.DB(), outsideUUID); held {
		t.Fatal("metadata-only carry fetched outside payload")
	}
}

// Retaining bytes does not authorize consuming a shunned structural node.
func TestRequireParentContentHeldShunnedChild(t *testing.T) {
	files := sparseBoundaryCandidates(t)
	old := append(files[:1:1], files[2:]...)
	tree := mustBuildTree(t, old, 1)
	r, rid, _, _ := sparseBoundaryParent(t, files, old, tree)
	held := tree.S[0].UUID
	if _, ok := content.AvailableByUUID(r.DB(), held); !ok {
		t.Fatal("fixture first subtree is not held")
	}
	if _, err := r.DB().Exec("INSERT INTO shun(uuid) VALUES(?)", held); err != nil {
		t.Fatal(err)
	}
	err := RequireParentContent(context.Background(), r.DB(), []libfossil.FslID{rid})
	if err == nil || !strings.Contains(err.Error(), "shunned") {
		t.Fatalf("held shunned structural content was not refused: %v", err)
	}
}

func TestRequireParentContentCancellation(t *testing.T) {
	r := sha3Repo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RequireParentContent(ctx, r.DB(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preflight: %v", err)
	}
}
