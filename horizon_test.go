package libfossil

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/tag"
)

const horizonTestHash = "0123456789abcdef0123456789abcdef01234567"

func TestNewHorizonConeValidation(t *testing.T) {
	for _, bad := range []string{"", "abc", horizonTestHash[:39], strings.Repeat("g", 40), strings.Repeat("a", 41)} {
		if _, err := NewHorizonCone(nil, nil, Horizon{Checkin: bad}); err == nil {
			t.Errorf("checkin %q accepted", bad)
		}
	}
	if _, err := NewHorizonCone(nil, nil, Horizon{Checkin: horizonTestHash, Rule: HorizonRule(7)}); err == nil {
		t.Error("unknown rule accepted")
	}
	c, err := NewHorizonCone(nil, nil, Horizon{Checkin: strings.ToUpper(horizonTestHash)})
	if err != nil {
		t.Fatal(err)
	}
	h, ok := c.Horizon()
	if !ok || h.Checkin != horizonTestHash || h.Rule != HorizonDescendants {
		t.Fatalf("Horizon() = %+v, %v", h, ok)
	}
}

func TestHorizonOnlyConeCoversEverything(t *testing.T) {
	c, err := NewHorizonCone(nil, nil, Horizon{Checkin: horizonTestHash, Rule: HorizonLeaves})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Include()) != 0 || !c.Covers("any/path") {
		t.Fatalf("horizon-only cone: include=%v covers=%v", c.Include(), c.Covers("any/path"))
	}
	want := "horizon " + horizonTestHash + " leaves content"
	if got := c.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	h, _ := c.Horizon()
	if h.Rule != HorizonLeaves {
		t.Fatalf("rule = %v", h.Rule)
	}
}

func TestPathConeHasNoHorizon(t *testing.T) {
	c, err := NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := c.Horizon(); ok {
		t.Fatalf("path cone reports horizon %+v", h)
	}
}

func TestWithHorizonAddsAndDrops(t *testing.T) {
	c, err := NewCone([]string{"src"}, []string{"src/gen"})
	if err != nil {
		t.Fatal(err)
	}
	h := Horizon{Checkin: horizonTestHash}
	withH, err := c.WithHorizon(&h)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := withH.Horizon()
	if !ok || got != h {
		t.Fatalf("Horizon() = %+v, %v", got, ok)
	}
	if strings.Join(withH.Include(), ",") != "src" || strings.Join(withH.Exclude(), ",") != "src/gen" {
		t.Fatalf("members lost: %v %v", withH.Include(), withH.Exclude())
	}
	if withH.Digest() == c.Digest() {
		t.Fatal("horizon did not change the digest")
	}
	if _, ok := c.Horizon(); ok {
		t.Fatal("WithHorizon mutated its receiver")
	}
	back, err := withH.WithHorizon(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.Horizon(); ok || back.Digest() != c.Digest() {
		t.Fatalf("drop did not restore the path cone: %v", back)
	}
	if _, err := withH.WithHorizon(&Horizon{Checkin: "bad"}); err == nil {
		t.Fatal("bad horizon accepted")
	}
}

func TestWithHorizonNilOnHorizonOnlyRefuses(t *testing.T) {
	c, err := NewHorizonCone(nil, nil, Horizon{Checkin: horizonTestHash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WithHorizon(nil); err == nil {
		t.Fatal("dropping the only member left a cone")
	}
}

func TestPathMembersKeepHorizon(t *testing.T) {
	c, err := NewHorizonCone([]string{"src"}, nil, Horizon{Checkin: horizonTestHash})
	if err != nil {
		t.Fatal(err)
	}
	wider, err := c.With([]string{"doc"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wider.Horizon(); !ok {
		t.Fatal("With dropped the horizon")
	}
}

func TestStoreConeHorizonPersists(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c, err := NewHorizonCone([]string{"src"}, nil, Horizon{Checkin: horizonTestHash, Rule: HorizonLeaves})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetStoreCone(c); err != nil {
		t.Fatal(err)
	}
	got, err := r.StoreCone()
	if err != nil {
		t.Fatal(err)
	}
	h, ok := got.Horizon()
	if !ok || h.Checkin != horizonTestHash || h.Rule != HorizonLeaves || got.Digest() != c.Digest() {
		t.Fatalf("store cone = %v (%+v, %v)", got, h, ok)
	}
}

func TestCheckoutRefusesHorizon(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	hc, err := NewHorizonCone([]string{"src"}, nil, Horizon{Checkin: horizonTestHash})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CreateCheckout(t.TempDir(), CheckoutCreateOpts{Cone: hc})
	if !errors.Is(err, ErrHorizonCheckout) {
		t.Fatalf("CreateCheckout error = %v, want ErrHorizonCheckout", err)
	}
	co, err := r.CreateCheckout(t.TempDir(), CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if _, err := co.SetCone(hc); !errors.Is(err, ErrHorizonCheckout) {
		t.Fatalf("SetCone error = %v, want ErrHorizonCheckout", err)
	}
	// The refusal precedes any store-policy change.
	_, err = co.SetConeContext(context.Background(), hc, RequiredContentOpts{})
	if !errors.Is(err, ErrHorizonCheckout) {
		t.Fatalf("SetConeContext error = %v, want ErrHorizonCheckout", err)
	}
	if sc, _ := r.StoreCone(); sc != nil {
		t.Fatalf("refused checkout cone changed the store cone: %v", sc)
	}
}

// horizonFixture is a source with three check-ins; "old a" is named only by the
// first, so a horizon at the second elides it.
type horizonFixture struct {
	source *Repo
	peer   Transport
	c1, c2 string
}

func newHorizonFixture(t *testing.T) horizonFixture {
	t.Helper()
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	commit := func(parent int64, files ...FileToCommit) (int64, string) {
		rid, uuid, err := source.Commit(CommitOpts{User: "test", ParentID: parent, Files: files})
		if err != nil {
			t.Fatal(err)
		}
		return rid, uuid
	}
	r1, c1 := commit(0, FileToCommit{Name: "a", Content: []byte("old a\n")}, FileToCommit{Name: "b", Content: []byte("shared b\n")})
	r2, c2 := commit(r1, FileToCommit{Name: "a", Content: []byte("new a\n")}, FileToCommit{Name: "b", Content: []byte("shared b\n")})
	commit(r2, FileToCommit{Name: "a", Content: []byte("newest a\n")}, FileToCommit{Name: "b", Content: []byte("shared b\n")})
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return source.HandleSync(ctx, payload)
	})
	return horizonFixture{source: source, peer: peer, c1: c1, c2: c2}
}

func holdsBlob(t *testing.T, r *Repo, data string) bool {
	t.Helper()
	_, held, err := content.CheckAvailableByUUID(r.inner.DB(), hash.SHA1([]byte(data)))
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func TestCloneIntoHorizonStore(t *testing.T) {
	f := newHorizonFixture(t)
	hc, err := NewHorizonCone(nil, nil, Horizon{Checkin: f.c2})
	if err != nil {
		t.Fatal(err)
	}
	target, res, err := Clone(context.Background(), filepath.Join(t.TempDir(), "t.fossil"), f.peer, CloneOpts{StoreCone: hc})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if res.HorizonServed == 0 {
		t.Fatalf("HorizonServed = 0: %+v", res)
	}
	if holdsBlob(t, target, "old a\n") {
		t.Fatal("blob below the horizon was transferred")
	}
	for _, kept := range []string{"new a\n", "newest a\n", "shared b\n"} {
		if !holdsBlob(t, target, kept) {
			t.Fatalf("blob %q at or above the horizon is missing", kept)
		}
	}
	got, err := target.StoreCone()
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := got.Horizon(); !ok || h.Checkin != f.c2 {
		t.Fatalf("store cone horizon = %+v, %v", h, ok)
	}
}

func TestSetStoreConeContextDeepensHorizon(t *testing.T) {
	f := newHorizonFixture(t)
	shallow, err := NewHorizonCone(nil, nil, Horizon{Checkin: f.c2})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "t.fossil"), f.peer, CloneOpts{StoreCone: shallow})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	deep, err := shallow.WithHorizon(&Horizon{Checkin: f.c1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.SetStoreConeContext(context.Background(), deep, RequiredContentOpts{}); err == nil {
		t.Fatal("deepening without a peer succeeded")
	}
	if holdsBlob(t, target, "old a\n") {
		t.Fatal("refused deepening fetched content")
	}
	res, err := target.SetStoreConeContext(context.Background(), deep, RequiredContentOpts{Peer: f.peer})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.FilesRecvd == 0 || !holdsBlob(t, target, "old a\n") {
		t.Fatalf("deepening fetched nothing: %+v", res)
	}
	// Dropping the horizon widens to dense and needs no further content.
	if _, err := target.SetStoreConeContext(context.Background(), nil, RequiredContentOpts{Peer: f.peer}); err != nil {
		t.Fatal(err)
	}
	if sc, _ := target.StoreCone(); sc != nil {
		t.Fatalf("store cone = %v, want dense", sc)
	}
}

func TestRequiredStoreUnionKeepsStoreHorizon(t *testing.T) {
	h := Horizon{Checkin: horizonTestHash}
	only, _ := NewHorizonCone(nil, nil, h)
	pathed, _ := NewHorizonCone([]string{"src"}, nil, h)
	checkout, _ := NewCone([]string{"doc"}, nil)

	got, err := requiredStoreUnion(only, checkout)
	if err != nil || got.Digest() != only.Digest() {
		t.Fatalf("horizon-only ∪ path = %v, %v", got, err)
	}
	got, err = requiredStoreUnion(pathed, checkout)
	if err != nil {
		t.Fatal(err)
	}
	if hh, ok := got.Horizon(); !ok || hh != h || strings.Join(got.Include(), ",") != "doc,src" {
		t.Fatalf("pathed ∪ path = %v", got)
	}
	got, err = requiredStoreUnion(pathed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hh, ok := got.Horizon(); !ok || hh != h || len(got.Include()) != 0 {
		t.Fatalf("pathed ∪ dense = %v", got)
	}
	if got, err = requiredStoreUnion(nil, checkout); err != nil || got != nil {
		t.Fatalf("dense ∪ path = %v, %v", got, err)
	}
	if _, err := requiredStoreUnion(checkout, pathed); err == nil {
		t.Fatal("a horizon on the widening side was accepted")
	}
}

// A sync over the HTTP transport keys the horizon cursor by the peer's URL,
// so the next request to that peer can send it (draft-fossil-horizon-00
// §6.2.6), and the key never carries the URL's password. Method: clone a
// horizon store over HTTP, pull with a URL that carries credentials, and
// read the cursor back under the password-free URL.
func TestSyncKeysHorizonCursorByURL(t *testing.T) {
	f := newHorizonFixture(t)
	server := httptest.NewServer(f.source.XferHandler())
	defer server.Close()
	hc, err := NewHorizonCone(nil, nil, Horizon{Checkin: f.c2})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "t.fossil"),
		NewHTTPTransport(server.URL), CloneOpts{StoreCone: hc})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("anon", "s3cret")
	if _, err := target.Sync(context.Background(), NewHTTPTransport(u.String()), SyncOpts{Pull: true}); err != nil {
		t.Fatal(err)
	}
	u.User = url.User("anon")
	cursor, err := target.Config("horizon-cursor:" + u.String())
	if err != nil {
		t.Fatal(err)
	}
	if cursor == "" {
		t.Fatalf("no horizon cursor recorded for %s", u)
	}
	var leaked int
	if err := target.inner.DB().QueryRow(
		"SELECT count(*) FROM config WHERE name LIKE '%s3cret%' OR value LIKE '%s3cret%'").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("%d config rows carry the URL's password", leaked)
	}
}

// The structural cut is the Structural flag; the zero value is the content
// horizon, so a Horizon built before the flag existed means what it meant.
func TestStructuralHorizonCone(t *testing.T) {
	c, err := NewHorizonCone([]string{"src"}, nil, Horizon{Checkin: horizonTestHash, Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	h, ok := c.Horizon()
	if !ok || !h.Structural || h.Checkin != horizonTestHash {
		t.Fatalf("Horizon() = %+v, %v", h, ok)
	}
	if got, want := c.String(), "horizon "+horizonTestHash+" descendants structural, src"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	content, err := c.WithHorizon(&Horizon{Checkin: horizonTestHash})
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := content.Horizon(); h.Structural {
		t.Fatal("a Horizon without Structural is structural")
	}
}

// A clone into a structural store holds no manifest below the horizon, says
// how its rounds were served, and keeps the depth as its policy.
func TestCloneIntoStructuralStore(t *testing.T) {
	f := newHorizonFixture(t)
	hc, err := NewHorizonCone(nil, nil, Horizon{Checkin: f.c2, Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	target, res, err := Clone(context.Background(), filepath.Join(t.TempDir(), "t.fossil"), f.peer,
		CloneOpts{StoreCone: hc, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if res.StructuralServed == 0 || res.HorizonFallback {
		t.Fatalf("StructuralServed = %d, HorizonFallback = %v", res.StructuralServed, res.HorizonFallback)
	}
	_, held, err := content.CheckAvailableByUUID(target.inner.DB(), f.c1)
	if err != nil || held {
		t.Fatalf("the manifest below the horizon is held (%v)", err)
	}
	got, err := target.StoreCone()
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := got.Horizon(); !ok || !h.Structural {
		t.Fatalf("store cone horizon = %+v, %v", h, ok)
	}
	sres, err := target.Sync(context.Background(), f.peer, SyncOpts{Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if sres.StructuralServed == 0 || sres.HorizonFallback {
		t.Fatalf("pull: StructuralServed = %d, HorizonFallback = %v", sres.StructuralServed, sres.HorizonFallback)
	}
}

// DeepenHorizon moves a store's horizon to the check-in named and fetches
// what the deeper horizon keeps, keeping the depth (draft-fossil-horizon-00
// §5.8). It needs a peer and a horizon to deepen.
func TestDeepenHorizon(t *testing.T) {
	f := newHorizonFixture(t)
	hc, err := NewHorizonCone(nil, nil, Horizon{Checkin: f.c2, Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "t.fossil"), f.peer,
		CloneOpts{StoreCone: hc, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := target.DeepenHorizon(context.Background(), f.c1, RequiredContentOpts{}); err == nil {
		t.Fatal("deepening without a peer succeeded")
	}
	res, err := target.DeepenHorizon(context.Background(), f.c1, RequiredContentOpts{Peer: f.peer})
	if err != nil {
		t.Fatal(err)
	}
	if res.StructuralServed == 0 || res.HorizonFallback {
		t.Fatalf("deepen: StructuralServed = %d, HorizonFallback = %v", res.StructuralServed, res.HorizonFallback)
	}
	got, err := target.StoreCone()
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := got.Horizon(); !ok || h.Checkin != f.c1 || !h.Structural {
		t.Fatalf("store cone horizon = %+v, %v; want %s structural", h, ok, f.c1)
	}
	if _, held, err := content.CheckAvailableByUUID(target.inner.DB(), f.c1); err != nil || !held {
		t.Fatalf("the deepened-to manifest is not held (%v)", err)
	}
	if !holdsBlob(t, target, "old a\n") {
		t.Fatal("the deepen fetched no blob the deeper horizon keeps")
	}
	dense, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "d.fossil"), f.peer, CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer dense.Close()
	if _, err := dense.DeepenHorizon(context.Background(), f.c1, RequiredContentOpts{Peer: f.peer}); err == nil {
		t.Fatal("deepening a store with no horizon succeeded")
	}
}

// A branch merge whose base lies below the store's structural horizon is
// refused naming the horizon, before any commit (draft-fossil-horizon-00
// §5.5): f, feat's first check-in, forks below the horizon at c2; feat's tip
// merges c2 in and trunk's tip merges f, so the base of the two tips is f.
func TestBranchMergeRefusesBaseBelowHorizon(t *testing.T) {
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	commit := func(a string, parents ...int64) (int64, string) {
		rid, uuid, err := source.Commit(CommitOpts{User: "test", ParentID: parents[0], MergeParents: parents[1:],
			Files: []FileToCommit{{Name: "a", Content: []byte(a)}}})
		if err != nil {
			t.Fatal(err)
		}
		return rid, uuid
	}
	r1, _ := commit("one\n", 0)
	rf, _ := commit("one\nfork\n", r1)
	for _, tc := range []struct {
		name string
		typ  int
	}{{"branch", tag.TagPropagating}, {"sym-feat", tag.TagPropagating}, {"sym-trunk", tag.TagCancel}} {
		value := ""
		if tc.name == "branch" {
			value = "feat"
		}
		if _, err := tag.AddTag(source.inner, tag.TagOpts{TargetRID: fsltype.FslID(rf), TagName: tc.name,
			TagType: tc.typ, Value: value, User: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	r2, c2 := commit("zero\none\n", r1)
	ra, _ := commit("zero\none\nA\n", r2)
	commit("zero\none\nfork\n", rf, r2)
	commit("zero\none\nA\nfork\n", ra, rf)
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return source.HandleSync(ctx, payload)
	})
	hc, err := NewHorizonCone(nil, nil, Horizon{Checkin: c2, Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	clone, res, err := Clone(context.Background(), filepath.Join(t.TempDir(), "s.fossil"), peer,
		CloneOpts{StoreCone: hc, PullPhantoms: true})
	if err != nil || res.HorizonFallback {
		t.Fatalf("structural clone: %v %+v", err, res)
	}
	defer clone.Close()
	_, _, err = clone.Merge("feat", "trunk", "merge feat", "test")
	if !errors.Is(err, ErrBelowHorizon) || !strings.Contains(err.Error(), "below the structural horizon at "+c2) {
		t.Fatalf("branch merge across the horizon: %v, want ErrBelowHorizon naming %s", err, c2)
	}
}

// A symbolic name on a check-in below a structural horizon resolves to that
// check-in through the repository and a checkout alike, because the tag's
// control artifact is held (draft-fossil-horizon-00 §5.5).
func TestResolveNameBelowHorizon(t *testing.T) {
	f := newHorizonFixture(t)
	r1, err := f.source.ResolveVersion(f.c1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.source.Tag(TagOpts{Name: "old", TargetID: r1, User: "test"}); err != nil {
		t.Fatal(err)
	}
	hc, err := NewHorizonCone(nil, nil, Horizon{Checkin: f.c2, Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "s.fossil"), f.peer,
		CloneOpts{StoreCone: hc, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	co, err := clone.CreateCheckout(t.TempDir(), CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	for name, resolve := range map[string]func(string) (int64, error){
		"repo": clone.ResolveVersion, "checkout": co.ResolveVersion,
	} {
		rid, err := resolve("old")
		if err != nil {
			t.Fatalf("%s: old does not resolve: %v", name, err)
		}
		if uuid, err := clone.UUIDFromRID(rid); err != nil || uuid != f.c1 {
			t.Fatalf("%s: old resolves to %s (%v), want %s", name, uuid, err, f.c1)
		}
	}
	if _, err := clone.ResolveVersion("missing"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("an unknown name: %v, want ErrVersionNotFound", err)
	}
}
