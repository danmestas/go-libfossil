//go:build !js

package checkout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/testutil"
)

func TestSparseMarkerNativeRefusal(t *testing.T) {
	binary := testutil.RequireFossilBin(t)
	f := newSparseRepo(t, false)
	c := f.open(t, nil, f.c1)
	sparse, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetCone(sparse); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"status", "changes", "commit", "update", "revert", "stash", "clean"} {
		t.Run(command, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, command)
			cmd.Dir = c.Dir()
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "within an open check-out") {
				t.Fatalf("native %s: %v: %s", command, err, output)
			}
		})
	}
	if _, err := c.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	assertCheckoutMarker(t, c.Dir(), PreferredDBName())
	if _, err := f.repo.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir(), "src/a.c"), []byte("native after densification\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "commit", "-m", "dense after sparse", "--user", "test")
	cmd.Dir = c.Dir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native commit after densification: %v: %s", err, output)
	}
	rid, _, err := c.Version()
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, maps(sparseFiles, map[string]string{"src/a.c": "native after densification\n"}))
}
