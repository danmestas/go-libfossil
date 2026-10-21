package checkout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
)

// Destination sidecars belong to their creator, not a future SQLite connection.
// Each refusal must precede any change to the active checkout or working tree.
func TestMarkerTransitionRefusesDestinationFamily(t *testing.T) {
	for _, dense := range []bool{false, true} {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			for _, kind := range []string{"file", "directory", "dangling", "case-alias"} {
				name := "sparse/" + suffix + "/" + kind
				if dense {
					name = "dense/" + suffix + "/" + kind
				}
				t.Run(name, func(t *testing.T) {
					testMarkerTransitionCollision(t, dense, suffix, kind)
				})
			}
		}
	}
}

func testMarkerTransitionCollision(t *testing.T, dense bool, suffix, kind string) {
	t.Helper()
	f := newSparseRepo(t, false)
	var initial, next *cone.Cone
	target := sparseDBName
	if dense {
		initial = sparseCone(t)
		target = PreferredDBName()
	} else {
		next = sparseCone(t)
	}
	co := f.open(t, initial, f.c1)
	covered := filepath.Join(co.Dir(), "src/a.c")
	if err := os.WriteFile(covered, []byte("unscanned edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	marker := co.dbPath
	outside := filepath.Join(co.Dir(), "tools/t.c")
	beforeBytes, beforeErr := os.ReadFile(outside)
	check := createMarkerSentinel(t, filepath.Join(co.Dir(), target+suffix), kind)
	if _, err := co.SetCone(next); err == nil {
		t.Fatal("SetCone accepted an occupied destination marker family")
	}
	check()
	if data, err := os.ReadFile(covered); err != nil || string(data) != "unscanned edit\n" {
		t.Fatalf("refusal changed covered edit: %q, %v", data, err)
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after || marker != co.dbPath || co.Cone() != initial {
		t.Fatalf("refusal changed checkout state: snapshot=%v, marker=%s, err=%v",
			before == after, co.dbPath, err)
	}
	afterBytes, afterErr := os.ReadFile(outside)
	existenceChanged := os.IsNotExist(beforeErr) != os.IsNotExist(afterErr)
	if string(beforeBytes) != string(afterBytes) || existenceChanged {
		t.Fatalf("refusal changed outside file: %q -> %q, %v -> %v",
			beforeBytes, afterBytes, beforeErr, afterErr)
	}
}

func TestCreateRefusesDestinationFamily(t *testing.T) {
	for _, sparse := range []bool{false, true} {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			for _, kind := range []string{"file", "directory", "dangling", "case-alias"} {
				target := PreferredDBName()
				if sparse {
					target = sparseDBName
				}
				t.Run(target+suffix+"/"+kind, func(t *testing.T) {
					f := newSparseRepo(t, false)
					dir := t.TempDir()
					check := createMarkerSentinel(t, filepath.Join(dir, target+suffix), kind)
					var selected *cone.Cone
					if sparse {
						selected = sparseCone(t)
					}
					co, err := Create(f.repo, dir, CreateOpts{Cone: selected})
					if err == nil {
						co.Close()
						t.Error("Create accepted an occupied destination marker family")
					}
					check()
					if suffix != "" {
						if _, err := os.Lstat(filepath.Join(dir, target)); !os.IsNotExist(err) {
							t.Fatalf("refused Create left a database: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestOpenAllowsOwnSQLiteSidecars(t *testing.T) {
	for _, sparse := range []bool{false, true} {
		name := "dense"
		if sparse {
			name = "sparse"
		}
		t.Run(name, func(t *testing.T) {
			f := newSparseRepo(t, false)
			var selected *cone.Cone
			if sparse {
				selected = sparseCone(t)
			}
			co := f.open(t, selected, f.c1)
			if err := setVVar(co.db, "owned-sidecar", "sentinel"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(co.dbPath + "-wal"); err != nil {
				t.Fatalf("active checkout has no WAL: %v", err)
			}
			reopened, err := Open(f.repo, co.Dir(), OpenOpts{})
			if err != nil {
				t.Fatalf("Open refused its own SQLite sidecars: %v", err)
			}
			defer reopened.Close()
			value, err := getVVar(reopened.db, "owned-sidecar")
			if err != nil || value != "sentinel" {
				t.Fatalf("Open changed existing checkout: %q, %v", value, err)
			}
		})
	}
}

func createMarkerSentinel(t *testing.T, name, kind string) func() {
	t.Helper()
	actual := name
	switch kind {
	case "directory":
		if err := os.Mkdir(name, 0o755); err != nil {
			t.Fatal(err)
		}
	case "dangling":
		if err := os.Symlink("missing-sentinel-target", name); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	case "case-alias", "file":
		if kind == "case-alias" {
			actual = filepath.Join(filepath.Dir(name), strings.ToUpper(filepath.Base(name)))
		}
		if err := os.WriteFile(actual, []byte("owned sentinel\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(name); os.IsNotExist(err) {
			t.Skip("filesystem does not alias case")
		}
	default:
		t.Fatal("unknown sentinel kind")
	}
	before, err := os.Lstat(actual)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		after, err := os.Lstat(actual)
		if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
			t.Fatalf("sentinel replaced: before=%v, after=%v, err=%v", before, after, err)
		}
		if kind == "dangling" {
			link, err := os.Readlink(actual)
			if err != nil || link != "missing-sentinel-target" {
				t.Fatalf("sentinel link changed: %q, %v", link, err)
			}
		} else if kind != "directory" {
			data, err := os.ReadFile(actual)
			if err != nil || string(data) != "owned sentinel\n" {
				t.Fatalf("sentinel bytes changed: %q, %v", data, err)
			}
		}
	}
}
