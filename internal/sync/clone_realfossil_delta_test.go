package sync_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/simio"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestCloneRealFossilWithDeltaChain drives a fixed corpus (a linear delta
// chain) from this server through a real fossil binary and checks that the
// clone is actually usable, not merely that the `fossil clone` subprocess
// exits 0. It is skipped, not failed, when no fossil binary is on PATH.
//
// #141 changed how a deltified row is sent during clone: instead of expanding
// the delta and shipping full content, it retransmits the row as a delta and
// emits the delta's source ahead of it (buildCloneArtifact walks the chain
// source-first). content.Deltify deltifies the OLDER artifact against the
// NEWER one, so a delta's source almost always has a *greater* rid than the
// delta itself and, under the send loop's ascending-rid order, would not yet
// have been sent; walking source-first guarantees no delta forward-references
// a card that has not arrived. The delta rides an uncompressed "file UUID
// DELTASRC SIZE" card (not a compressed "cfile"), matching canonical fossil's
// send_delta_native, so the receiver re-frames it into fossil's on-disk blob
// format. That change is a bandwidth win and is verified content-identical for
// libfossil<->libfossil clones by the self-round-trip tests.
//
// Full content rides a compressed "cfile" card. #152 fixed that card's wire
// framing: the payload is now fossil's on-disk blob format ([4-byte
// big-endian size][zlib]), the exact bytes blob_compress() produces, so a real
// fossil client stores it verbatim and later reads it back through
// blob_uncompress() successfully. Before that fix go-libfossil emitted bare
// zlib (no prefix), so full content decoded to garbage and rebuilt to zero
// check-ins. This test asserts real usability -- it clones, rebuilds, and
// requires all six check-ins to materialize.
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
	const wantCheckins = 6
	base := make([]byte, 4096)
	for i := range base {
		base[i] = byte(i % 251)
	}
	var parent libfossil.FslID
	for c := 0; c < wantCheckins; c++ {
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
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real fossil clone of a %d-deep delta chain failed: %v\n%s", deltaCount, err, out)
	}

	// A clone that exits 0 is necessary but not sufficient: a real fossil client
	// reports success and can still hold unusable content. Rebuild forces it to
	// decode and verify every artifact; counting check-ins afterward proves the
	// content actually materialized. Asserting only on the clone exit code (as
	// this test originally did) proves nothing but that the subprocess ran.
	if out, err := exec.Command(bin, "rebuild", clonePath).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild of the clone failed: %v\n%s", err, out)
	}

	got := fossilCheckinCount(t, bin, clonePath)
	if got != wantCheckins {
		t.Fatalf("clone materialized %d check-ins, want %d (#152: cfile framing regression?)", got, wantCheckins)
	}
}

// fossilCheckinCount returns the number of check-in events a real fossil repo
// holds, via `fossil sql`. Used to prove a clone is actually populated.
func fossilCheckinCount(t *testing.T, bin, repoPath string) int {
	t.Helper()
	out, err := exec.Command(bin, "sql", "-R", repoPath,
		"SELECT count(*) FROM event WHERE type='ci';").Output()
	if err != nil {
		t.Fatalf("counting check-ins in clone: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parsing check-in count %q: %v", string(out), err)
	}
	return n
}
