package libfossil

import (
	"errors"
	"path/filepath"
	"testing"
)

// costRepo is c1 -> c2 -> c3 on trunk and an unmerged childless fork feat off
// c1. A horizon at c2 holds {c2, c3} under the descendants rule and all four
// under the leaves rule, where feat's ancestry meets c2's at c1.
func costRepo(t *testing.T) (r *Repo, c2 string) {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "cost.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	commit := func(parent int64, a string) (int64, string) {
		rid, uuid, err := r.Commit(CommitOpts{User: "test", ParentID: parent, Files: []FileToCommit{
			{Name: "a", Content: []byte(a)}, {Name: "b", Content: []byte("shared b\n")},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return rid, uuid
	}
	r1, _ := commit(0, "old a\n")
	r2, c2 := commit(r1, "new a\n")
	commit(r2, "newest a\n")
	commit(r1, "feat a\n")
	return r, c2
}

func TestHorizonCost(t *testing.T) {
	r, c2 := costRepo(t)
	got, err := r.HorizonCost(c2)
	if err != nil {
		t.Fatal(err)
	}
	want := []HorizonRuleCost{
		{Rule: HorizonDescendants, Checkins: 2, Blobs: 3, ExpandedBytes: 6 + 9 + 9},
		{Rule: HorizonLeaves, Checkins: 4, Blobs: 5, ExpandedBytes: 6 + 6 + 9 + 7 + 9},
	}
	if len(got) != len(want) {
		t.Fatalf("HorizonCost = %+v, want %d rules", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.StoredBytes <= 0 {
			t.Errorf("%s: stored bytes %d, want positive", w.Rule, g.StoredBytes)
		}
		g.StoredBytes = 0
		if g != w {
			t.Errorf("%s = %+v, want %+v", w.Rule, g, w)
		}
	}
}

func TestHorizonCostUnknownCheckin(t *testing.T) {
	r, _ := costRepo(t)
	if _, err := r.HorizonCost("no-such-name"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("error = %v, want ErrVersionNotFound", err)
	}
}
