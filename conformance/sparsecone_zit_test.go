package conformance

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	fsltype "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/simio"
)

// zit as the oracle for sparse cones. With zit on PATH, TestZitSparseInterop
// has zit write every cone below into a checkout and open sparse worktrees of
// a flat and a tree repository, and requires go-libfossil to serialize the
// same bytes and materialize the same files; it then commits from a sparse
// checkout on each side and has the other side verify the result.
// REQUIRE_ZIT_BIN=1 makes a missing zit a failure; ZIT_REGENERATE=1 records
// zit's answers under testdata/zit-cones, which the sparsecone suite's
// replay cases check in CI, where there is no zit.

const zitConeDir = "testdata/zit-cones"

// zitConeSpec is one cone as the fixtures record it.
type zitConeSpec struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude,omitempty"`
}

func (s zitConeSpec) cone() (*cone.Cone, error) { return cone.New(s.Include, s.Exclude) }

func (s zitConeSpec) flags() []string {
	var out []string
	for _, m := range s.Include {
		out = append(out, "--cone="+m)
	}
	for _, m := range s.Exclude {
		out = append(out, "--cone-not="+m)
	}
	return out
}

// zitSerial is a cone and the bytes zit stored for it.
type zitSerial struct {
	zitConeSpec
	Serialized string `json:"serialized"`
}

// zitMaterial is a cone and the files zit materialized for it at a check-in.
type zitMaterial struct {
	zitConeSpec
	Mode    string   `json:"mode"` // "flat": zitSourceHistory; "tree": the zit-trees bundle
	Checkin string   `json:"checkin"`
	Files   []string `json:"files"`
}

// zitCommit is a check-in zit committed from a sparse worktree over the flat
// source history, with the artifacts it added.
type zitCommit struct {
	zitConeSpec
	Checkin   string   `json:"checkin"`
	Listing   []string `json:"listing"`
	Artifacts []string `json:"artifacts"` // names of files under commit/
}

// zitMaterialCones are the cones whose materialization is compared, over
// the names zitSourceHistory writes.
var zitMaterialCones = []zitConeSpec{
	{Include: []string{"src"}},
	{Include: []string{"src"}, Exclude: []string{"src/sub"}},
	{Include: []string{"src", "src/sub/f3.c"}, Exclude: []string{"src/sub"}},
	{Include: []string{"README", "doc/moved.c", "test/f2.c"}},
	{Include: []string{"doc", "test"}, Exclude: []string{"test/f0.c"}},
	{Include: []string{"src/sub"}},
	{Include: []string{"src-x", "src"}, Exclude: []string{"src/sub", "src/f0.c"}},
	{Include: []string{"nothing/here"}},
}

// zitSerialCones are cones only serialized: bounds, odd bytes and orders.
func zitSerialCones() []zitConeSpec {
	many := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("d%03d", n-1-i)
		}
		return out
	}
	return append(slices.Clone(zitMaterialCones),
		zitConeSpec{Include: many(64)},
		zitConeSpec{Include: many(255)},
		zitConeSpec{Include: many(254), Exclude: []string{"d000/x"}},
		zitConeSpec{Include: []string{"ünï/çødé", "a b/c d", "src"}, Exclude: []string{"src/ü"}},
		zitConeSpec{Include: []string{"src/vendor/keep", "src", "docs/rfcs", "src/vendor/deep"}, Exclude: []string{"src/vendor"}},
		zitConeSpec{Include: []string{"a", "a-b", "a.b", "a/b/c"}, Exclude: []string{"a/b"}},
	)
}

// zitConeSection extracts the cone section (tag 5) from a zit checkout's
// container: magic, version, count and a reserved byte, then a table of
// (tag, u64 LE length), then the payloads in table order. It returns nil
// when the checkout is dense.
func zitConeSection(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".zit-checkout"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 12 || string(data[:8]) != "ZITCKOT\x00" {
		t.Fatalf("%s: not a zit checkout container", dir)
	}
	count := int(data[10])
	offset := 12 + 9*count
	for i := range count {
		entry := data[12+9*i:]
		n := int(binary.LittleEndian.Uint64(entry[1:9]))
		if entry[0] == 5 {
			return data[offset : offset+n]
		}
		offset += n
	}
	return nil
}

// --- CI replay of zit's recorded answers ----------------------------------

func scZitReplayCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.3.3.zit-serial", Req: "SC-3.3-1"}, scZitSerial},
		{Case{ID: "sparsecone.4.1.zit-materialize", Req: "SC-4.1-1"}, scZitMaterialize},
		{Case{ID: "sparsecone.4.2.zit-commit", Req: "SC-4.2-4"}, scZitCommit},
	}
}

func readZitFixture(name string, v any) error {
	data, err := os.ReadFile(filepath.Join(zitConeDir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeZitFixture(t *testing.T, name string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(zitConeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zitConeDir, name), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// scZitSerial: every recorded cone serializes to zit's bytes, and its digest
// is the SHA3-256 of them.
func scZitSerial(*testing.T) error {
	var vectors []zitSerial
	if err := readZitFixture("serial.json", &vectors); err != nil {
		return err
	}
	if len(vectors) < 10 {
		return expectf("only %d recorded vectors", len(vectors))
	}
	for _, v := range vectors {
		c, err := v.cone()
		if err != nil {
			return expectf("cone %v refused: %v", v.zitConeSpec, err)
		}
		want, err := hex.DecodeString(v.Serialized)
		if err != nil {
			return err
		}
		if got := c.Serialize(); !bytes.Equal(got, want) {
			return expectf("cone %v serializes to %x, zit wrote %x", v.zitConeSpec, got, want)
		}
		if c.Digest() != hash.SHA3(want) {
			return expectf("cone %v digest %s", v.zitConeSpec, c.Digest())
		}
	}
	return nil
}

// scZitMaterialize: a sparse checkout materializes the files zit did for
// every recorded cone, over the flat history and over zit's trees.
func scZitMaterialize(t *testing.T) error {
	var records []zitMaterial
	if err := readZitFixture("materialized.json", &records); err != nil {
		return err
	}
	repos := map[string]*repo.Repo{"flat": zitFlatHistory(t), "tree": zitTreeBundleRepo(t)}
	for _, m := range records {
		r := repos[m.Mode]
		if r == nil {
			return expectf("unknown mode %q", m.Mode)
		}
		got, err := goMaterialize(t, r, m.Checkin, m.zitConeSpec)
		if err != nil {
			return err
		}
		if strings.Join(got, "\n") != strings.Join(m.Files, "\n") {
			return expectf("%s cone %v: go %q, zit %q", m.Mode, m.zitConeSpec, got, m.Files)
		}
	}
	return nil
}

// scZitCommit: zit's commit from a sparse worktree is an ordinary check-in:
// stored into the same history it links, lists as zit listed it, and its R
// card verifies.
func scZitCommit(t *testing.T) error {
	var rec zitCommit
	if err := readZitFixture("commit.json", &rec); err != nil {
		return err
	}
	r := zitFlatHistory(t)
	for _, name := range rec.Artifacts {
		data, err := os.ReadFile(filepath.Join(zitConeDir, "commit", name))
		if err != nil {
			return err
		}
		if err := r.WithTx(func(tx *db.Tx) error { _, _, err := blob.Store(tx, data); return err }); err != nil {
			return err
		}
	}
	if _, err := manifest.Crosslink(r); err != nil {
		return err
	}
	return checkZitCheckin(r, rec.Checkin, rec.Listing)
}

// checkZitCheckin requires uuid to be held, linked, listed as want, its R
// card to verify, and the repository to verify.
func checkZitCheckin(r *repo.Repo, uuid string, want []string) error {
	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		return expectf("check-in %s is not held", uuid)
	}
	got, err := goListing(r, rid)
	if err != nil {
		return err
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		return expectf("check-in %s lists %q, zit %q", uuid[:10], got, want)
	}
	if err := r.WithTx(func(tx *db.Tx) error { return manifest.VerifyRCard(tx, nil, rid) }); err != nil {
		return err
	}
	report, err := verify.Verify(r)
	if err != nil {
		return err
	}
	if !report.OK() {
		return expectf("verify: %+v", report.Issues)
	}
	return nil
}

// zitFlatHistory is zitSourceHistory in a fresh repository.
func zitFlatHistory(t *testing.T) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flat.fossil")
	zitSourceHistory(t, path)
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// zitTreeBundleRepo loads the zit-trees bundle, zit's re-minting of
// zitSourceHistory as trees at k = 1.
func zitTreeBundleRepo(t *testing.T) *repo.Repo {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(zitBundleDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Create(filepath.Join(t.TempDir(), "trees.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetHashPolicy("sha3"); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(zitBundleDir, "artifacts", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.WithTx(func(tx *db.Tx) error { _, _, err := blob.Store(tx, data); return err }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// goMaterialize opens a sparse checkout of r at uuid and lists what it wrote.
func goMaterialize(t *testing.T, r *repo.Repo, uuid string, spec zitConeSpec) ([]string, error) {
	t.Helper()
	c, err := spec.cone()
	if err != nil {
		return nil, err
	}
	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		return nil, expectf("check-in %s is not held", uuid)
	}
	co, err := checkout.Create(r, t.TempDir(), checkout.CreateOpts{Cone: c})
	if err != nil {
		return nil, err
	}
	defer co.Close()
	if err := co.Extract(rid, checkout.ExtractOpts{Force: true}); err != nil {
		return nil, err
	}
	return scOnDisk(t, co.Dir()), nil
}

// --- the live run --------------------------------------------------------

// zitSparseEnv is the live run's shared state.
type zitSparseEnv struct {
	bin, home, dir string
	src            *libfossil.Repo // the flat history, served at srcURL
	srcURL         string
	flatStore      string          // zit's clone of src
	tree           *libfossil.Repo // go-libfossil's clone of zit's trees
	treeStore      string          // zit's tree re-minting of src
}

func TestZitSparseInterop(t *testing.T) {
	bin := zitBinary(t)
	e := newZitSparseEnv(t, bin)
	regenerate := os.Getenv("ZIT_REGENERATE") == "1"

	serials := e.compareSerials(t)
	materials := e.compareMaterials(t)
	commit := e.zitCommitsSparse(t)
	e.goCommitsSparse(t)
	e.goCommitsSparseTree(t)
	if regenerate {
		writeZitFixture(t, "serial.json", serials)
		writeZitFixture(t, "materialized.json", materials)
		writeZitFixture(t, "commit.json", commit)
	}
}

func newZitSparseEnv(t *testing.T, bin string) *zitSparseEnv {
	t.Helper()
	dir := t.TempDir()
	e := &zitSparseEnv{bin: bin, dir: dir, home: filepath.Join(dir, "home")}
	if err := os.Mkdir(e.home, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(dir, "src.fossil")
	zitSourceHistory(t, srcPath)
	var err error
	if e.src, err = libfossil.Open(srcPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.src.Close() })
	// Pushes from zit land here.
	if _, err := e.src.Inner().DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(e.src.XferHandler())
	t.Cleanup(server.Close)
	e.srcURL = server.URL

	e.flatStore = filepath.Join(dir, "zflat")
	zitRun(t, bin, e.home, dir, "clone", "--no-open", e.srcURL, e.flatStore)
	e.treeStore = filepath.Join(dir, "ztrees")
	zitRun(t, bin, e.home, dir, "import-fossil", e.srcURL, e.treeStore, "--submanifest", "--k", "1")
	treeURL := zitServe(t, bin, e.home, e.treeStore)
	treePath := filepath.Join(dir, "trees.fossil")
	if e.tree, _, err = libfossil.Clone(context.Background(), treePath, libfossil.NewHTTPTransport(treeURL), libfossil.CloneOpts{}); err != nil {
		t.Fatalf("Clone of zit's trees: %v", err)
	}
	t.Cleanup(func() { e.tree.Close() })
	return e
}

// zitWorktree opens a zit worktree of store at checkin under spec, and
// returns its directory.
func (e *zitSparseEnv) zitWorktree(t *testing.T, store, checkin string, spec zitConeSpec) string {
	t.Helper()
	dir, err := os.MkdirTemp(e.dir, "zw")
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"open", "--store=" + store}, spec.flags()...)
	if checkin != "" {
		args = append(args, checkin)
	}
	zitRun(t, e.bin, e.home, dir, args...)
	return dir
}

func (e *zitSparseEnv) compareSerials(t *testing.T) []zitSerial {
	t.Helper()
	tip := zitTip(t, e.bin, e.home, e.flatStore)
	w := e.zitWorktree(t, e.flatStore, tip, zitConeSpec{})
	var out []zitSerial
	for _, spec := range zitSerialCones() {
		zitRun(t, e.bin, e.home, w, append(append([]string{"update"}, spec.flags()...), tip)...)
		stored := zitConeSection(t, w)
		c, err := spec.cone()
		if err != nil {
			t.Fatalf("cone %v: %v", spec, err)
		}
		if !bytes.Equal(c.Serialize(), stored) {
			t.Errorf("cone %v: go %x, zit %x", spec, c.Serialize(), stored)
		}
		out = append(out, zitSerial{zitConeSpec: spec, Serialized: hex.EncodeToString(stored)})
	}
	return out
}

func (e *zitSparseEnv) compareMaterials(t *testing.T) []zitMaterial {
	t.Helper()
	var out []zitMaterial
	for _, mode := range []struct {
		name, store string
		r           *repo.Repo
	}{{"flat", e.flatStore, e.src.Inner()}, {"tree", e.treeStore, e.tree.Inner()}} {
		tip := zitTip(t, e.bin, e.home, mode.store)
		for _, spec := range zitMaterialCones {
			zitFiles := scOnDisk(t, e.zitWorktree(t, mode.store, tip, spec))
			goFiles, err := goMaterialize(t, mode.r, tip, spec)
			if err != nil {
				t.Fatalf("%s cone %v: %v", mode.name, spec, err)
			}
			if strings.Join(goFiles, "\n") != strings.Join(zitFiles, "\n") {
				t.Errorf("%s cone %v: go %q, zit %q", mode.name, spec, goFiles, zitFiles)
			}
			out = append(out, zitMaterial{zitConeSpec: spec, Mode: mode.name, Checkin: tip, Files: zitFiles})
		}
	}
	return out
}

// zitCommitsSparse commits from a zit sparse worktree, pushes to
// go-libfossil, and checks the check-in there. It returns the record the
// replay case reads, writing its artifacts when regenerating.
func (e *zitSparseEnv) zitCommitsSparse(t *testing.T) zitCommit {
	t.Helper()
	spec := zitConeSpec{Include: []string{"doc"}, Exclude: []string{"doc/f3.c"}}
	w := e.zitWorktree(t, e.flatStore, "", spec)
	if err := os.WriteFile(filepath.Join(w, "doc", "f2.c"), []byte("zit sparse edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zitRun(t, e.bin, e.home, w, "commit", "-m", "sparse commit from zit")
	zitRun(t, e.bin, e.home, w, "push", e.srcURL)
	tip := zitTip(t, e.bin, e.home, w)
	listing := zitListing(t, e.bin, e.home, w, tip)
	if err := checkZitCheckin(e.src.Inner(), tip, listing); err != nil {
		t.Fatalf("zit's sparse commit in go-libfossil: %v", err)
	}
	rec := zitCommit{zitConeSpec: spec, Checkin: tip, Listing: listing}
	if os.Getenv("ZIT_REGENERATE") == "1" {
		rec.Artifacts = e.recordNewArtifacts(t)
	}
	return rec
}

// recordNewArtifacts writes the artifacts src holds beyond zitSourceHistory
// under commit/, returning their names.
func (e *zitSparseEnv) recordNewArtifacts(t *testing.T) []string {
	t.Helper()
	base := zitFlatHistory(t)
	known := map[string]bool{}
	rows, err := base.DB().Query("SELECT uuid FROM blob")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var uuid string
		rows.Scan(&uuid)
		known[uuid] = true
	}
	rows.Close()
	dir := filepath.Join(zitConeDir, "commit")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows, err = e.src.Inner().DB().Query("SELECT rid, uuid FROM blob WHERE size>=0")
	if err != nil {
		t.Fatal(err)
	}
	type art struct {
		rid  int64
		uuid string
	}
	var arts []art
	for rows.Next() {
		var a art
		rows.Scan(&a.rid, &a.uuid)
		if !known[a.uuid] {
			arts = append(arts, a)
		}
	}
	rows.Close()
	var names []string
	for _, a := range arts {
		data, err := content.Expand(e.src.Inner().DB(), fsltype.FslID(a.rid))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, a.uuid), data, 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, a.uuid)
	}
	sort.Strings(names)
	return names
}

// goCommitsSparse commits from a go-libfossil sparse checkout of the flat
// history; zit pulls it and verifies its store.
func (e *zitSparseEnv) goCommitsSparse(t *testing.T) {
	t.Helper()
	uuid := e.goSparseCommit(t, e.src.Inner(), zitConeSpec{Include: []string{"src"}, Exclude: []string{"src/sub"}},
		"src/f2.c")
	zitRun(t, e.bin, e.home, e.flatStore, "pull", e.srcURL)
	zitRun(t, e.bin, e.home, e.flatStore, "verify")
	e.sameListing(t, e.src.Inner(), e.flatStore, uuid)
}

// goCommitsSparseTree commits a tree check-in from a go-libfossil sparse
// checkout of zit's trees; a zit clone of it verifies.
func (e *zitSparseEnv) goCommitsSparseTree(t *testing.T) {
	t.Helper()
	r := e.tree.Inner()
	for k, v := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
	} {
		if err := r.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	uuid := e.goSparseCommit(t, r, zitConeSpec{Include: []string{"src/sub"}}, "src/sub/f1.c")
	rid, _ := blob.Exists(r.DB(), uuid)
	if d, err := manifest.GetManifest(r, rid); err != nil || len(d.S) == 0 {
		t.Fatalf("the sparse tree commit is not a tree: %v", err)
	}
	server := httptest.NewServer(e.tree.XferHandler())
	defer server.Close()
	relay := filepath.Join(e.dir, "relay")
	zitRun(t, e.bin, e.home, e.dir, "clone", "--no-open", server.URL, relay)
	zitRun(t, e.bin, e.home, relay, "verify")
	e.sameListing(t, r, relay, uuid)
}

// goSparseCommit edits name in a sparse checkout of r at its tip and
// commits, returning the check-in's name.
func (e *zitSparseEnv) goSparseCommit(t *testing.T, r *repo.Repo, spec zitConeSpec, name string) string {
	t.Helper()
	c, err := spec.cone()
	if err != nil {
		t.Fatal(err)
	}
	co, err := checkout.Create(r, filepath.Join(e.dir, "go-"+strings.ReplaceAll(name, "/", "-")), checkout.CreateOpts{Cone: c})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	vid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(vid, checkout.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(co.Dir(), name), []byte("go sparse edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, uuid, err := co.Commit(checkout.CommitOpts{Message: "sparse commit from go-libfossil", User: "u",
		Time: time.Now().UTC().Truncate(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return uuid
}

// sameListing requires zit's listing of uuid in store to be go-libfossil's.
func (e *zitSparseEnv) sameListing(t *testing.T, r *repo.Repo, store, uuid string) {
	t.Helper()
	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatalf("%s is not held", uuid)
	}
	got, err := goListing(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	if want := zitListing(t, e.bin, e.home, store, uuid); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("check-in %s: go %q, zit %q", uuid[:10], got, want)
	}
}
