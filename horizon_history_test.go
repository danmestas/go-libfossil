package libfossil

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
)

// horizonHistory is a three-check-in repository cut the way a structural
// clone at c2 cuts it: c1, c2's parent, is a phantom with no event. label
// says whether the origin labelled it below the horizon.
func horizonHistory(t *testing.T, label bool) (r *Repo, c1, c2, c3 string, rid3 int64) {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "h.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	var rids [3]int64
	var uuids [3]string
	parent := int64(0)
	for i := range rids {
		rids[i], uuids[i], err = r.Commit(CommitOpts{User: "test", ParentID: parent, Files: []FileToCommit{
			{Name: "a", Content: []byte{byte('a' + i)}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		parent = rids[i]
	}
	db := r.Inner().DB()
	if _, err := db.Exec("UPDATE blob SET size=-1, content=NULL WHERE rid=?", rids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM event WHERE objid=?", rids[0]); err != nil {
		t.Fatal(err)
	}
	if label {
		if err := content.LabelBelow(db, []string{uuids[0]}); err != nil {
			t.Fatal(err)
		}
	}
	return r, uuids[0], uuids[1], uuids[2], rids[2]
}

func TestHistoryRendersBoundaryAsHorizon(t *testing.T) {
	r, c1, c2, c3, rid3 := horizonHistory(t, true)

	anc, err := r.Ancestry(LogOpts{Start: rid3})
	if err != nil {
		t.Fatalf("ancestry ended at the horizon with an error: %v", err)
	}
	if len(anc) != 2 || anc[0].UUID != c3 || anc[1].UUID != c2 {
		t.Fatalf("ancestry = %+v, want c3, c2 and no more", anc)
	}
	if len(anc[0].HorizonParents) != 0 {
		t.Errorf("c3 horizon parents = %v, want none", anc[0].HorizonParents)
	}
	if got := anc[1].HorizonParents; len(got) != 1 || got[0] != c1 {
		t.Errorf("c2 horizon parents = %v, want [%s]", got, c1)
	}
	if got := anc[1].Parents; len(got) != 1 || got[0] != c1 {
		t.Errorf("c2 parents = %v, want [%s]", got, c1)
	}

	tl, err := r.Timeline(TimelineOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl) != 2 || len(tl[1].HorizonParents) != 1 || tl[1].HorizonParents[0] != c1 {
		t.Fatalf("timeline = %+v, want c2 to name %s as the horizon", tl, c1)
	}
}

func TestHistoryUnlabelledAbsentParentIsNotHorizon(t *testing.T) {
	r, c1, c2, _, _ := horizonHistory(t, false)
	tl, err := r.Timeline(TimelineOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl) != 2 || tl[1].UUID != c2 || len(tl[1].Parents) != 1 || tl[1].Parents[0] != c1 {
		t.Fatalf("timeline = %+v", tl)
	}
	if len(tl[1].HorizonParents) != 0 {
		t.Errorf("unlabelled absent parent rendered as horizon: %v", tl[1].HorizonParents)
	}
}
