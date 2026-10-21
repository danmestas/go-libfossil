package checkout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/simio"
)

func TestCreateMemoryStorageRefusesDiskMarker(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	dir := t.TempDir()
	c, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := setVVar(c.db, "undo_available", "sentinel"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := Create(r, dir, CreateOpts{Env: &simio.Env{Storage: simio.NewMemStorage()}})
	if err == nil {
		other.Close()
		t.Fatal("memory working storage concealed an existing SQLite checkout")
	}
	if !strings.Contains(err.Error(), "checkout marker already exists") {
		t.Fatalf("collision = %v", err)
	}
	reopened, err := Open(r, dir, OpenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := getVVar(reopened.db, "undo_available"); err != nil || got != "sentinel" {
		t.Fatalf("existing metadata overwritten: %q, %v", got, err)
	}
}

func TestOpenMigratesLegacySparseManagedRows(t *testing.T) {
	f := newSparseRepo(t, false)
	remoteFiles := maps(sparseFiles, map[string]string{"tools/t.c": "legacy merge\n"})
	remote := f.commit(t, remoteFiles, f.c1)
	c := f.open(t, nil, f.c1)
	if _, err := c.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	sparse := sparseCone(t)
	if err := writeCone(c.db, sparse); err != nil {
		t.Fatal(err)
	}
	for name := range sparseFiles {
		if !sparse.Covers(name) {
			if err := os.Remove(filepath.Join(c.Dir(), filepath.FromSlash(name))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.repo, c.Dir(), OpenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertCheckoutMarker(t, c.Dir(), ".libfossil-checkout")
	var managed, pending int
	if err := reopened.db.QueryRow("SELECT count(*) FROM vfile").Scan(&managed); err != nil || managed != len(sparseCovered) {
		t.Fatalf("managed rows = %d: %v", managed, err)
	}
	if err := reopened.db.QueryRow("SELECT count(*) FROM sparse_pending").Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending rows = %d: %v", pending, err)
	}
	rid, _, err := reopened.Commit(CommitOpts{Message: "migrated merge", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, remoteFiles)
}

func TestLegacySparseMigrationFailureRestoresMarker(t *testing.T) {
	f := newSparseRepo(t, false)
	c := f.open(t, nil, f.c1)
	if err := writeCone(c.db, sparseCone(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`CREATE TRIGGER reject_prune BEFORE DELETE ON vfile
	 BEGIN SELECT RAISE(ABORT, 'injected migration failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.repo, c.Dir(), OpenOpts{})
	if err == nil {
		reopened.Close()
		t.Fatal("migration ignored injected pruning failure")
	}
	if !strings.Contains(err.Error(), "injected migration failure") {
		t.Fatalf("migration = %v", err)
	}
	assertCheckoutMarker(t, c.Dir(), PreferredDBName())
	metadata, err := db.OpenSQL(filepath.Join(c.Dir(), PreferredDBName()), db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	var count int
	if err := metadata.QueryRow("SELECT count(*) FROM vfile").Scan(&count); err != nil || count != len(sparseFiles) {
		t.Fatalf("failed migration changed managed rows: %d, %v", count, err)
	}
}

func TestSetConePreservesExistingPrivateNameData(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, nil, f.c1)
	file := filepath.Join(co.Dir(), ".libfossil-checkout")
	if err := os.WriteFile(file, []byte("existing user data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(sparseCone(t)); err == nil {
		t.Fatal("narrowing overwrote existing private-name data")
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "existing user data\n" {
		t.Fatalf("collision changed existing data: %q, %v", got, err)
	}
	if co.Cone() != nil {
		t.Fatal("collision changed dense cone")
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), PreferredDBName())); err != nil {
		t.Fatal(err)
	}
	for name, want := range sparseFiles {
		if got, err := os.ReadFile(filepath.Join(co.Dir(), filepath.FromSlash(name))); err != nil || string(got) != want {
			t.Fatalf("collision changed %s: %q, %v", name, got, err)
		}
	}
}

func TestPrivateMetadataNotManagedAfterDensification(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	names := []string{".libfossil-checkout", ".libfossil-checkout-wal", ".libfossil-checkout-shm", ".libfossil-checkout-journal", "src/.LIBFOSSIL-CHECKOUT-WAL"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(co.Dir(), filepath.FromSlash(name)), []byte("control path sentinel\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := co.Manage(ManageOpts{Paths: []string{name}}); err == nil {
			t.Fatalf("managed control path %s after densification", name)
		}
	}
	files, err := co.walkDir(co.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if files[name] {
			t.Fatalf("extras walk exposed control path %s", name)
		}
	}
	var count int
	if err := co.db.QueryRow("SELECT count(*) FROM vfile").Scan(&count); err != nil || count != len(sparseFiles) {
		t.Fatalf("metadata protection changed managed rows: %d, %v", count, err)
	}
}

func TestForcedExtractCannotOverwritePrivateCheckout(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{".libfossil-checkout": "valid repository file\n"}), f.c1)
	sparse, err := cone.New([]string{".libfossil-checkout"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	co := f.open(t, sparse, f.c1)
	if err := setVVar(co.db, "local-sentinel", "protected"); err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(remote, ExtractOpts{Force: true}); err == nil {
		t.Fatal("forced extraction accepted private control path")
	}
	if got, err := getVVar(co.db, "local-sentinel"); err != nil || got != "protected" {
		t.Fatalf("forced extraction damaged checkout metadata: %q, %v", got, err)
	}
	if vid, _, err := co.Version(); err != nil || vid != f.c1 {
		t.Fatalf("refused extraction changed version: %d, %v", vid, err)
	}
}

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
