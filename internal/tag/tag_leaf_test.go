package tag_test

import (
	"slices"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
)

func leafRids(t *testing.T, r *repo.Repo) []libfossil.FslID {
	t.Helper()
	rows, err := r.DB().Query("SELECT rid FROM leaf ORDER BY rid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []libfossil.FslID
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			t.Fatal(err)
		}
		out = append(out, libfossil.FslID(rid))
	}
	return out
}

// TestAddBranchTagRechecksLeaves pins draft-fossil-repo-state-01 Sections 7.3
// and 7.6: a branch tag applied by a tag control artifact rechecks the
// check-in and its parent before the transaction commits. Moving c2 onto
// feat leaves c1, on trunk, without a same-branch child, so c1 is a leaf
// again; c2 stays one.
func TestAddBranchTagRechecksLeaves(t *testing.T) {
	r := setupTestRepo(t)
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	commit := func(label string, parent libfossil.FslID, n int) libfossil.FslID {
		t.Helper()
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "f.txt", Content: []byte(label)}},
			Comment: label, User: "testuser", Parent: parent,
			Time: at.Add(time.Duration(n) * time.Minute),
		})
		if err != nil {
			t.Fatalf("Checkin(%s): %v", label, err)
		}
		return rid
	}
	c1 := commit("c1", 0, 0)
	c2 := commit("c2", c1, 1)
	if got := leafRids(t, r); !slices.Equal(got, []libfossil.FslID{c2}) {
		t.Fatalf("before: leaves = %v, want [%d]", got, c2)
	}
	if _, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: c2, TagName: "branch", TagType: tag.TagPropagating, Value: "feat",
		User: "testuser", Time: at.Add(time.Hour),
	}); err != nil {
		t.Fatalf("AddTag: %v", err)
	}
	if got := leafRids(t, r); !slices.Equal(got, []libfossil.FslID{c1, c2}) {
		t.Fatalf("after: leaves = %v, want [%d %d]", got, c1, c2)
	}
}

// TestApplyTagWithTxBranchNeedsLeaves pins the programmer-error contract: a
// branch tag applied on a caller's transaction must name the batch's leaf
// schedule, or its rechecks would be lost.
func TestApplyTagWithTxBranchNeedsLeaves(t *testing.T) {
	r := setupTestRepo(t)
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "f.txt", Content: []byte("x")}},
		Comment: "c1", User: "testuser",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("ApplyTagWithTx with a branch tag and no Leaves did not panic")
		}
	}()
	_ = tag.ApplyTagWithTx(r.DB(), tag.ApplyOpts{
		TargetRID: rid, SrcRID: rid, TagName: "branch", TagType: tag.TagSingleton,
		Value: "x", MTime: 2470000,
	})
}
