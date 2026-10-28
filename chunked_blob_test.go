package libfossil

import (
	"bytes"
	"context"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// hostedValueLimit is what html-artifact-publisher configures, below the
// 2,000,000-byte ceiling the triggers in rejectHostedValues enforce.
const hostedValueLimit = 1_990_000

// rejectHostedValues makes d refuse any blob.content over 2,000,000 bytes,
// as a hosted SQLite with that value limit would.
func rejectHostedValues(t *testing.T, d *db.DB) {
	t.Helper()
	for _, op := range []string{"INSERT", "UPDATE"} {
		if _, err := d.Exec(`CREATE TRIGGER hosted_blob_` + op + ` BEFORE ` + op + ` ON blob
			WHEN length(NEW.content)>2000000
			BEGIN SELECT RAISE(ABORT, 'hosted SQLite value too large'); END`); err != nil {
			t.Fatal(err)
		}
	}
}

// largestValue is the largest value d holds in blob or blobpart.
func largestValue(t *testing.T, d *db.DB) int64 {
	t.Helper()
	var n int64
	if err := d.QueryRow("SELECT coalesce(max(length(content)), 0) FROM blob").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if hasBlobParts(t, d) {
		var p int64
		if err := d.QueryRow("SELECT coalesce(max(length(content)), 0) FROM blobpart").Scan(&p); err != nil {
			t.Fatal(err)
		}
		n = max(n, p)
	}
	return n
}

func hasBlobParts(t *testing.T, d *db.DB) bool {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='blobpart'").
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// wantChunked checks d holds n chunked rows, and parts for no other row.
func wantChunked(t *testing.T, d *db.DB, n int) {
	t.Helper()
	var rows, orphans int
	if err := d.QueryRow(`SELECT
		(SELECT count(*) FROM blob WHERE content IS NULL AND size>=0),
		(SELECT count(*) FROM blobpart WHERE rid NOT IN
			(SELECT rid FROM blob WHERE content IS NULL AND size>=0))`).Scan(&rows, &orphans); err != nil {
		t.Fatal(err)
	}
	if rows != n || orphans != 0 {
		t.Fatalf("chunked rows = %d, orphan parts = %d; want %d and 0", rows, orphans, n)
	}
}

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	_, _ = rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// commitBig commits content as big.bin and returns the check-in's rid and
// name; a clone numbers rids its own way, so other stores look it up by name.
func commitBig(t *testing.T, r *Repo, parent int64, content []byte) (int64, string) {
	t.Helper()
	rid, uuid, err := r.Commit(CommitOpts{User: "test", Comment: "big", ParentID: parent,
		Files: []FileToCommit{{Name: "big.bin", Content: content}}})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return rid, uuid
}

func wantFile(t *testing.T, r *Repo, version string, want []byte) {
	t.Helper()
	got, err := r.ReadFileAt(version, "big.bin")
	if err != nil {
		t.Fatalf("ReadFileAt %s: %v", version, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFileAt %s: %d bytes differ from the %d committed", version, len(got), len(want))
	}
}

// A file larger than a hosted backend's value limit commits, keeps its old
// version, clones to a store with no limit, and comes back by push, with no
// value in the limited store ever over the limit (issue #459).
func TestChunkedBlobsCloneAndPush(t *testing.T) {
	restore := blob.SetMaxValueBytes(hostedValueLimit)
	t.Cleanup(func() { restore() })
	ctx := context.Background()
	hosted, err := Create(filepath.Join(t.TempDir(), "hosted.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer hosted.Close()
	rejectHostedValues(t, hosted.DB())
	if err := hosted.SetPassword("test", "secret"); err != nil {
		t.Fatal(err)
	}

	v1 := randomBytes(2_500_000, 1)
	v2 := bytes.Clone(v1)
	copy(v2[1_000_000:], "second version")
	r1, c1 := commitBig(t, hosted, 0, v1)
	_, c2 := commitBig(t, hosted, r1, v2)
	wantFile(t, hosted, c1, v1)
	wantFile(t, hosted, c2, v2)
	if err := hosted.Verify(); err != nil {
		t.Fatalf("hosted Verify: %v", err)
	}
	// v1 was rewritten as a delta against v2, which dropped its parts.
	wantChunked(t, hosted.DB(), 1)

	// The local store has no limit: what it clones is stored inline.
	blob.SetMaxValueBytes(0)
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return hosted.HandleSync(ctx, payload)
	})
	local, _, err := Clone(ctx, filepath.Join(t.TempDir(), "local.fossil"), peer, CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer local.Close()
	if hasBlobParts(t, local.DB()) {
		t.Fatal("an unlimited clone created blobpart")
	}
	wantFile(t, local, c1, v1)
	wantFile(t, local, c2, v2)
	if err := local.Verify(); err != nil {
		t.Fatalf("local Verify: %v", err)
	}

	v3 := randomBytes(2_600_000, 3)
	parent, err := local.ResolveVersion(c2)
	if err != nil {
		t.Fatal(err)
	}
	_, c3 := commitBig(t, local, parent, v3)
	blob.SetMaxValueBytes(hostedValueLimit)
	if _, err := local.Sync(ctx, peer, SyncOpts{Push: true, Pull: true,
		User: "test", Password: "secret"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	wantFile(t, hosted, c3, v3)
	if err := hosted.Verify(); err != nil {
		t.Fatalf("hosted Verify after push: %v", err)
	}
	if _, err := verify.Rebuild(hosted.inner); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := hosted.Verify(); err != nil {
		t.Fatalf("hosted Verify after rebuild: %v", err)
	}
	wantFile(t, hosted, c1, v1)
	wantChunked(t, hosted.DB(), 2)
	if n := largestValue(t, hosted.DB()); n > hostedValueLimit {
		t.Fatalf("hosted store holds a %d-byte value, over %d", n, hostedValueLimit)
	}
}
