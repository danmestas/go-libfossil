package libfossil

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
)

type storeConeFixture struct {
	target   *Repo
	peer     Transport
	requests *int
	cone     *Cone
}

// A dense clone narrowed to src, with zzz/b's content then made an elided
// phantom, as a cone-scoped pull would have left it.
func newStoreConeFixture(t *testing.T) storeConeFixture {
	t.Helper()
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.Commit(CommitOpts{User: "test", Files: []FileToCommit{
		{Name: "src/a", Content: []byte("inside\n")}, {Name: "zzz/b", Content: []byte("outside\n")},
	}}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		requests++
		return source.HandleSync(ctx, payload)
	})
	target, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "target.fossil"), peer, CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })
	c, err := NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.SetStoreCone(c); err != nil {
		t.Fatal(err)
	}
	uuid := hash.SHA1([]byte("outside\n"))
	for _, stmt := range []string{
		"UPDATE blob SET size=-1,content=NULL WHERE uuid=?",
		"INSERT OR IGNORE INTO phantom SELECT rid FROM blob WHERE uuid=?",
	} {
		if _, err := target.inner.DB().Exec(stmt, uuid); err != nil {
			t.Fatal(err)
		}
	}
	requests = 0
	f := storeConeFixture{target: target, peer: peer, requests: &requests, cone: c}
	if f.holdsOutside(t) {
		t.Fatal("fixture failed to elide zzz/b")
	}
	return f
}

func (f storeConeFixture) holdsOutside(t *testing.T) bool {
	t.Helper()
	_, held, err := content.CheckAvailableByUUID(f.target.inner.DB(), hash.SHA1([]byte("outside\n")))
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func (f storeConeFixture) storeConeIs(t *testing.T, want *Cone) {
	t.Helper()
	got, err := f.target.StoreCone()
	if err != nil {
		t.Fatal(err)
	}
	if (got == nil) != (want == nil) || got != nil && got.Digest() != want.Digest() {
		t.Fatalf("store cone = %v, want %v", got, want)
	}
}

// §6.4 policy form: widening updates the store cone and pulls the newly
// covered content through an ordinary cone-scoped synchronization.
func TestSetStoreConeContextFetchesFromPeer(t *testing.T) {
	f := newStoreConeFixture(t)
	result, err := f.target.SetStoreConeContext(context.Background(), nil, RequiredContentOpts{Peer: f.peer})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.FilesRecvd == 0 || *f.requests == 0 {
		t.Fatalf("widening fetched nothing: result=%+v requests=%d", result, *f.requests)
	}
	f.storeConeIs(t, nil)
	if !f.holdsOutside(t) {
		t.Fatal("newly covered content still elided")
	}
}

// §6.4: with no peer the operation refuses, names the class and keeps policy.
func TestSetStoreConeContextNoPeerRefuses(t *testing.T) {
	f := newStoreConeFixture(t)
	_, err := f.target.SetStoreConeContext(context.Background(), nil, RequiredContentOpts{})
	want := "libfossil: set store cone: sparse cone: 1 elided artifact(s); " +
		"no configured peer to fetch 1 newly covered artifact(s)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	f.storeConeIs(t, f.cone)
	if f.holdsOutside(t) {
		t.Fatal("refused widening fetched content")
	}
}

func TestSetStoreConeContextCancelled(t *testing.T) {
	f := newStoreConeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.target.SetStoreConeContext(ctx, nil, RequiredContentOpts{Peer: f.peer})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if *f.requests != 0 {
		t.Fatalf("cancelled widening made %d request(s)", *f.requests)
	}
	f.storeConeIs(t, f.cone)
}

// Narrowing needs no peer and moves no content (§5.4).
func TestSetStoreConeContextNarrowsWithoutPeer(t *testing.T) {
	f := newStoreConeFixture(t)
	narrower, err := NewCone([]string{"src/a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.SetStoreConeContext(context.Background(), narrower, RequiredContentOpts{}); err != nil {
		t.Fatal(err)
	}
	f.storeConeIs(t, narrower)
}
