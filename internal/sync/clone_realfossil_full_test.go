package sync_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/simio"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestCloneRealFossilFullContent is the direct regression test for issue #152:
// a real fossil client cloning FULL (non-delta) content from a go-libfossil
// server must end up with usable check-ins and manifests, not garbage.
//
// Every full-content artifact -- file blobs AND the manifests themselves --
// rides a compressed "cfile" card during clone (handler.go emits CFileCard for
// any non-delta row). The bug was that go-libfossil wrote the cfile payload as
// bare zlib, but fossil's on-disk cfile format (blob_compress) is [4-byte
// big-endian size][zlib], and fossil's receiver stores the wire payload
// verbatim into blob.content, later reading it back through blob_uncompress()
// -- which needs that prefix. Without it, `fossil rebuild` decodes every
// artifact to garbage and lands zero check-ins even though `fossil clone`
// exits 0.
//
// This corpus is built so nothing deltifies (independent files, no parent
// linkage), guaranteeing the source stores only full content -- asserted via
// the empty delta table -- so the test exercises the full-content cfile path
// specifically, not the delta path #141 routed onto uncompressed file cards.
func TestCloneRealFossilFullContent(t *testing.T) {
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

	// Independent check-ins, each a distinct file with no parent -- there is no
	// predecessor for content.Deltify to act on, so every blob is stored full.
	const wantCheckins = 4
	for c := 0; c < wantCheckins; c++ {
		content := make([]byte, 2048)
		for i := range content {
			content[i] = byte((i*7 + c*131) % 256)
		}
		content = append(content, []byte(fmt.Sprintf("\nfull-content artifact %d\n", c))...)
		if _, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
			Comment: fmt.Sprintf("full checkin %d", c),
			User:    "testuser",
			Parent:  libfossil.FslID(0),
			Files: []manifest.File{
				{Name: fmt.Sprintf("file%d.bin", c), Content: content},
			},
		}); err != nil {
			t.Fatalf("Checkin %d: %v", c, err)
		}
	}

	if _, err := manifest.Crosslink(srcRepo); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	var deltaCount int
	if err := srcRepo.DB().QueryRow("SELECT count(*) FROM delta").Scan(&deltaCount); err != nil {
		t.Fatalf("count deltas: %v", err)
	}
	if deltaCount != 0 {
		t.Fatalf("fixture bug: source stored %d deltas -- this test must exercise only full content", deltaCount)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url := serveRepo(ctx, t, srcRepo)

	clonePath := filepath.Join(dir, "clone.fossil")
	if out, err := exec.Command(bin, "clone", url, clonePath).CombinedOutput(); err != nil {
		t.Fatalf("real fossil clone of full content failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, "rebuild", clonePath).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild of the clone failed: %v\n%s", err, out)
	}

	if got := fossilCheckinCount(t, bin, clonePath); got != wantCheckins {
		t.Fatalf("clone materialized %d check-ins, want %d (#152: full-content cfile framing regression?)", got, wantCheckins)
	}

	// A manifest is itself full content shipped over a cfile. Decoding it back
	// to parseable manifest text (not garbage) is the second half of #152's
	// acceptance: pick one check-in's manifest and confirm `fossil artifact`
	// yields real manifest cards.
	uuid := firstCheckinUUID(t, bin, clonePath)
	out, err := exec.Command(bin, "artifact", uuid, "-R", clonePath).Output()
	if err != nil {
		t.Fatalf("fossil artifact %s: %v", uuid, err)
	}
	text := string(out)
	// Canonical manifest cards prove the bytes decompressed to a real manifest
	// rather than undecodable garbage: the "C" comment card (our check-in
	// comment), an "F" file card naming a tracked file, and the trailing "Z"
	// md5 checksum card.
	for _, want := range []string{"C full", "\nF file", "\nZ "} {
		if !strings.Contains(text, want) {
			t.Fatalf("cloned manifest %s did not decode to parseable text (missing %q):\n%s", uuid, want, text)
		}
	}
}

// firstCheckinUUID returns the blob uuid of one check-in manifest in repoPath.
func firstCheckinUUID(t *testing.T, bin, repoPath string) string {
	t.Helper()
	out, err := exec.Command(bin, "sql", "-R", repoPath,
		"SELECT blob.uuid FROM event JOIN blob ON blob.rid=event.objid "+
			"WHERE event.type='ci' ORDER BY event.mtime LIMIT 1;").Output()
	if err != nil {
		t.Fatalf("selecting a check-in uuid: %v", err)
	}
	uuid := strings.Trim(strings.TrimSpace(string(out)), "'\"")
	if uuid == "" {
		t.Fatal("no check-in uuid found in clone")
	}
	return uuid
}
