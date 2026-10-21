package conformance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// scDemandPublic is a dense HTTP clone whose store cone is src and whose
// zzz/b content is an elided phantom, served by a counting HTTP peer.
type scDemandPublic struct {
	path     string
	url      string
	requests *atomic.Int64
	cone     *gofossil.Cone
}

func newSCDemandPublic(t *testing.T) *scDemandPublic {
	t.Helper()
	source, err := gofossil.Create(filepath.Join(t.TempDir(), "source.fossil"), gofossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.Commit(gofossil.CommitOpts{User: "u", Files: []gofossil.FileToCommit{
		{Name: "src/a", Content: []byte("inside\n")}, {Name: "zzz/b", Content: []byte("outside\n")},
	}}); err != nil {
		t.Fatal(err)
	}
	f := &scDemandPublic{path: filepath.Join(t.TempDir(), "target.fossil"), requests: &atomic.Int64{}}
	handler := source.XferHandler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	target, _, err := gofossil.Clone(context.Background(), f.path,
		gofossil.NewHTTPTransport(server.URL), gofossil.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if f.cone, err = gofossil.NewCone([]string{"src"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := target.SetStoreCone(f.cone); err != nil {
		t.Fatal(err)
	}
	uuid := hash.SHA1([]byte("outside\n"))
	for _, stmt := range []string{
		"UPDATE blob SET size=-1,content=NULL WHERE uuid=?",
		"INSERT OR IGNORE INTO phantom SELECT rid FROM blob WHERE uuid=?",
	} {
		if _, err := target.Inner().DB().Exec(stmt, uuid); err != nil {
			t.Fatal(err)
		}
	}
	f.requests.Store(0)
	return f
}

// check reports whether zzz/b is held and whether the store cone is still src.
func (f *scDemandPublic) check() (held, narrow bool, err error) {
	r, err := gofossil.Open(f.path)
	if err != nil {
		return false, false, err
	}
	defer r.Close()
	_, held, err = content.CheckAvailableByUUID(r.Inner().DB(), hash.SHA1([]byte("outside\n")))
	if err != nil {
		return false, false, err
	}
	c, err := r.StoreCone()
	if err != nil {
		return false, false, err
	}
	return held, c != nil && c.Digest() == f.cone.Digest(), nil
}

// The policy form through the public CLI, with the peer taken from the saved
// last-sync-url as an operator's widening would find it.
func scDemandSavedPeer(t *testing.T) error {
	f := newSCDemandPublic(t)
	r, err := gofossil.Open(f.path)
	if err != nil {
		return err
	}
	if err := errors.Join(r.SetConfig("last-sync-url", f.url), r.Close()); err != nil {
		return err
	}
	if err := (&cli.RepoConeStoreCmd{Dense: true}).Run(&cli.Globals{Repo: f.path}); err != nil {
		return err
	}
	held, narrow, err := f.check()
	if err != nil {
		return err
	}
	if !held || narrow || f.requests.Load() == 0 {
		return fmt.Errorf("saved-peer widening held=%v narrow=%v requests=%d", held, narrow, f.requests.Load())
	}
	return nil
}

// No peer and a cancelled context both refuse through the public operation
// before any request, leaving the policy and holdings unchanged.
func scDemandPublicRefusal(t *testing.T) error {
	f := newSCDemandPublic(t)
	r, err := gofossil.Open(f.path)
	if err != nil {
		return err
	}
	_, noPeer := r.SetStoreConeContext(context.Background(), nil, gofossil.RequiredContentOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cancelled := r.SetStoreConeContext(ctx, nil,
		gofossil.RequiredContentOpts{Peer: gofossil.NewHTTPTransport(f.url)})
	if err := r.Close(); err != nil {
		return err
	}
	want := "libfossil: set store cone: sparse cone: 1 elided artifact(s); " +
		"no configured peer to fetch 1 newly covered artifact(s)"
	if noPeer == nil || noPeer.Error() != want {
		return fmt.Errorf("no-peer error = %v, want %q", noPeer, want)
	}
	if !errors.Is(cancelled, context.Canceled) {
		return fmt.Errorf("cancelled error = %v", cancelled)
	}
	held, narrow, err := f.check()
	if err != nil {
		return err
	}
	if held || !narrow || f.requests.Load() != 0 {
		return fmt.Errorf("refusal held=%v narrow=%v requests=%d", held, narrow, f.requests.Load())
	}
	return nil
}
