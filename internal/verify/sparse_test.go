package verify_test

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// A store complete within its cone verifies OK, its elision counted apart
// from transit phantoms; without the cone the same phantom is in transit
// (draft-fossil-sparse-cone-00 §5.2). Rebuild keeps the cone.
func TestVerifyCountsElision(t *testing.T) {
	r := newTestRepo(t)
	c1, _, err := manifest.Checkin(r, manifest.CheckinOpts{Comment: "c1", User: "test", Files: []manifest.File{
		{Name: "src/a.c", Content: []byte("a\n")}, {Name: "tools/t.c", Content: []byte("t\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := manifest.LookupFile(r, c1, "tools/t.c")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"UPDATE blob SET size=-1, content=NULL WHERE uuid=?",
		"INSERT INTO phantom(rid) SELECT rid FROM blob WHERE uuid=?",
	} {
		if _, err := r.DB().Exec(stmt, e.UUID); err != nil {
			t.Fatal(err)
		}
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	check := func(what string, report *verify.Report, elided, transit int) {
		t.Helper()
		if !report.OK() || report.Elided != elided || report.Phantoms != transit {
			t.Fatalf("%s: OK=%v elided %d transit %d; want true %d %d (issues %+v)",
				what, report.OK(), report.Elided, report.Phantoms, elided, transit, report.Issues)
		}
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	check("sparse", report, 1, 0)
	if report, err = verify.Rebuild(r); err != nil {
		t.Fatal(err)
	}
	check("rebuilt", report, 1, 0)
	if err := content.SetStoreCone(r.DB(), nil); err != nil {
		t.Fatal(err)
	}
	if report, err = verify.Verify(r); err != nil {
		t.Fatal(err)
	}
	check("dense", report, 0, 1)
}
