package libfossil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/content"
)

func requiredUpdateFixture(t *testing.T) (requiredPublicFixture, *Checkout, int64, string) {
	t.Helper()
	f := newRequiredPublicFixture(t, "sha3")
	co := requiredPublicCheckout(t, f)
	parent, err := f.source.ResolveVersion("tip")
	if err != nil {
		t.Fatal(err)
	}
	_, uuid, err := f.source.Commit(CommitOpts{ParentID: parent, User: "test",
		Files: []FileToCommit{{Name: "f0000", Content: []byte("updated payload")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.Sync(context.Background(), f.peer, SyncOpts{Pull: true}); err != nil {
		t.Fatal(err)
	}
	target, found := blob.Exists(f.target.inner.DB(), uuid)
	if !found {
		t.Fatal("update target not synchronized")
	}
	var payload string
	if err := f.source.inner.DB().QueryRow(`SELECT uuid FROM blob WHERE rid IN
		(SELECT fid FROM mlink WHERE mid IN (SELECT rid FROM blob WHERE uuid=?)
		AND fnid IN (SELECT fnid FROM filename WHERE name='f0000'))`, uuid).
		Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.inner.DB().Exec(
		"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", payload); err != nil {
		t.Fatal(err)
	}
	return f, co, int64(target), payload
}

// An update fetches exactly the covered target payload, never arbitrary outside
// F bytes. An implicit target is resolved before recovery and reused locally.
func TestUpdateContextRequiredPayload(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		f, co, target, payload := requiredUpdateFixture(t)
		requested := false
		peer := TransportFunc(func(ctx context.Context, data []byte) ([]byte, error) {
			requested = requested || requiredRequestsPayload(t, data, payload)
			for _, outside := range f.outside {
				if requiredRequestsPayload(t, data, outside) {
					t.Fatal("update fetched an outside F payload")
				}
			}
			return f.source.HandleSync(ctx, data)
		})
		opts := UpdateOpts{TargetRID: target}
		if implicit {
			opts.TargetRID = 0
		}
		_, err := co.UpdateContext(context.Background(), opts, RequiredContentOpts{Peer: peer})
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(co.Dir(), "f0000"))
		if err != nil || string(got) != "updated payload" || !requested {
			t.Fatalf("update result: %q, requested=%v, %v", got, requested, err)
		}
		version, _, err := co.Version()
		if err != nil || version != target {
			t.Fatalf("update version: %d, want %d, %v", version, target, err)
		}
		for _, uuid := range f.outside {
			if _, held := content.AvailableByUUID(f.target.inner.DB(), uuid); held {
				t.Fatal("outside payload became available")
			}
		}
	}
}

// A network callback can change pending selection. Recovery must reject the
// captured snapshot rather than apply an update to the changed checkout.
func TestUpdateContextRefusesChangedQueue(t *testing.T) {
	f, co, target, _ := requiredUpdateFixture(t)
	changed := false
	peer := TransportFunc(func(ctx context.Context, data []byte) ([]byte, error) {
		if !changed {
			changed = true
			if err := co.inner.Enqueue(checkout.EnqueueOpts{Paths: []string{"f0000"}}); err != nil {
				t.Fatal(err)
			}
		}
		return f.source.HandleSync(ctx, data)
	})
	_, err := co.UpdateContext(context.Background(), UpdateOpts{TargetRID: target},
		RequiredContentOpts{Peer: peer})
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("changed snapshot was not rejected: %v", err)
	}
	version, _, err := co.Version()
	if err != nil || version != f.parent {
		t.Fatalf("refused update moved version: %d, %v", version, err)
	}
	got, err := os.ReadFile(filepath.Join(co.Dir(), "f0000"))
	if err != nil || string(got) != "f0000" {
		t.Fatalf("refused update changed worktree: %q, %v", got, err)
	}
}

// No-peer refusal precedes checkout scanning and leaves pending metadata and
// working bytes untouched. A saved remote is not used by this explicit API.
func TestUpdateContextNoPeerPreservesCheckout(t *testing.T) {
	_, co, target, _ := requiredUpdateFixture(t)
	before, err := co.inner.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(co.Dir(), "f0000")
	if err := os.WriteFile(name, []byte("unscanned edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = co.UpdateContext(context.Background(), UpdateOpts{TargetRID: target},
		RequiredContentOpts{})
	if err == nil || !strings.Contains(err.Error(), "no configured peer") {
		t.Fatalf("no-peer update refusal: %v", err)
	}
	after, err := co.inner.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("refusal changed checkout metadata: %v", err)
	}
	got, err := os.ReadFile(name)
	if err != nil || string(got) != "unscanned edit" {
		t.Fatalf("refusal changed worktree: %q, %v", got, err)
	}
}
