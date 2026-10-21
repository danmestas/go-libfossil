package conformance

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// §3.5 with §6.4's policy form: widening a checkout over a sparse store
// fetches what the cone newly covers from a peer and materializes it. With no
// peer the widening refuses, names the elided class, and changes nothing.

func scWidenSparseCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.3.5.widen-sparse", Req: "SC-3.5-2"}, scWidenSparse},
		{Case{ID: "sparsecone.3.5.widen-no-peer", Req: "SC-3.5-2"}, scWidenNoPeer},
		{Case{ID: "sparsecone.3.5.widen-peer-lacks", Req: "SC-3.5-2"}, scWidenPeerLacks},
	}
}

type scWidenFixture struct {
	clone  *gofossil.Repo
	co     *gofossil.Checkout
	peer   gofossil.Transport
	elided map[string]bool
	tools  map[string]bool
}

// newSCWidenFixture serves a dense origin of scFiles and checks out its
// sparse clone at scDefault, the store sparse at the same cone.
func newSCWidenFixture(t *testing.T) *scWidenFixture {
	t.Helper()
	origin, err := gofossil.Create(filepath.Join(t.TempDir(), "origin.fossil"),
		gofossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { origin.Close() })
	names := make([]string, 0, len(scFiles))
	for name := range scFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	var files []gofossil.FileToCommit
	for _, name := range names {
		files = append(files, gofossil.FileToCommit{Name: name, Content: []byte(scFiles[name])})
	}
	if _, _, err := origin.Commit(gofossil.CommitOpts{Files: files, User: "u", Comment: "c"}); err != nil {
		t.Fatal(err)
	}
	f := &scWidenFixture{peer: gofossil.TransportFunc(origin.HandleSync)}
	f.clone = f.sparseClone(t)
	f.open(t)
	return f
}

// sparseClone clones the fixture's peer and makes the clone sparse at scDefault.
func (f *scWidenFixture) sparseClone(t *testing.T) *gofossil.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sparse.fossil")
	clone, _, err := gofossil.Clone(context.Background(), path, f.peer, gofossil.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := clone.Close(); err != nil {
		t.Fatal(err)
	}
	f.sparsify(t, path)
	if clone, err = gofossil.Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clone.Close() })
	return clone
}

func (f *scWidenFixture) sparsify(t *testing.T, path string) {
	t.Helper()
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f.elided = scSparsify(t, r, scDefault(t))
	f.tools = map[string]bool{}
	var tip int64
	if err := r.DB().QueryRow("SELECT objid FROM event WHERE type='ci' ORDER BY mtime DESC LIMIT 1").
		Scan(&tip); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tools/t.c", "tools/x/y/z.txt"} {
		uuid := scUUIDOf(t, r, fsltype.FslID(tip), name)
		if !f.elided[uuid] {
			t.Fatalf("fixture: %s not elided", name)
		}
		f.tools[uuid] = true
	}
}

func (f *scWidenFixture) open(t *testing.T) {
	t.Helper()
	narrow, err := gofossil.NewCone([]string{"src", "src/vendor/keep"}, []string{"src/vendor"})
	if err != nil {
		t.Fatal(err)
	}
	tip, err := f.clone.ResolveVersion("trunk")
	if err != nil {
		t.Fatal(err)
	}
	if f.co, err = f.clone.CreateCheckout(t.TempDir(), gofossil.CheckoutCreateOpts{Cone: narrow}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.co.Close() })
	if err := f.co.Extract(tip, gofossil.ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
}

func (f *scWidenFixture) wider(t *testing.T) *gofossil.Cone {
	t.Helper()
	wider, err := f.co.Cone().With([]string{"tools"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return wider
}

// held reports which of the originally elided payloads the store now holds.
func (f *scWidenFixture) held(t *testing.T) map[string]bool {
	t.Helper()
	path := f.clone.Path()
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	held := map[string]bool{}
	for uuid := range f.elided {
		if _, ok := content.AvailableByUUID(r.DB(), uuid); ok {
			held[uuid] = true
		}
	}
	return held
}

func scWidenSparse(t *testing.T) error {
	f := newSCWidenFixture(t)
	change, err := f.co.SetConeContext(context.Background(), f.wider(t),
		gofossil.RequiredContentOpts{Peer: f.peer})
	if err != nil {
		return err
	}
	want := []string{"tools/t.c", "tools/x/y/z.txt"}
	if err := scSameList("widening materialized", change.Materialized, want); err != nil {
		return err
	}
	for _, name := range want {
		data, err := os.ReadFile(filepath.Join(f.co.Dir(), name))
		if err != nil {
			return err
		}
		if string(data) != scFiles[name] {
			return expectf("widened %s holds %q, want %q", name, data, scFiles[name])
		}
	}
	held := f.held(t)
	for uuid := range f.elided {
		if held[uuid] != f.tools[uuid] {
			return expectf("after widening %s held=%v, newly covered=%v", uuid, held[uuid], f.tools[uuid])
		}
	}
	return scSameList("on disk", scOnDisk(t, f.co.Dir()), append(append([]string{}, scDefaultCovered...), want...))
}

func scWidenNoPeer(t *testing.T) error {
	f := newSCWidenFixture(t)
	before := scOnDisk(t, f.co.Dir())
	coneBefore := f.co.Cone().String()
	_, err := f.co.SetConeContext(context.Background(), f.wider(t), gofossil.RequiredContentOpts{})
	if err == nil {
		return expectf("widening a sparse store with no peer succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "elided") || !strings.Contains(msg, "no configured peer") {
		return expectf("no-peer refusal %q does not name the elided class and the absent peer", msg)
	}
	if strings.Contains(msg, "missing") || strings.Contains(msg, "corrupt") {
		return expectf("no-peer refusal %q describes elision as damage", msg)
	}
	if got := f.co.Cone().String(); got != coneBefore {
		return expectf("refused widening changed the checkout cone: %q -> %q", coneBefore, got)
	}
	if held := f.held(t); len(held) != 0 {
		return expectf("refused widening fetched %d payload(s)", len(held))
	}
	if err := f.storeCone(t, scDefault(t).Digest()); err != nil {
		return err
	}
	return scSameList("on disk after refusal", scOnDisk(t, f.co.Dir()), before)
}

// A peer that is itself sparse at the old cone cannot supply what widening
// needs: the widening refuses, naming the unavailable content, and the
// checkout keeps its cone and files.
func scWidenPeerLacks(t *testing.T) error {
	f := newSCWidenFixture(t)
	lacking := f.sparseClone(t)
	before := scOnDisk(t, f.co.Dir())
	coneBefore := f.co.Cone().String()
	_, err := f.co.SetConeContext(context.Background(), f.wider(t),
		gofossil.RequiredContentOpts{Peer: gofossil.TransportFunc(lacking.HandleSync)})
	if err == nil {
		return expectf("widening succeeded from a peer lacking the newly covered payloads")
	}
	if !strings.Contains(err.Error(), "cannot supply") {
		return expectf("failed widening %q does not identify the unavailable content", err)
	}
	if got := f.co.Cone().String(); got != coneBefore {
		return expectf("failed widening changed the checkout cone: %q -> %q", coneBefore, got)
	}
	return scSameList("on disk after failed widening", scOnDisk(t, f.co.Dir()), before)
}

func (f *scWidenFixture) storeCone(t *testing.T, want string) error {
	t.Helper()
	r, err := repo.Open(f.clone.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := content.StoreCone(r.DB())
	if err != nil {
		return err
	}
	if got == nil || got.Digest() != want {
		return expectf("refused widening changed the store cone")
	}
	return nil
}
