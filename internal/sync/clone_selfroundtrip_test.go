package sync_test

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/internal/sync"
	"github.com/danmestas/libfossil/simio"
)

// blobUUIDs returns every content-bearing blob hash in r. Phantoms (size < 0)
// are excluded: they are placeholders for artifacts the repository has heard
// named but does not hold, and a clone is not expected to carry them.
func blobUUIDs(t *testing.T, r *repo.Repo) map[string]bool {
	t.Helper()
	rows, err := r.DB().Query("SELECT uuid FROM blob WHERE size >= 0")
	if err != nil {
		t.Fatalf("query blobs: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			t.Fatalf("scan blob: %v", err)
		}
		out[uuid] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate blobs: %v", err)
	}
	return out
}

// incompressibleBytes returns n bytes that zlib cannot shrink, from a fixed
// seed so the corpus is identical on every run. Compressible filler would make
// the wire message far smaller than the artifacts it carries, which is exactly
// the property under test.
func incompressibleBytes(seed int64, n int) []byte {
	if n <= 0 {
		panic("incompressibleBytes: n must be positive")
	}
	buf := make([]byte, n)
	rng := rand.New(rand.NewSource(seed))
	for i := range buf {
		buf[i] = byte(rng.Intn(256))
	}
	return buf
}

// listenAddr reserves a loopback address and releases it for the server to
// bind. Racy in principle, contained to a test in practice.
func listenAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return addr
}

// TestCloneSelfRoundTripLargeArtifacts clones a repository this implementation
// is serving, with this implementation's client, over the real HTTP transport,
// and requires the clone to hold every artifact the source holds.
//
// Method: build a corpus of multi-megabyte incompressible artifacts totalling
// more than sync.DefaultCloneBatchBytes, serve it with ServeHTTP, clone it with
// Clone over HTTPTransport, and compare blob-hash sets.
//
// Why the corpus looks like this. Every other clone test here is either served
// by real fossil or wired through MockTransport, which hands *xfer.Message
// values straight across and never encodes or decodes a wire body -- so no
// existing test could see a serialization defect at all. This one puts
// libfossil on both ends of a real encode/decode. The artifact sizes are the
// other half: issue #104 reproduced on the wapp corpus, whose artifacts are
// large enough that one clone round's expanded content ran past the bound the
// client applied when decompressing a message, and did not reproduce on
// pikchr, whose artifacts are small. A corpus of small artifacts round-trips
// cleanly under the defect and proves nothing, so this one is sized in
// megabytes and crosses the server's per-round budget, forcing multi-round
// pagination with large bodies on every round.
func TestCloneSelfRoundTripLargeArtifacts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-megabyte corpus")
	}
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "source.fossil")
	srcRepo, err := repo.Create(srcPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srcRepo.Close()

	// 6 checkins x 2 files x 1.8 MB = 21.6 MB expanded, against a
	// DefaultCloneBatchBytes of 16 MB: at least two rounds, each carrying
	// megabytes of expanded content.
	const (
		checkinCount  = 6
		filesPerCkin  = 2
		artifactBytes = 1_800_000
	)
	if checkinCount*filesPerCkin*artifactBytes <= sync.DefaultCloneBatchBytes {
		t.Fatalf("corpus does not cross the clone batch budget of %d bytes",
			sync.DefaultCloneBatchBytes)
	}
	for c := 0; c < checkinCount; c++ {
		files := make([]manifest.File, 0, filesPerCkin)
		for f := 0; f < filesPerCkin; f++ {
			seed := int64(c*filesPerCkin + f)
			files = append(files, manifest.File{
				Name:    fmt.Sprintf("blob%02d.bin", seed),
				Content: incompressibleBytes(seed, artifactBytes),
			})
		}
		if _, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
			Comment: fmt.Sprintf("checkin %d", c),
			User:    "testuser",
			Files:   files,
		}); err != nil {
			t.Fatalf("Checkin %d: %v", c, err)
		}
	}

	want := blobUUIDs(t, srcRepo)
	if len(want) == 0 {
		t.Fatal("source repository holds no blobs")
	}

	addr := listenAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sync.ServeHTTP(ctx, addr, srcRepo, sync.HandleSync)
	time.Sleep(200 * time.Millisecond)

	clonePath := filepath.Join(dir, "clone.fossil")
	transport := &sync.HTTPTransport{URL: "http://" + addr}
	cloneRepo, result, err := sync.Clone(ctx, clonePath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer cloneRepo.Close()

	got := blobUUIDs(t, cloneRepo)
	var missing []string
	for uuid := range want {
		if !got[uuid] {
			missing = append(missing, uuid)
		}
	}
	if len(missing) > 0 {
		t.Errorf("clone is missing %d of %d artifacts: %v",
			len(missing), len(want), missing)
	}
	if result.Rounds < 2 {
		t.Errorf("Rounds = %d, want >= 2: the corpus should not fit in one round",
			result.Rounds)
	}
}
