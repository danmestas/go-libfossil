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
	"github.com/danmestas/libfossil/internal/xfer"
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
// the wire body far smaller than the artifact it carries, and the size of the
// wire body is the whole point of the fixtures below.
func incompressibleBytes(seed int64, n int) []byte {
	if n <= 0 {
		panic("incompressibleBytes: n must be positive")
	}
	buf := make([]byte, n)
	if _, err := rand.New(rand.NewSource(seed)).Read(buf); err != nil {
		panic("incompressibleBytes: " + err.Error())
	}
	return buf
}

// serveRepo starts an xfer HTTP server for r on a loopback port and returns
// its base URL. It waits for the listener to accept a connection rather than
// sleeping, and fails the test if the server returns before then.
func serveRepo(ctx context.Context, t *testing.T, r *repo.Repo) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- sync.ServeHTTP(ctx, addr, r, sync.HandleSync) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return "http://" + addr
		}
		select {
		case serveErr := <-errc:
			t.Fatalf("ServeHTTP returned before accepting on %s: %v", addr, serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not accept on %s within 10s: %v", addr, dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cloneSelfAndCompare serves srcRepo with this implementation's server, clones
// it with this implementation's client over HTTP, and requires the clone to
// hold every artifact the source holds.
func cloneSelfAndCompare(t *testing.T, srcRepo *repo.Repo, dir string) *sync.CloneResult {
	t.Helper()
	want := blobUUIDs(t, srcRepo)
	if len(want) == 0 {
		t.Fatal("source repository holds no blobs")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url := serveRepo(ctx, t, srcRepo)

	clonePath := filepath.Join(dir, "clone.fossil")
	cloneRepo, result, err := sync.Clone(
		ctx, clonePath, &sync.HTTPTransport{URL: url}, sync.CloneOpts{})
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
		t.Fatalf("clone is missing %d of %d artifacts: %v", len(missing), len(want), missing)
	}
	return result
}

// newSelfRoundTripRepo creates an empty source repository under dir.
func newSelfRoundTripRepo(t *testing.T, dir string) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(dir, "source.fossil"), "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	return r
}

// TestCloneSelfRoundTrip clones a repository this implementation is serving,
// with this implementation's client, over the real HTTP transport, and
// requires artifact parity.
//
// Method: a small corpus, served and cloned, compared by blob-hash set.
//
// This is a coverage plank, not a guard on any particular defect. It exists
// because every other clone test here is either driven by real fossil or wired
// through MockTransport, which hands *xfer.Message values straight across and
// never encodes a wire body at all. Nothing covered a body this implementation
// produced being decoded by this implementation's decoder, which is the only
// configuration in which a serialization defect meets a decoder that disagrees
// with it. TestCloneSelfRoundTripOversizeArtifact is the actual #104
// regression test; this one is cheap and runs everywhere, including -short.
func TestCloneSelfRoundTrip(t *testing.T) {
	dir := t.TempDir()
	srcRepo := newSelfRoundTripRepo(t, dir)
	defer srcRepo.Close()

	for c := 0; c < 3; c++ {
		if _, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
			Comment: fmt.Sprintf("checkin %d", c),
			User:    "testuser",
			Files: []manifest.File{
				{Name: fmt.Sprintf("text%d.txt", c), Content: []byte(fmt.Sprintf("checkin %d\n", c))},
				{Name: fmt.Sprintf("data%d.bin", c), Content: incompressibleBytes(int64(c), 64<<10)},
			},
		}); err != nil {
			t.Fatalf("Checkin %d: %v", c, err)
		}
	}

	cloneSelfAndCompare(t, srcRepo, dir)
}

// TestCloneSelfRoundTripOversizeArtifact is the #104 regression test: it
// requires a clone to survive a round whose body is larger than the decoder's
// bound used to be.
//
// Method: one incompressible artifact of 7/8ths of xfer.MaxDecompressedBytes,
// served and cloned by this implementation, compared by blob-hash set.
//
// Why that size, precisely. The defect was never about artifact count or total
// corpus size. It was that the decoder's bound on one decompressed message sat
// below what one round can emit -- and a round is not bounded by
// sync.DefaultCloneBatchBytes. The budget is charged before each artifact and
// the artifact that crosses it is sent whole, so a round carries up to the
// budget plus one entire artifact of unbounded size. A corpus of many small
// artifacts therefore cannot reach the bound however large the corpus grows,
// because pagination keeps every round near the budget; only a single artifact
// larger than bound-minus-budget can. That single artifact is also the one
// remaining path by which a round can exceed the current bound, so this
// fixture guards the live risk with the same bytes that reproduce the
// historical one.
//
// The size is expressed against MaxDecompressedBytes rather than as a literal
// so it tracks the bound: 7/8ths clears any bound an eighth below the current
// one -- which the bound this replaced was -- while leaving an eighth of
// headroom beneath the current one.
func TestCloneSelfRoundTripOversizeArtifact(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates an artifact of 7/8ths of xfer.MaxDecompressedBytes")
	}
	const artifactBytes = xfer.MaxDecompressedBytes / 8 * 7
	if artifactBytes <= 2*sync.DefaultCloneBatchBytes {
		t.Fatalf("artifact of %d bytes does not exceed twice the clone batch budget of %d",
			artifactBytes, sync.DefaultCloneBatchBytes)
	}
	if artifactBytes >= xfer.MaxDecompressedBytes {
		t.Fatalf("artifact of %d bytes cannot fit under the decode bound of %d",
			artifactBytes, xfer.MaxDecompressedBytes)
	}

	dir := t.TempDir()
	srcRepo := newSelfRoundTripRepo(t, dir)
	defer srcRepo.Close()

	if _, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
		Comment: "one oversize artifact",
		User:    "testuser",
		Files: []manifest.File{
			{Name: "oversize.bin", Content: incompressibleBytes(1, artifactBytes)},
		},
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	result := cloneSelfAndCompare(t, srcRepo, dir)
	if result.BlobsRecvd < 2 {
		t.Errorf("BlobsRecvd = %d, want >= 2 (artifact and manifest)", result.BlobsRecvd)
	}
}
