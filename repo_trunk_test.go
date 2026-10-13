package libfossil

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/tag"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// commitAt commits one file on parent at the given time.
func commitAt(t *testing.T, r *Repo, parent int64, comment string, at time.Time) int64 {
	t.Helper()
	rid, _, err := r.Commit(CommitOpts{
		ParentID: parent,
		Files:    []FileToCommit{{Name: "f", Content: []byte(comment + "\n")}},
		Comment:  comment,
		User:     "test",
		Time:     at,
	})
	if err != nil {
		t.Fatalf("Commit %q: %v", comment, err)
	}
	return rid
}

func TestCurrentTrunk_FollowsMainBranchSetting(t *testing.T) {
	r := newTestRepo(t)
	base := commitBranch(t, r, 0, "trunk", "base", []FileToCommit{{Name: "f", Content: []byte("0\n")}})
	feat := makeBranch(t, r, base, "feat")

	got, err := r.CurrentTrunk()
	if err != nil {
		t.Fatalf("CurrentTrunk: %v", err)
	}
	if got != base {
		t.Errorf("CurrentTrunk with main-branch unset = %d, want %d", got, base)
	}
	if err := r.SetConfig("main-branch", "feat"); err != nil {
		t.Fatal(err)
	}
	if got, err = r.CurrentTrunk(); err != nil || got != feat {
		t.Errorf("CurrentTrunk with main-branch=feat = %d, %v; want %d", got, err, feat)
	}
}

func TestBranchOpenLeaf(t *testing.T) {
	r := newTestRepo(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := commitAt(t, r, 0, "base", t0)
	b1 := makeBranch(t, r, base, "feat")
	// newer is committed first, so it has the smaller rid; closed is the
	// newest of the three leaves but is closed.
	newer := commitAt(t, r, b1, "newer", time.Now().UTC().Add(2*time.Hour))
	older := commitAt(t, r, b1, "older", time.Now().UTC().Add(1*time.Hour))
	closed := commitAt(t, r, b1, "closed", time.Now().UTC().Add(3*time.Hour))
	_, err := tag.AddTag(r.Inner(), tag.TagOpts{
		TargetRID: fsltype.FslID(closed), TagName: "closed", TagType: tag.TagSingleton,
		User: "test", Time: time.Now().UTC().Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if older <= newer {
		t.Fatalf("setup: older rid %d should exceed newer rid %d", older, newer)
	}

	rid, open, err := r.BranchOpenLeaf("feat")
	if err != nil || !open || rid != newer {
		t.Errorf("BranchOpenLeaf(feat) = %d, %v, %v; want %d, true", rid, open, err, newer)
	}
	if rid, open, err := r.BranchOpenLeaf("nosuch"); err != nil || open || rid != 0 {
		t.Errorf("BranchOpenLeaf(nosuch) = %d, %v, %v; want 0, false, nil", rid, open, err)
	}
}
