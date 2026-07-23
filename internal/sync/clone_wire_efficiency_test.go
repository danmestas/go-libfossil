package sync_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// wireCaptureTransport speaks the HTTP xfer protocol directly against a server
// so it can measure both the exact compressed response bytes the server wrote
// and the card-type mix of every reply. It exists to attribute a clone's wire
// cost to card type -- the distinction issue #98 turns on and issue #113 found
// no test guarded.
type wireCaptureTransport struct {
	url           string
	fileCards     int
	cfileCards    int
	otherCards    int
	respBodyBytes int64
}

func (t *wireCaptureTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	if req == nil {
		panic("wireCaptureTransport.Exchange: req must not be nil")
	}
	body, err := req.Encode()
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", xfer.ContentTypeCompressed)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	t.respBodyBytes += int64(len(respBody))
	msg, err := xfer.Decode(respBody, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	for _, c := range msg.Cards {
		switch c.(type) {
		case *xfer.FileCard:
			t.fileCards++
		case *xfer.CFileCard:
			t.cfileCards++
		default:
			t.otherCards++
		}
	}
	return msg, nil
}

// sumBlobContentBytes returns the server's own on-disk stored size: the sum of
// stored blob content lengths, each blob as fossil holds it (zlib-compressed,
// or a delta). This is the baseline a conformant clone's wire cost is measured
// against.
func sumBlobContentBytes(t *testing.T, r *repo.Repo) int64 {
	t.Helper()
	var n int64
	if err := r.DB().QueryRow(
		"SELECT coalesce(sum(length(content)),0) FROM blob WHERE content IS NOT NULL",
	).Scan(&n); err != nil {
		t.Fatalf("sum blob content: %v", err)
	}
	if n <= 0 {
		t.Fatalf("stored blob bytes must be positive, got %d", n)
	}
	return n
}

// compressibleBlob returns n bytes of highly compressible, per-seed-distinct
// text. Compressibility is the point: a regression that serves raw `file` cards
// instead of `cfile` cards puts uncompressed content on the wire, which this
// content makes visibly larger than its stored (compressed) form.
func compressibleBlob(seed, n int) []byte {
	if n <= 0 {
		panic("compressibleBlob: n must be positive")
	}
	line := fmt.Sprintf("line of repository content, artifact seed %d, padding padding padding\n", seed)
	b := make([]byte, 0, n+len(line))
	for len(b) < n {
		b = append(b, line...)
	}
	return b[:n]
}

// TestCloneServesCompressedNotExpandedContent is the wire-efficiency regression
// guard issue #113 asks for: it fails if the clone server reverts to emitting
// expanded `file` cards instead of the stored-content `cfile` cards issue #98
// established (§7.2), the defect that produced 17.9x-224x serve-side wire
// amplification.
//
// Method: serve a small, deliberately compressible corpus with this
// implementation's server; clone it with this implementation's client over the
// real HTTP transport while a capture transport records both the card-type mix
// of every reply and the total compressed response bytes. Assert the clone
// content travels as cfile cards, that not one expanded file card is emitted,
// and -- as a coarse backstop -- that the served bytes stay within a generous
// multiple of the repository's own stored size.
//
// Why assert on card type, not only bytes: the server compresses the whole
// reply container, so a regression to raw file cards on compressible content is
// re-compressed back down and can hide inside a byte-ratio bound. The card type
// is the load-bearing, compression-proof signal; the 3x byte bound is a second,
// looser net that also catches gross or uncompressed-wire regressions.
func TestCloneServesCompressedNotExpandedContent(t *testing.T) {
	dir := t.TempDir()
	srcRepo := newSelfRoundTripRepo(t, dir)
	defer srcRepo.Close()

	const commits = 4
	const filesPerCommit = 5
	const fileBytes = 8 << 10
	var parent libfossil.FslID
	for c := 0; c < commits; c++ {
		files := make([]manifest.File, 0, filesPerCommit)
		for f := 0; f < filesPerCommit; f++ {
			seed := c*filesPerCommit + f
			files = append(files, manifest.File{
				Name:    fmt.Sprintf("src/file%02d.txt", seed),
				Content: compressibleBlob(seed, fileBytes),
			})
		}
		mid, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
			Comment: fmt.Sprintf("commit %d", c),
			User:    "testuser",
			Parent:  parent,
			Files:   files,
		})
		if err != nil {
			t.Fatalf("Checkin %d: %v", c, err)
		}
		parent = mid
	}
	if _, err := manifest.Crosslink(srcRepo); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	stored := sumBlobContentBytes(t, srcRepo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url := serveRepo(ctx, t, srcRepo)

	capture := &wireCaptureTransport{url: url}
	cloneRepo, result, err := sync.Clone(ctx, filepath.Join(dir, "clone.fossil"), capture, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer cloneRepo.Close()

	// The clone must actually carry content, or the guard proves nothing.
	if result.BlobsRecvd == 0 {
		t.Fatal("clone received no blobs; nothing was measured")
	}

	// The issue #98 invariant and issue #113 regression guard: stored content
	// is served as cfile cards, never expanded file cards.
	if capture.fileCards != 0 {
		t.Fatalf("clone served %d expanded file card(s); §7.2 requires stored cfile content (issue #98 regression)",
			capture.fileCards)
	}
	if capture.cfileCards == 0 {
		t.Fatalf("clone served no cfile cards (blobs received=%d); the stored-content path was not exercised",
			result.BlobsRecvd)
	}

	// Coarse backstop: served bytes stay within a generous multiple of stored.
	const maxRatio = 3
	if capture.respBodyBytes > int64(maxRatio)*stored {
		t.Fatalf("served %d bytes exceeds %dx stored size %d (ratio %.2fx)",
			capture.respBodyBytes, maxRatio, stored, float64(capture.respBodyBytes)/float64(stored))
	}
	t.Logf("clone wire: %d bytes served, %d stored (%.2fx); cards: cfile=%d file=%d other=%d",
		capture.respBodyBytes, stored, float64(capture.respBodyBytes)/float64(stored),
		capture.cfileCards, capture.fileCards, capture.otherCards)
}
