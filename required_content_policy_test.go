package libfossil

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
)

// Lasting coverage must affect a later ordinary pull of a newly-created path,
// not just the exact payloads fetched while editing the worktree cone.
func TestSetConeContextFuturePull(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	co := requiredPublicCheckout(t, f)
	next, err := co.Cone().With([]string{"future"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetConeContext(context.Background(), next,
		RequiredContentOpts{Peer: f.peer}); err != nil {
		t.Fatal(err)
	}
	var parentUUID string
	if err := f.target.inner.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", f.parent).
		Scan(&parentUUID); err != nil {
		t.Fatal(err)
	}
	parent, held := blob.Exists(f.source.inner.DB(), parentUUID)
	if !held {
		t.Fatal("source parent disappeared")
	}
	_, _, err = f.source.Commit(CommitOpts{ParentID: int64(parent), User: "test",
		Files: []FileToCommit{{Name: "future/new", Content: []byte("future payload")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.Sync(context.Background(), f.peer, SyncOpts{Pull: true}); err != nil {
		t.Fatal(err)
	}
	_, uuid, err := blob.Store(f.source.inner.DB(), []byte("future payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := content.AvailableByUUID(f.target.inner.DB(), uuid); !held {
		t.Fatal("ordinary pull did not fetch a new file in widened coverage")
	}
	stored, err := f.target.StoreCone()
	if err != nil || stored == nil || !stored.Covers("future/another") || !stored.Covers("f0000") {
		t.Fatalf("lasting shared-store coverage: %v, %v", stored, err)
	}
	for _, name := range f.outside {
		if _, held := content.AvailableByUUID(f.target.inner.DB(), name); held {
			t.Fatal("widening fetched unrelated outside payload")
		}
	}
}

// A failed fetch retains explicit widening intent, as the pinned reference does,
// while no-peer refusal does not silently change policy or the worktree.
func TestSetConeContextFailedFetchPolicy(t *testing.T) {
	for _, hasPeer := range []bool{false, true} {
		f := newRequiredPublicFixture(t, "sha3")
		co := requiredPublicCheckout(t, f)
		before, err := f.target.StoreCone()
		if err != nil {
			t.Fatal(err)
		}
		var required RequiredContentOpts
		if hasPeer {
			required.Peer = TransportFunc(func(context.Context, []byte) ([]byte, error) {
				return nil, errors.New("injected fetch refusal")
			})
		}
		if _, err := co.SetConeContext(context.Background(), nil, required); err == nil {
			t.Fatal("widening unexpectedly succeeded")
		}
		after, err := f.target.StoreCone()
		if err != nil {
			t.Fatal(err)
		}
		if hasPeer {
			if after != nil {
				t.Fatal("failed fetch discarded explicit dense store intent")
			}
		} else if after == nil || after.Digest() != before.Digest() {
			t.Fatal("no-peer refusal changed store policy")
		}
		if co.Cone() == nil {
			t.Fatal("failed fetch changed worktree coverage")
		}
	}
}

// A widening combines coverage, not raw members: one checkout's exclusions
// must never take coverage away from another checkout sharing its store.
func TestRequiredStoreUnion(t *testing.T) {
	left, err := NewCone([]string{"a", "a/cut/back", "left"}, []string{"a/cut"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewCone([]string{"a/cut", "right"}, []string{"a/cut/hole"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := requiredStoreUnion(left, right)
	if err != nil {
		t.Fatal(err)
	}
	for name, covered := range map[string]bool{
		"a/file": true, "a/cut/file": true, "a/cut/hole/file": false,
		"a/cut/back/file": true, "left/new": true, "right/new": true, "other": false,
	} {
		if got.Covers(name) != covered {
			t.Fatalf("union coverage of %s: got %v, want %v", name, got.Covers(name), covered)
		}
	}
	for _, pair := range [][2]*Cone{{left, nil}, {nil, right}, {nil, nil}} {
		dense, err := requiredStoreUnion(pair[0], pair[1])
		if err != nil || dense != nil {
			t.Fatalf("union with dense: %v, %v", dense, err)
		}
	}
}
