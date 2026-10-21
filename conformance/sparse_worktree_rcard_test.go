package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// §4.3's verification side. fossil's open and checkout check a present R
// card against the aggregate over every file the manifest names, read from
// the store, and warn -- not refuse -- on disagreement. A sparse
// materialization over a dense store must reach the same verdict: the cone
// narrows what is written, never what R is checked against. Over a sparse
// store the aggregate is uncomputable and the check is skipped.

const scRCardWarning = "manifest checksum does not agree with manifest"

func scRCardCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.4.3.materialize-dense-r", Req: "SC-4.3-4"}, scMaterializeDenseR},
		{Case{ID: "sparsecone.4.3.materialize-sparse-skips", Req: "SC-4.3-5"}, scMaterializeSparseSkipsR},
	}
}

type scRCardFixture struct {
	ref       *fossilReference
	path      string
	good, bad fsltype.FslID
}

// newSCRCardFixture has fossil make a repository of scFiles and then adds a
// child check-in naming the same files under a wrong R card, which fossil
// stores and crosslinks as given.
func newSCRCardFixture(t *testing.T) *scRCardFixture {
	t.Helper()
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("fossil(1) builds this fixture")
	}
	dir := t.TempDir()
	f := &scRCardFixture{ref: ref, path: filepath.Join(t.TempDir(), "rcard.fossil")}
	scRCardRun(t, ref, dir, "init", "--admin-user", "u", f.path)
	scRCardRun(t, ref, dir, "open", f.path)
	for name, text := range scFiles {
		scWrite(t, dir, name, text)
	}
	scRCardRun(t, ref, dir, "add", ".")
	scRCardRun(t, ref, dir, "commit", "--nosync", "--no-warnings", "-m", "good")
	parent := strings.TrimSpace(scRCardRun(t, ref, dir, "sql",
		"SELECT uuid FROM blob WHERE rid=(SELECT objid FROM event WHERE comment='good')"))
	d, err := deck.Parse([]byte(scRCardRun(t, ref, dir, "artifact", "tip")))
	if err != nil {
		t.Fatal(err)
	}
	d.C, d.R, d.D = "bad", strings.Repeat("0", 32), d.D.Add(time.Hour)
	d.P = []string{strings.Trim(parent, "'")}
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	scRCardRun(t, ref, dir, "test-content-put", bad)
	scRCardRun(t, ref, dir, "close", "--force")
	scRCardRun(t, ref, t.TempDir(), "rebuild", f.path)
	f.good, f.bad = f.rid(t, "good"), f.rid(t, "bad")
	return f
}

func scRCardRun(t *testing.T, ref *fossilReference, dir string, args ...string) string {
	t.Helper()
	out, err := ref.run(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *scRCardFixture) rid(t *testing.T, comment string) fsltype.FslID {
	t.Helper()
	r, err := repo.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var rid fsltype.FslID
	if err := r.DB().QueryRow("SELECT objid FROM event WHERE comment=?", comment).Scan(&rid); err != nil {
		t.Fatalf("check-in %q: %v", comment, err)
	}
	return rid
}

// fossilWarns reports whether fossil's open of rid warns about the R card.
func (f *scRCardFixture) fossilWarns(t *testing.T, rid fsltype.FslID) bool {
	t.Helper()
	r, err := repo.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var uuid string
	err = r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := scRCardRun(t, f.ref, dir, "open", f.path, uuid)
	scRCardRun(t, f.ref, dir, "close", "--force")
	return strings.Contains(out, scRCardWarning)
}

// warnings extracts rid into a checkout with cone c (nil dense) of the store
// at path and returns what Extract warned.
func scRCardExtract(t *testing.T, path string, c *cone.Cone, rid fsltype.FslID) ([]string, error) {
	t.Helper()
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	co, err := checkout.Create(r, t.TempDir(), checkout.CreateOpts{Cone: c})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	var warned []string
	err = co.Extract(rid, checkout.ExtractOpts{Force: true, Warn: func(msg string) {
		warned = append(warned, msg)
	}})
	return warned, err
}

func scMaterializeDenseR(t *testing.T) error {
	f := newSCRCardFixture(t)
	if !f.fossilWarns(t, f.bad) || f.fossilWarns(t, f.good) {
		return expectf("fixture: fossil does not warn exactly on the bad R card")
	}
	for _, c := range []*cone.Cone{nil, scDefault(t)} {
		for _, rid := range []fsltype.FslID{f.good, f.bad} {
			warned, err := scRCardExtract(t, f.path, c, rid)
			if err != nil {
				return expectf("extract (cone %v) of %d failed: %v; fossil warns and proceeds", c, rid, err)
			}
			want := rid == f.bad
			got := len(warned) == 1 && strings.Contains(warned[0], scRCardWarning)
			if got != want || (!want && len(warned) != 0) {
				return expectf("extract (cone %v) of %d warned %q; fossil warns: %v", c, rid, warned, want)
			}
		}
	}
	return nil
}

func scMaterializeSparseSkipsR(t *testing.T) error {
	f := newSCRCardFixture(t)
	r, err := repo.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	scSparsify(t, r, scDefault(t))
	r.Close()
	warned, err := scRCardExtract(t, f.path, scDefault(t), f.bad)
	if err != nil {
		return expectf("sparse-store extract of a bad R card failed: %v; the check is skipped", err)
	}
	if len(warned) != 0 {
		return expectf("sparse-store extract reported %q; an elided aggregate is unverifiable, not wrong", warned)
	}
	return nil
}
