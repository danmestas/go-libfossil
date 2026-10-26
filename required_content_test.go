package libfossil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

type requiredPublicFixture struct {
	source, target *Repo
	parent         int64
	peer           Transport
	outside        []string
}

// Both hash families must recover only S structure through the public byte
// transport; unchanged outside F payloads remain genuinely unavailable.
func newRequiredPublicFixture(t *testing.T, policy string) requiredPublicFixture {
	t.Helper()
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.inner.SetHashPolicy(policy); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
	} {
		if err := source.inner.SetConfig(key, value); err != nil {
			t.Fatal(err)
		}
	}
	var files []FileToCommit
	for _, name := range []string{"f0000", "f0003", "f0004", "f0005", "f0006", "f0008",
		"f0010", "f0014", "f0015", "f0016", "f0017", "f0018", "f0019", "f0021",
		"f0022", "f0023", "f0024"} {
		files = append(files, FileToCommit{Name: name, Content: []byte(name)})
	}
	_, parentUUID, err := source.Commit(CommitOpts{Files: files, User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return source.HandleSync(ctx, payload)
	})
	target, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "target.fossil"),
		peer, CloneOpts{PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })
	parent, ok := blob.Exists(target.inner.DB(), parentUUID)
	if !ok {
		t.Fatal("parent not cloned")
	}
	fixture := requiredPublicFixture{source: source, target: target, parent: int64(parent), peer: peer}
	fixture.elide(t)
	return fixture
}

func (f *requiredPublicFixture) elide(t *testing.T) {
	t.Helper()
	parent, err := manifest.GetManifest(f.target.inner, fsltype.FslID(f.parent))
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.S) < 2 {
		t.Fatal("fixture needs actual sibling sub-manifests")
	}
	var names []string
	var remove []string
	for i, sub := range parent.S {
		rid, ok := content.AvailableByUUID(f.target.inner.DB(), sub.UUID)
		if !ok {
			t.Fatal("fixture leaf not cloned")
		}
		data, err := content.Expand(f.target.inner.DB(), rid)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := deck.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range leaf.F {
			if i == 0 {
				names = append(names, file.Name)
			} else {
				f.outside = append(f.outside, file.UUID)
			}
		}
		if i > 0 {
			remove = append(remove, sub.UUID)
		}
	}
	c, err := cone.New(names, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.target.inner.DB(), c); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range append(remove, f.outside...) {
		if _, err := f.target.inner.DB().Exec(
			"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", uuid); err != nil {
			t.Fatal(err)
		}
		if _, err := f.target.inner.DB().Exec(
			"INSERT OR IGNORE INTO phantom SELECT rid FROM blob WHERE uuid=?", uuid); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommitContextRequiredContent(t *testing.T) {
	for _, policy := range []string{"sha1", "sha3"} {
		t.Run(policy, func(t *testing.T) {
			f := newRequiredPublicFixture(t, policy)
			opts := CommitOpts{ParentID: f.parent, User: "test",
				Files: []FileToCommit{{Name: "f0000", Content: []byte("edited")}}}
			if _, _, err := f.target.Commit(opts); err == nil ||
				!strings.Contains(err.Error(), "no configured peer") {
				t.Fatalf("legacy method did not remain local: %v", err)
			}
			_, _, err := f.target.CommitContext(context.Background(), opts,
				RequiredContentOpts{Peer: f.peer})
			if err != nil {
				t.Fatal(err)
			}
			for _, uuid := range f.outside {
				if _, held := content.AvailableByUUID(f.target.inner.DB(), uuid); held {
					t.Fatalf("required S recovery fetched outside F %s", uuid)
				}
			}
		})
	}
}

func TestCommitContextRequiredInConePayload(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	parent, err := manifest.GetManifest(f.target.inner, fsltype.FslID(f.parent))
	if err != nil {
		t.Fatal(err)
	}
	rid, held := content.AvailableByUUID(f.target.inner.DB(), parent.S[0].UUID)
	if !held {
		t.Fatal("fixture covered subtree not held")
	}
	data, err := content.Expand(f.target.inner.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := deck.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	uuid := leaf.F[0].UUID
	if _, err := f.target.inner.DB().Exec(
		"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", uuid); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.target.CommitContext(context.Background(), CommitOpts{
		ParentID: f.parent, User: "test",
		Files: []FileToCommit{{Name: "f0003", Content: []byte("edited")}},
	}, RequiredContentOpts{Peer: f.peer})
	if err != nil {
		t.Fatal(err)
	}
	if _, held := content.AvailableByUUID(f.target.inner.DB(), uuid); !held {
		t.Fatal("necessary in-cone carried payload was not recovered")
	}
}

func TestCommitContextRefusesChangedPolicy(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		if err := f.target.inner.SetConfig(manifest.SettingSubmanifestK, "2"); err != nil {
			return nil, err
		}
		return f.peer.RoundTrip(ctx, payload)
	})
	_, _, err := f.target.CommitContext(context.Background(),
		CommitOpts{ParentID: f.parent, User: "test"}, RequiredContentOpts{Peer: peer})
	if err == nil || !strings.Contains(err.Error(), "changed during required-content recovery") {
		t.Fatalf("replayed operation after policy changed: %v", err)
	}
}

func requiredPublicCheckout(t *testing.T, f requiredPublicFixture) *Checkout {
	t.Helper()
	stored, err := content.StoreCone(f.target.inner.DB())
	if err != nil {
		t.Fatal(err)
	}
	co, err := f.target.CreateCheckout(t.TempDir(), CheckoutCreateOpts{Cone: wrapCone(stored)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(f.parent, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	return co
}

// No peer means refusal before scanning or moving checkout files. Compare every
// durable checkout byte, including metadata, and the repository artifact count.
func TestRequiredContentNoPeerPreservesCheckout(t *testing.T) {
	for _, operation := range []string{"checkin", "cone"} {
		t.Run(operation, func(t *testing.T) {
			f := newRequiredPublicFixture(t, "sha3")
			co := requiredPublicCheckout(t, f)
			name := filepath.Join(co.Dir(), "f0000")
			if err := os.WriteFile(name, []byte("pending edit"), 0o644); err != nil {
				t.Fatal(err)
			}
			metadata, err := co.inner.RecoverySnapshot()
			if err != nil {
				t.Fatal(err)
			}
			before := requiredCheckoutBytes(t, co.Dir())
			var count, after int
			if err := f.target.inner.DB().QueryRow("SELECT count(*) FROM blob").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if operation == "checkin" {
				_, _, err = co.CheckinContext(context.Background(), CheckoutCommitOpts{User: "test"},
					RequiredContentOpts{})
			} else {
				_, err = co.SetConeContext(context.Background(), nil, RequiredContentOpts{})
			}
			if err == nil || !strings.Contains(err.Error(), "no configured peer") {
				t.Fatalf("no-peer refusal: %v", err)
			}
			currentMetadata, snapshotErr := co.inner.RecoverySnapshot()
			if snapshotErr != nil || currentMetadata != metadata {
				t.Fatalf("no-peer refusal changed logical checkout state: %v", snapshotErr)
			}
			current := requiredCheckoutBytes(t, co.Dir())
			if !reflect.DeepEqual(before, current) {
				for path, data := range current {
					if data != before[path] {
						t.Errorf("no-peer refusal changed %s (%d -> %d bytes)",
							filepath.Base(path), len(before[path]), len(data))
					}
				}
				t.Fatal("no-peer refusal changed checkout bytes")
			}
			if err := f.target.inner.DB().QueryRow("SELECT count(*) FROM blob").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if count != after {
				t.Fatalf("no-peer refusal published artifacts: %d -> %d", count, after)
			}
		})
	}
}

func requiredCheckoutBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		// SQLite read locks update the shared-memory WAL index, not durable state.
		if entry.Name() == ".fslckout-shm" || entry.Name() == ".libfossil-checkout-shm" {
			return nil
		}
		if len(files) >= 1024 {
			t.Fatal("fixture unexpectedly exceeds checkout file bound")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCheckinContextRequiredContent(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	co := requiredPublicCheckout(t, f)
	name := filepath.Join(co.Dir(), "f0000")
	if err := os.WriteFile(name, []byte("edited checkout"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := co.CheckinContext(context.Background(), CheckoutCommitOpts{User: "test"},
		RequiredContentOpts{Peer: f.peer})
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range f.outside {
		if _, held := content.AvailableByUUID(f.target.inner.DB(), uuid); held {
			t.Fatal("checkout recovery fetched outside F payload")
		}
	}
}

func TestSetConeContextRequiredContent(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	co := requiredPublicCheckout(t, f)
	change, err := co.SetConeContext(context.Background(), nil, RequiredContentOpts{Peer: f.peer})
	if err != nil {
		t.Fatal(err)
	}
	if co.Cone() != nil || len(change.Materialized) != 9 {
		t.Fatalf("dense widening did not materialize nine entering files: %+v", change)
	}
	for _, name := range []string{"f0015", "f0024"} {
		data, err := os.ReadFile(filepath.Join(co.Dir(), name))
		if err != nil || string(data) != name {
			t.Fatalf("newly covered payload %s: %q, %v", name, data, err)
		}
	}
	got, err := content.StoreCone(f.target.inner.DB())
	if err != nil || got != nil {
		t.Fatalf("dense checkout intent did not densify store: %v, %v", got, err)
	}
}

func TestCommitContextCancellationAndFlags(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := f.target.CommitContext(ctx, CommitOpts{}, RequiredContentOpts{Peer: f.peer})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled operation: %v", err)
	}
	_, _, err = f.target.CommitContext(context.Background(), CommitOpts{},
		RequiredContentOpts{Peer: f.peer, Sync: SyncOpts{Push: true}})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("push option authorized a recovery operation: %v", err)
	}
}

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

// Structural recovery does not make outside F bytes readable. Each local read
// refuses accurately instead of calling healthy elision a missing tracked path.
func TestSparseLocalReadRefusals(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	ctx := context.Background()
	parents := []fsltype.FslID{fsltype.FslID(f.parent)}
	err := manifest.RequireParentContent(ctx, f.target.inner.DB(), parents)
	if err := (RequiredContentOpts{Peer: f.peer}).recover(ctx, f.target.inner, err); err != nil {
		t.Fatal(err)
	}
	files, err := f.target.ListFiles(f.parent)
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for _, file := range files {
		if file.UUID == f.outside[0] {
			name = file.Name
		}
	}
	if name == "" {
		t.Fatal("outside payload path not found")
	}
	for name, read := range map[string]func() error{
		"cat": func() error { _, err := f.target.ReadFile(f.parent, name); return err },
		"annotate": func() error {
			_, err := f.target.Annotate(AnnotateOpts{FilePath: name, StartRID: f.parent})
			return err
		},
		"diff": func() error { _, err := f.target.Diff(f.parent, f.parent, name); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := read()
			if err == nil || !strings.Contains(err.Error(), "elided") ||
				!strings.Contains(err.Error(), "widen/deepen") {
				t.Fatalf("local read refusal: %v", err)
			}
		})
	}
}

// An unlocked exchange cannot silently replay a queue selected before another
// operation changed it. The local retry must reject that stale snapshot.
func TestCheckinContextRefusesChangedQueue(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	co := requiredPublicCheckout(t, f)
	changed := false
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		if !changed {
			changed = true
			if err := co.inner.Enqueue(checkout.EnqueueOpts{Paths: []string{"f0000"}}); err != nil {
				return nil, err
			}
		}
		return f.source.HandleSync(ctx, payload)
	})
	_, _, err := co.CheckinContext(context.Background(), CheckoutCommitOpts{
		User: "test", AllowEmpty: true,
	}, RequiredContentOpts{Peer: peer})
	if !changed || err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("changed queue was not rejected: changed=%v err=%v", changed, err)
	}
}

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
