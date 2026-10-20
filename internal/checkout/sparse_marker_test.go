package checkout

import (
	"errors"
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

func TestSetConeMarkerBoundary(t *testing.T) {
	f := newSparseRepo(t, false)
	c := f.open(t, nil, f.c1)
	sparse, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetCone(sparse); err != nil {
		t.Fatal(err)
	}
	assertCheckoutMarker(t, c.Dir(), ".libfossil-checkout")
	if _, err := c.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	assertCheckoutMarker(t, c.Dir(), PreferredDBName())
}

func TestSetConeRefusesDenseMarkerCollision(t *testing.T) {
	f := newSparseRepo(t, false)
	sparse, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := f.open(t, sparse, f.c1)
	before := onDisk(t, c)
	marker := filepath.Join(c.Dir(), PreferredDBName())
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetCone(nil); err == nil {
		t.Fatal("accepted dense marker collision")
	}
	if c.Cone() == nil {
		t.Fatal("collision changed cone")
	}
	if got := onDisk(t, c); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("collision changed disk: %v", got)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "keep" {
		t.Fatalf("overwrote marker: %q, %v", data, err)
	}
}

func TestSetConeFailureRestoresFilesAndMarker(t *testing.T) {
	f := newSparseRepo(t, false)
	c := f.open(t, nil, f.c1)
	before := onDisk(t, c)
	sparse, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`CREATE TRIGGER fail_cone BEFORE INSERT ON vvar WHEN NEW.name = 'sparse-cone' BEGIN SELECT RAISE(ABORT, 'injected cone failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetCone(sparse); err == nil {
		t.Fatal("expected failure")
	}
	assertCheckoutMarker(t, c.Dir(), PreferredDBName())
	if c.Cone() != nil {
		t.Fatal("failed transition changed cone")
	}
	if got := onDisk(t, c); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("disk changed: %v, want %v", got, before)
	}
	for name, text := range sparseFiles {
		data, err := os.ReadFile(filepath.Join(c.Dir(), filepath.FromSlash(name)))
		if err != nil || string(data) != text {
			t.Fatalf("restored %s: %q, %v", name, data, err)
		}
	}
}

func TestSetConeRemovesNativeMarkerBeforeDematerialization(t *testing.T) {
	f := newSparseRepo(t, false)
	c := f.open(t, nil, f.c1)
	storage := &markerCheckingStorage{Storage: c.env.Storage, dir: c.Dir()}
	c.env.Storage = storage
	sparse, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetCone(sparse); err != nil {
		t.Fatal(err)
	}
	if storage.removed == 0 {
		t.Fatal("did not exercise dematerialization")
	}
}

type markerCheckingStorage struct {
	simio.Storage
	dir     string
	removed int
}

func (s *markerCheckingStorage) Remove(name string) error {
	if _, err := os.Stat(filepath.Join(s.dir, PreferredDBName())); !os.IsNotExist(err) {
		return errors.New("native marker present during dematerialization")
	}
	s.removed++
	return s.Storage.Remove(name)
}

func TestCreateRejectsDanglingMarker(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(target, filepath.Join(dir, PreferredDBName())); err != nil {
		t.Skip(err)
	}
	c, err := Create(r, dir, CreateOpts{})
	if c != nil {
		c.Close()
	}
	if err == nil {
		t.Fatal("accepted dangling checkout marker")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("followed dangling marker: %v", err)
	}
}

func assertCheckoutMarker(t *testing.T, dir, want string) {
	t.Helper()
	for _, name := range DBNames() {
		_, err := os.Stat(filepath.Join(dir, name))
		if name == want && err != nil {
			t.Fatalf("missing marker %s: %v", name, err)
		}
		if name != want && !os.IsNotExist(err) {
			t.Fatalf("unexpected marker %s: %v", name, err)
		}
	}
}

func TestSparseMetadataPathProtected(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	c, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, name := range []string{".libfossil-checkout", ".libfossil-checkout-wal", ".libfossil-checkout-shm", ".libfossil-checkout-journal", "sub/.LIBFOSSIL-CHECKOUT"} {
		if _, err := c.CheckFilename(name); err == nil {
			t.Errorf("accepted checkout metadata path %s", name)
		}
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
