package content_test

import (
	"context"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// scaleFileBytes is each large file's size. Its bytes are incompressible and
// no two versions share a delta, so reading any one of them costs at least
// this much, which is more than the whole injected budget.
const scaleFileBytes = 1 << 20

// scaleBudget fits the repository's structure -- three small manifests --
// and none of its files.
var scaleBudget = content.ReferenceBudget{Artifacts: 1 << 10, StoredBytes: 64 << 10,
	InflatedBytes: 64 << 10, ExpandedBytes: 64 << 10, Cards: 1 << 10}

type scaleFixture struct {
	r          *repo.Repo
	c1, c2, c3 libfossil.FslID
	// big1v1 is doc/big1.bin as c1 has it, a phantom only c1 names; big2
	// is lib/big2.bin, a phantom every check-in names.
	big1v1, big2 libfossil.FslID
}

// newScaleFixture is c1 -> c2 -> c3, crosslinked by a sweep, so the store
// records every file blob as examined and not an artifact.
func newScaleFixture(t *testing.T) *scaleFixture {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "scale.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	random := rand.New(rand.NewSource(1))
	big := func() []byte {
		b := make([]byte, scaleFileBytes)
		random.Read(b)
		return b
	}
	f := &scaleFixture{r: r}
	big1, big2 := big(), big()
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	commit := func(parent libfossil.FslID, src string, big1 []byte) libfossil.FslID {
		t.Helper()
		when = when.Add(time.Hour)
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: []manifest.File{
				{Name: "src/a.c", Content: []byte(src)},
				{Name: "doc/big1.bin", Content: big1},
				{Name: "lib/big2.bin", Content: big2},
			},
			Comment: "c", User: "u", Parent: parent, Time: when,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	f.c1 = commit(0, "a 1\n", big1)
	f.c2 = commit(f.c1, "a 2\n", big())
	f.c3 = commit(f.c2, "a 3\n", big1[:len(big1)-1])
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatal(err)
	}
	for _, at := range []struct {
		rid  *libfossil.FslID
		name string
	}{{&f.big1v1, "doc/big1.bin"}, {&f.big2, "lib/big2.bin"}} {
		e, ok, err := manifest.LookupFile(r, f.c1, at.name)
		if err != nil || !ok {
			t.Fatalf("LookupFile %s: %v, %v", at.name, ok, err)
		}
		*at.rid = hzPhantomize(t, r, e.UUID)
	}
	return f
}

func (f *scaleFixture) load(t *testing.T) *content.Absences {
	t.Helper()
	a, work, err := content.LoadAbsencesWithin(context.Background(), f.r.DB(), scaleBudget, 64<<10)
	if err != nil {
		t.Fatalf("classification over a budget its files exceed: %v (work %+v)", err, work)
	}
	if read := work.InflateBytes + work.ExpandBytes + work.TreeBytes; read >= scaleFileBytes {
		t.Fatalf("classification read file content: %d bytes (work %+v)", read, work)
	}
	return a
}

// Classification reads what it must prove, not the store: a path cone over a
// store whose files far exceed the budget classifies from its structure.
func TestSparseScalePathConeReadsNoFiles(t *testing.T) {
	f := newScaleFixture(t)
	if err := content.SetStoreCone(f.r.DB(), mustConeExt(t, []string{"src"})); err != nil {
		t.Fatal(err)
	}
	a := f.load(t)
	if a.Class(f.big1v1) != content.Elided || a.Class(f.big2) != content.Elided {
		t.Fatalf("outside files: big1v1=%v big2=%v, want elided", a.Class(f.big1v1), a.Class(f.big2))
	}
}

// A content horizon reads only the manifests of its held set: c1's version
// of doc/big1.bin is below a horizon at c2, lib/big2.bin, which c2 and c3
// still name, is above it.
func TestSparseScaleHorizonReadsNoFiles(t *testing.T) {
	f := newScaleFixture(t)
	h, err := cone.NewHorizon(hzUUID(t, f.r, f.c2), cone.RuleDescendants, cone.DepthContent)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), sc); err != nil {
		t.Fatal(err)
	}
	a := f.load(t)
	if a.Class(f.big1v1) != content.Elided || a.Class(f.big2) != content.Transit {
		t.Fatalf("big1v1=%v big2=%v, want elided and transit", a.Class(f.big1v1), a.Class(f.big2))
	}
}

func mustConeExt(t *testing.T, include []string) *cone.Cone {
	t.Helper()
	c, err := cone.New(include, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func hzUUID(t *testing.T, r *repo.Repo, rid libfossil.FslID) string {
	t.Helper()
	var uuid string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	return uuid
}

// A store fossil crosslinked has no record of the blobs it found to be no
// artifact, only mlink naming them as file versions and no artifact table
// claiming them. That proves file content as well, so such a store, served
// or cloned as fossil wrote it, still classifies without reading its files.
func TestSparseScaleFossilWrittenStoreReadsNoFiles(t *testing.T) {
	f := newScaleFixture(t)
	if _, err := f.r.DB().Exec("DROP TABLE crosslink_nonartifact_v2"); err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), mustConeExt(t, []string{"src"})); err != nil {
		t.Fatal(err)
	}
	a := f.load(t)
	if a.Class(f.big1v1) != content.Elided || a.Class(f.big2) != content.Elided {
		t.Fatalf("outside files: big1v1=%v big2=%v, want elided", a.Class(f.big1v1), a.Class(f.big2))
	}
}
