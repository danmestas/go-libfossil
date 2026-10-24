package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestCloneWithholdsDanglingDelta pins that an origin holding a delta whose
// source is a phantom can still be cloned. It pushes an ordinary file and a
// delta against a base the origin never receives, then clones. The origin
// cannot expand the delta, so it withholds it, and the ordinary file arrives
// whole. Before, the origin tried to expand the delta, failed, and the clone
// stopped at clone sequence 1.
func TestCloneWithholdsDanglingDelta(t *testing.T) {
	origin := setupPushableTestRepo(t)
	base := []byte("missing base\n")
	target := []byte("target content\n")
	d, err := delta.Create(base, target)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := []byte("ordinary\n")
	if _, err := HandleSync(context.Background(), origin, &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "s", ProjectCode: testPC},
		&xfer.FileCard{UUID: hash.SHA1(ordinary), Content: ordinary},
		&xfer.FileCard{UUID: hash.SHA1(target), DeltaSrc: hash.SHA1(base), Content: d},
	}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clone, _, err := Clone(ctx, filepath.Join(t.TempDir(), "c.fossil"), handlerTransport(t, origin), CloneOpts{})
	if err != nil {
		t.Fatalf("clone of an origin holding a dangling delta: %v", err)
	}
	defer clone.Close()

	rid, ok := blob.Exists(clone.DB(), hash.SHA1(ordinary))
	if !ok {
		t.Fatal("ordinary file not cloned")
	}
	got, err := content.Expand(clone.DB(), rid)
	if err != nil || string(got) != string(ordinary) {
		t.Fatalf("ordinary file: %q, %v", got, err)
	}
	if _, ok := blob.Exists(clone.DB(), hash.SHA1(target)); ok {
		t.Fatal("the dangling delta was cloned; it cannot be expanded and must be withheld")
	}

	// A later pull must not fail on it either.
	serverCode, err := clone.Config("server-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(ctx, clone, handlerTransport(t, origin), SyncOpts{
		Pull: true, ProjectCode: testPC, ServerCode: serverCode,
	}); err != nil {
		t.Fatalf("pull after the clone: %v", err)
	}
}
