package sync_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/simio"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestCloneRealFossilWithDeltaChain requires a real fossil client to clone,
// and successfully rebuild, a repository this server holds with deltified
// content in the chain -- the interop regression a code review caught on
// issue #98's first attempt.
//
// That attempt forwarded a deltified row's stored delta bytes verbatim
// (correct per §7.2) but emitted cards in ascending-rid order for
// cursor/pagination reasons unrelated to delta direction. content.Deltify
// always deltifies the OLDER artifact against the NEWER one, so a delta's
// DeltaSource almost always names a rid greater than the delta's own rid --
// meaning the delta card routinely reached the client before the very source
// card it names. libfossil's own client tolerates that forward reference
// (storeDeltaContent phantoms the unseen source and resolves it later), but
// real fossil 2.28 does not: the source stayed a genuine, unfilled phantom,
// and content_get() on it during the client's post-clone rebuild pass
// returned without initializing its output blob, tripping an assertion in
// blob_copy (blob.c:397) and aborting the client mid-rebuild.
//
// The fix in emitCloneBatch expands a deltified row to full content before
// sending, rather than forwarding the stored delta bytes -- still
// zlib-compressed as a cfile card, just no longer dependent on send order.
// This test drives a fixed corpus (a linear 5-deep delta chain) through a
// real fossil binary end to end; it is skipped, not failed, when no fossil
// binary is on PATH.
func TestCloneRealFossilWithDeltaChain(t *testing.T) {
	bin, err := exec.LookPath("fossil")
	if err != nil {
		t.Skip("no fossil binary on PATH")
	}

	dir := t.TempDir()
	srcRepo, err := repo.Create(filepath.Join(dir, "source.fossil"), "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srcRepo.Close()

	// Six revisions of one file, each a small edit of the last, chained via
	// Parent so mlink resolves a predecessor for content.Deltify to act on.
	// Small edits keep the delta well under deltifyMinRatio, so every
	// revision but the last is actually stored as a delta.
	base := make([]byte, 4096)
	for i := range base {
		base[i] = byte(i % 251)
	}
	var parent libfossil.FslID
	for c := 0; c < 6; c++ {
		content := append([]byte(nil), base...)
		content[c] = byte(200 + c)
		content = append(content, []byte(fmt.Sprintf("\nrevision marker %d\n", c))...)
		mid, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
			Comment: fmt.Sprintf("checkin %d", c),
			User:    "testuser",
			Parent:  parent,
			Files: []manifest.File{
				{Name: "big.bin", Content: content},
			},
		})
		if err != nil {
			t.Fatalf("Checkin %d: %v", c, err)
		}
		parent = mid
	}

	if _, err := manifest.Crosslink(srcRepo); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	var deltaCount int
	if err := srcRepo.DB().QueryRow("SELECT count(*) FROM delta").Scan(&deltaCount); err != nil {
		t.Fatalf("count deltas: %v", err)
	}
	if deltaCount == 0 {
		t.Fatal("fixture bug: no deltas were created in the source repo -- this test needs a deltified chain to be meaningful")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url := serveRepo(ctx, t, srcRepo)

	clonePath := filepath.Join(dir, "clone.fossil")
	cmd := exec.Command(bin, "clone", url, clonePath)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real fossil clone of a %d-deep delta chain failed: %v\n%s", deltaCount, err, out)
	}
}
