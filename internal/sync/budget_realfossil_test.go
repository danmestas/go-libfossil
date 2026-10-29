package sync_test

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/auth"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// TestRealFossilPushOverBudget pushes an artifact over the server's
// MaxStoredBytes from stock fossil: fossil prints the budget's error, and
// the server holds exactly what it held before the push.
func TestRealFossilPushOverBudget(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	const limit = 64 << 10

	srv, err := repo.Create(filepath.Join(dir, "server.fossil"), "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srv.Close()
	if _, _, err := manifest.Checkin(srv, manifest.CheckinOpts{
		Comment: "seed", User: "testuser", Parent: libfossil.FslID(0),
		Files: []manifest.File{{Name: "seed.txt", Content: []byte("seed\n")}},
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if err := auth.UpdateCaps(srv.DB(), "nobody", "gijoryz"); err != nil {
		t.Fatalf("grant nobody push: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := serveRepoWith(ctx, t, srv,
		func(ctx context.Context, r *repo.Repo, req *xfer.Message) (*xfer.Message, error) {
			resp, err := sync.HandleSyncWithOpts(ctx, r, req, sync.HandleOpts{MaxStoredBytes: limit})
			// A host serving HTTP sends a refused round's reply: its error
			// card is what fossil prints.
			var refused *sync.RefusedError
			if errors.As(err, &refused) {
				return resp, nil
			}
			return resp, err
		})

	clonePath := filepath.Join(dir, "clone.fossil")
	fossilOK(t, bin, dir, "clone", base, clonePath)
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilOK(t, bin, work, "settings", "autosync", "off", "-R", clonePath)
	fossilOK(t, bin, work, "open", "--nosync", clonePath)
	big := make([]byte, 4*limit)
	rand.New(rand.NewSource(1)).Read(big)
	if err := os.WriteFile(filepath.Join(work, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	fossilOK(t, bin, work, "add", "big.bin")
	fossilOK(t, bin, work, "commit", "-m", "adds a big file", "--no-warnings")

	before := blobDigest(t, srv)
	out, _ := fossilRun(bin, work, "push", base, "--once")
	if !strings.Contains(out, "this repository stores at most 65536 bytes per artifact") {
		t.Fatalf("fossil push did not report the budget refusal:\n%s", out)
	}
	if after := blobDigest(t, srv); after != before {
		t.Fatalf("server store changed across a refused push:\nbefore %s\n after %s", before, after)
	}
}

// blobDigest summarises r's blob table, phantoms included.
func blobDigest(t *testing.T, r *repo.Repo) string {
	t.Helper()
	var d string
	if err := r.DB().QueryRow(`SELECT count(*) || ':' || coalesce(group_concat(uuid || '/' || size, ','), '')
		FROM (SELECT uuid, size FROM blob ORDER BY uuid)`).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}
