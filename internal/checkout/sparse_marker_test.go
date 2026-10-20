package checkout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/simio"
)

func TestSparseCreatePrivateMarker(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	sparse, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c, err := Create(r, dir, CreateOpts{Cone: sparse})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".libfossil-checkout")
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".fslckout", "_FOSSIL_"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("native marker %s exists: %v", name, err)
		}
	}
	got, err := FindCheckoutDB(simio.OSStorage{}, dir, false)
	if err != nil || got != want {
		t.Fatalf("discovery = %q, %v; want %q", got, err, want)
	}
	reopened, err := Open(r, dir, OpenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Cone() == nil {
		t.Fatal("lost sparse cone")
	}
}

func TestCheckoutMarkerCollision(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	for _, name := range []string{".libfossil-checkout", ".fslckout", "_FOSSIL_"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, name)
			original := []byte("do not overwrite")
			if err := os.WriteFile(marker, original, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Create(r, dir, CreateOpts{})
			if c != nil {
				c.Close()
			}
			if err == nil {
				t.Fatal("Create accepted existing marker")
			}
			got, err := os.ReadFile(marker)
			if err != nil || string(got) != string(original) {
				t.Fatalf("marker changed: %q, %v", got, err)
			}
		})
	}
}

func TestFindCheckoutDBRejectsAmbiguousMarkers(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".libfossil-checkout", PreferredDBName()} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := FindCheckoutDB(simio.OSStorage{}, dir, false)
	if err == nil || !strings.Contains(err.Error(), "multiple checkout") {
		t.Fatalf("ambiguous discovery = %v", err)
	}
}
