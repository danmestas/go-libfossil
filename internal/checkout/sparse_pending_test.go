package checkout

import (
	"strings"
	"testing"
	"time"
)

func TestSparseSecondMergeRefused(t *testing.T) {
	f := newSparseRepo(t, false)
	first := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "first\n"}), f.c1)
	second := f.commit(t, maps(sparseFiles, map[string]string{"doc/d.txt": "second\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: first}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: second}); err == nil || !strings.Contains(err.Error(), "merge is already pending") {
		t.Fatalf("second merge = %v", err)
	}
	var parents int
	if err := co.db.QueryRow("SELECT count(*) FROM vmerge WHERE id=0").Scan(&parents); err != nil || parents != 1 {
		t.Fatalf("merge parents = %d: %v", parents, err)
	}
}

func TestSparseUpdateConsumesPendingOverlay(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "merged\n"}), f.c1)
	target := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "target\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(UpdateOpts{TargetRID: target}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sparse_pending", "vmerge"} {
		var count int
		if err := co.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("update retained %s rows: %d, %v", table, count, err)
		}
	}
}

func TestSparsePendingUndoAfterUpdate(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "merged\n"}), f.c1)
	target := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "target\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(UpdateOpts{TargetRID: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "restored pending", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, maps(sparseFiles, map[string]string{"tools/t.c": "merged\n"}))
}
