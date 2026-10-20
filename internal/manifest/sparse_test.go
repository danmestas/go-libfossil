package manifest

import (
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// sparseParent commits src/a.c and tools/t.c, then makes the store sparse
// at {src} with tools/t.c's content elided. It returns the check-in and the
// elided file's name.
func sparseParent(t *testing.T) (*repo.Repo, libfossil.FslID, string) {
	t.Helper()
	r := setupTestRepo(t)
	c1, _, err := Checkin(r, CheckinOpts{Comment: "c1", User: "u", Files: []File{
		{Name: "src/a.c", Content: []byte("a\n")}, {Name: "tools/t.c", Content: []byte("t\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := LookupFile(r, c1, "tools/t.c")
	if err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
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
	return r, c1, e.UUID
}

// A commit over a sparse store carries the elided file by hash and has no R
// card (draft-fossil-sparse-cone-00 §4.3, §5.3).
func TestCheckinOverSparseStore(t *testing.T) {
	r, c1, elided := sparseParent(t)
	c2, _, err := Checkin(r, CheckinOpts{Comment: "c2", User: "u", Parent: c1, Files: []File{
		{Name: "src/a.c", Content: []byte("a2\n")}, {Name: "tools/t.c", UUID: elided},
	}})
	if err != nil {
		t.Fatalf("Checkin over a sparse store: %v", err)
	}
	d, err := GetManifest(r, c2)
	if err != nil {
		t.Fatal(err)
	}
	if d.R != "" {
		t.Fatalf("R card %q over a sparse store", d.R)
	}
	if e, ok, err := LookupFile(r, c2, "tools/t.c"); err != nil || !ok || e.UUID != elided {
		t.Fatalf("tools/t.c = %+v, %v, %v; want %s", e, ok, err, elided)
	}
	if err := r.WithTx(func(tx *db.Tx) error { return VerifyRCard(tx, nil, c1) }); err != nil {
		t.Fatalf("VerifyRCard over a sparse store: %v", err)
	}
}

// Content the store cone covers is never elided: carrying it while absent is
// still refused, as over a dense store.
func TestCheckinOverSparseStoreRefusesMissingInCone(t *testing.T) {
	r, c1, elided := sparseParent(t)
	c, err := cone.New([]string{"src", "tools"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	_, _, err = Checkin(r, CheckinOpts{Comment: "c2", User: "u", Parent: c1, Files: []File{
		{Name: "src/a.c", Content: []byte("a2\n")}, {Name: "tools/t.c", UUID: elided},
	}})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("carrying absent in-cone content: %v; want a refusal", err)
	}
}
