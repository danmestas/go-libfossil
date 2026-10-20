package manifest

import (
	"context"
	"errors"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

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
	if len(required.Roots) != 1 || len(required.Artifacts) != 1 || required.Artifacts[0] != tree.S[1].UUID || required.Elided < 2 {
		t.Fatalf("incomplete recovery instruction: %+v", required)
	}
	if !strings.Contains(err.Error(), "elided") || !strings.Contains(err.Error(), "no configured peer") || strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("incorrect absence diagnostic: %v", err)
	}
	var before int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, _, err = Checkin(r, CheckinOpts{Files: []File{{Name: "new", Content: []byte("new bytes")}}, Parent: rid, User: "test"})
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

func TestRequireParentContentCancellation(t *testing.T) {
	r := sha3Repo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RequireParentContent(ctx, r.DB(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preflight: %v", err)
	}
}
