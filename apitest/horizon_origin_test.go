package apitest_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil"
)

// embedderTransport is a Transport defined outside the package, as an
// embedder's NATS or in-process transport is: it reaches the origin directly
// and names it by a stable string.
type embedderTransport struct {
	origin *libfossil.Repo
	name   string
}

func (t embedderTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	return t.origin.HandleSync(ctx, payload)
}

func (t embedderTransport) SyncOrigin() string { return t.name }

var _ libfossil.OriginNamer = embedderTransport{}

// igotCounter counts the igot cards every request of a sync announces.
type igotCounter struct {
	libfossil.SyncObserver
	igots int
}

func (c *igotCounter) RoundCompleted(_ int, stats libfossil.RoundStats) { c.igots += stats.IGots }

// newOriginFixture is an origin of three check-ins and a store cone at the
// horizon of the second.
func newOriginFixture(t *testing.T) (*libfossil.Repo, *libfossil.Cone) {
	t.Helper()
	origin, err := libfossil.Create(filepath.Join(t.TempDir(), "origin.fossil"),
		libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { origin.Close() })
	var parent int64
	var horizon string
	for i, body := range []string{"one\n", "two\n", "three\n"} {
		rid, uuid, err := origin.Commit(libfossil.CommitOpts{User: "test", ParentID: parent,
			Files: []libfossil.FileToCommit{{Name: "a", Content: []byte(body)}}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			horizon = uuid
		}
		parent = rid
	}
	cone, err := libfossil.NewHorizonCone(nil, nil, libfossil.Horizon{Checkin: horizon})
	if err != nil {
		t.Fatal(err)
	}
	return origin, cone
}

// cloneThenPull clones a horizon store over t and pulls once more, returning
// the clone and the igot cards the pull announced.
func cloneThenPull(t *testing.T, origin *libfossil.Repo, cone *libfossil.Cone,
	tr libfossil.Transport) (*libfossil.Repo, int) {
	t.Helper()
	clone, _, err := libfossil.Clone(context.Background(),
		filepath.Join(t.TempDir(), "clone.fossil"), tr, libfossil.CloneOpts{StoreCone: cone})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clone.Close() })
	counter := &igotCounter{SyncObserver: libfossil.NopSyncObserver()}
	if _, err := clone.Sync(context.Background(), tr,
		libfossil.SyncOpts{Pull: true, Observer: counter}); err != nil {
		t.Fatal(err)
	}
	return clone, counter.igots
}

// A Transport an embedder defines keeps the horizon cursor of
// draft-fossil-horizon-00 §6.2.6 when it names its origin: the clone records
// the cursor under that name, and the pull that follows a converged exchange
// announces nothing with igot.
func TestEmbedderTransportKeepsHorizonCursor(t *testing.T) {
	origin, cone := newOriginFixture(t)
	const name = "nats://origin.example/project"
	clone, igots := cloneThenPull(t, origin, cone, embedderTransport{origin: origin, name: name})
	cursor, err := clone.Config("horizon-cursor:" + name)
	if err != nil {
		t.Fatal(err)
	}
	if cursor == "" {
		t.Fatalf("no horizon cursor recorded for %s", name)
	}
	if igots != 0 {
		t.Fatalf("pull under a cursor announced %d igot cards, want 0", igots)
	}
}

// A Transport that names no origin keeps no cursor, and its pull announces
// the client's whole holding, which §6.2.6 allows.
func TestUnnamedTransportKeepsNoHorizonCursor(t *testing.T) {
	origin, cone := newOriginFixture(t)
	tr := libfossil.TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return origin.HandleSync(ctx, payload)
	})
	if _, ok := libfossil.Transport(tr).(libfossil.OriginNamer); ok {
		t.Fatal("TransportFunc names an origin")
	}
	_, igots := cloneThenPull(t, origin, cone, tr)
	if igots == 0 {
		t.Fatal("pull without a cursor announced nothing")
	}
}
