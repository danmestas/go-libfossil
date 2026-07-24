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
// That attempt forwarded a deltified row's stored delta bytes as a compressed
// "cfile" card and emitted cards in ascending-rid order. Two things broke a
// real fossil 2.28 client: the send order (content.Deltify deltifies the OLDER
// artifact against the NEWER one, so a delta's source almost always has a
// greater rid than the delta and, in ascending order, had not been sent yet),
// and the wire format (a real client stores a cfile payload verbatim and
// cannot decompress it without fossil's on-disk [4-byte size][zlib] framing,
// which the wire cfile omits). Either way the delta's source never
// materialized, so content_get() on it during the post-clone rebuild returned
// without initializing its output blob, tripping an assertion in blob_copy
// (blob.c:397) and aborting the client mid-rebuild.
//
// The #141 fix retransmits a deltified row as a delta but emits its source
// first (buildCloneArtifact walks the chain source-first) and sends the delta
// as an uncompressed "file UUID DELTASRC SIZE" card -- exactly as canonical
// fossil's send_delta_native does, so the receiver re-frames it into fossil's
// on-disk blob format. Full content still rides a compressed cfile. This test
// drives a fixed corpus (a linear delta chain) through a real fossil binary end
// to end; it is skipped, not failed, when no fossil binary is on PATH.
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
