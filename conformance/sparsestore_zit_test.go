package conformance

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// TestZitSparseStoreInterop has zit, whose sparse store is implemented, check
// go-libfossil's (draft-fossil-sparse-cone-00 §5, Appendix A step 3). zit
// clones go-libfossil's flat history through a cone, filtering locally, and
// both implementations must count the same elided phantoms over the same
// cone. Each then commits over its sparse store and pushes to a dense store
// of the other: the commit carries no R card, and the receiver holds the
// whole check-in, the files the committer never held included.
func TestZitSparseStoreInterop(t *testing.T) {
	bin := zitBinary(t)
	e := newZitSparseEnv(t, bin)
	spec := zitConeSpec{Include: []string{"src"}, Exclude: []string{"src/sub"}}
	c, err := spec.cone()
	if err != nil {
		t.Fatal(err)
	}

	// zit's elision count over its own cone-scoped clone.
	zsparse := filepath.Join(e.dir, "zsparse")
	zitRun(t, bin, e.home, e.dir, append([]string{"clone", "--no-open"}, append(spec.flags(), e.srcURL, zsparse)...)...)
	zitElided := zitElidedCount(t, zitRun(t, bin, e.home, zsparse, "verify"))
	if zitElided == 0 {
		t.Fatal("zit elided nothing")
	}

	// go-libfossil's over a dense clone made sparse at the same cone.
	gsparse, _, err := libfossil.Clone(context.Background(), filepath.Join(e.dir, "gsparse.fossil"),
		libfossil.NewHTTPTransport(e.srcURL), libfossil.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer gsparse.Close()
	scSparsify(t, gsparse.Inner(), c)
	report, err := verify.Verify(gsparse.Inner())
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() || report.Elided != zitElided || report.Phantoms != 0 {
		t.Fatalf("go-libfossil over zit's cone: OK=%v elided %d transit %d; zit elided %d",
			report.OK(), report.Elided, report.Phantoms, zitElided)
	}

	// go-libfossil commits over its sparse store and pushes to zit's dense
	// store, from which zit opens the whole check-in.
	zitURL := zitServe(t, bin, e.home, e.flatStore)
	if err := gsparse.SetStoreCone(mustPublicCone(t, spec)); err != nil {
		t.Fatal(err)
	}
	uuid := goSparseStoreCommit(t, e, gsparse, spec, "src/f2.c")
	if _, err := gsparse.Sync(context.Background(), libfossil.NewHTTPTransport(zitURL),
		libfossil.SyncOpts{Push: true, Pull: true}); err != nil {
		t.Fatalf("push of a sparse-store commit to zit: %v", err)
	}
	zitRun(t, bin, e.home, e.flatStore, "verify")
	e.sameListing(t, gsparse.Inner(), e.flatStore, uuid)
	w := e.zitWorktree(t, e.flatStore, uuid, zitConeSpec{})
	for name, want := range map[string]string{"src/f2.c": "go sparse edit\n", "doc/f1.c": "doc/f1.c v1\n"} {
		got, err := os.ReadFile(filepath.Join(w, name))
		if err != nil || string(got) != want {
			t.Fatalf("zit's dense open of the pushed check-in: %s = %q, %v; want %q", name, got, err, want)
		}
	}

	// zit commits over its sparse store and pushes to go-libfossil's dense
	// one, which verifies and holds the whole check-in.
	// The store cone lives beside the store, so zit opens the worktree there.
	zw := zsparse
	zitRun(t, bin, e.home, zw, append([]string{"open"}, spec.flags()...)...)
	if err := os.WriteFile(filepath.Join(zw, "src", "f3.c"), []byte("zit sparse-store edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zitRun(t, bin, e.home, zw, "commit", "-m", "commit over zit's sparse store")
	tip := zitTip(t, bin, e.home, zw)
	if strings.Contains(zitRun(t, bin, e.home, zw, "artifact", tip), "\nR ") {
		t.Fatal("zit's commit over a sparse store carries an R card")
	}
	zitRun(t, bin, e.home, zw, "push", e.srcURL)
	if err := checkZitCheckin(e.src.Inner(), tip, zitListing(t, bin, e.home, zw, tip)); err != nil {
		t.Fatalf("zit's sparse-store commit in go-libfossil: %v", err)
	}
	rid, ok := blob.Exists(e.src.Inner().DB(), tip)
	if !ok {
		t.Fatal("zit's sparse-store commit did not arrive")
	}
	if d, err := manifest.GetManifest(e.src.Inner(), rid); err != nil || d.R != "" {
		t.Fatalf("the received check-in: %v, R %q", err, d.R)
	}
	got, err := scContent(e.src.Inner(), rid)
	if err != nil {
		t.Fatal(err)
	}
	if got["src/f3.c"] != "zit sparse-store edit\n" || got["doc/f1.c"] != "doc/f1.c v1\n" {
		t.Fatalf("the received check-in's content: src/f3.c %q, doc/f1.c %q", got["src/f3.c"], got["doc/f1.c"])
	}
	if report, err := verify.Verify(e.src.Inner()); err != nil || !report.OK() {
		t.Fatalf("go-libfossil verify after zit's push: %v %+v", err, report)
	}
}

// zitElidedCount reads the elision count from zit verify's summary.
func zitElidedCount(t *testing.T, out string) int {
	t.Helper()
	m := regexp.MustCompile(`(\d+) artifacts? deliberately elided`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("zit verify reports no elision: %s", out)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustPublicCone(t *testing.T, spec zitConeSpec) *libfossil.Cone {
	t.Helper()
	c, err := libfossil.NewCone(spec.Include, spec.Exclude)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// goSparseStoreCommit edits name in a checkout of r's tip whose cone is
// spec, the store cone, and commits; the commit must carry no R card.
func goSparseStoreCommit(t *testing.T, e *zitSparseEnv, r *libfossil.Repo, spec zitConeSpec, name string) string {
	t.Helper()
	c, err := spec.cone()
	if err != nil {
		t.Fatal(err)
	}
	co, err := checkout.Create(r.Inner(), filepath.Join(e.dir, "gostore"), checkout.CreateOpts{Cone: c})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	vid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(vid, checkout.ExtractOpts{Force: true}); err != nil {
		t.Fatalf("extract over a sparse store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(co.Dir(), name), []byte("go sparse edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rid, uuid, err := co.Commit(checkout.CommitOpts{Message: "commit over a sparse store", User: "u",
		Time: time.Now().UTC().Truncate(time.Second)})
	if err != nil {
		t.Fatalf("commit over a sparse store: %v", err)
	}
	if d, err := manifest.GetManifest(r.Inner(), rid); err != nil || d.R != "" {
		t.Fatalf("the commit over a sparse store: %v, R %q", err, d.R)
	}
	return uuid
}
