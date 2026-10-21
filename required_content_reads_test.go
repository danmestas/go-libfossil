package libfossil

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// Structural recovery does not make outside F bytes readable. Each local read
// refuses accurately instead of calling healthy elision a missing tracked path.
func TestSparseLocalReadRefusals(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	ctx := context.Background()
	parents := []fsltype.FslID{fsltype.FslID(f.parent)}
	err := manifest.RequireParentContent(ctx, f.target.inner.DB(), parents)
	if err := (RequiredContentOpts{Peer: f.peer}).recover(ctx, f.target.inner, err); err != nil {
		t.Fatal(err)
	}
	files, err := f.target.ListFiles(f.parent)
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for _, file := range files {
		if file.UUID == f.outside[0] {
			name = file.Name
		}
	}
	if name == "" {
		t.Fatal("outside payload path not found")
	}
	for name, read := range map[string]func() error{
		"cat": func() error { _, err := f.target.ReadFile(f.parent, name); return err },
		"annotate": func() error {
			_, err := f.target.Annotate(AnnotateOpts{FilePath: name, StartRID: f.parent})
			return err
		},
		"diff": func() error { _, err := f.target.Diff(f.parent, f.parent, name); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := read()
			if err == nil || !strings.Contains(err.Error(), "elided") ||
				!strings.Contains(err.Error(), "widen/deepen") {
				t.Fatalf("local read refusal: %v", err)
			}
		})
	}
}
