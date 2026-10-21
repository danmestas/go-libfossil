//go:build !js

package checkout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyDenseUndoWithoutConeStamp(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "updated\n"}), f.c1)
	co := f.open(t, nil, f.c1)
	if _, err := co.Update(UpdateOpts{TargetRID: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec("DELETE FROM vvar WHERE name='undo_sparse_cone'"); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(co.Dir(), "src/a.c")); err != nil || string(data) != sparseFiles["src/a.c"] {
		t.Fatalf("legacy dense undo bytes: %q, %v", data, err)
	}
}

func TestLegacySparseUndoAfterWideningRefused(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "legacy pending\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec("DELETE FROM vvar WHERE name='undo_sparse_cone'"); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err == nil || !strings.Contains(err.Error(), "legacy sparse undo image") {
		t.Fatalf("unstamped legacy undo = %v", err)
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("legacy refusal changed checkout metadata: %q != %q: %v", before, after, err)
	}
	if data, err := os.ReadFile(filepath.Join(co.Dir(), "tools/t.c")); err != nil || string(data) != sparseFiles["tools/t.c"] {
		t.Fatalf("legacy refusal changed outside bytes: %q, %v", data, err)
	}
}

func TestSparseRevertUndoAfterWideningRefused(t *testing.T) {
	f := newSparseRepo(t, false)
	merged := maps(sparseFiles, map[string]string{"tools/t.c": "retained merge\n"})
	remote := f.commit(t, merged, f.c1)
	original := sparseCone(t)
	co := f.open(t, original, f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err == nil || !strings.Contains(err.Error(), "undo image") {
		t.Fatalf("undo of sparse revert after widening = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(co.Dir(), "tools/t.c")); err != nil || string(got) != sparseFiles["tools/t.c"] {
		t.Fatalf("refused undo changed baseline bytes: %q, %v", got, err)
	}
	if _, err := co.SetCone(original); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "restored merge", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, merged)
}

func TestSparseDenseUndoRequiresWidening(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, nil, f.c1)
	file := filepath.Join(co.Dir(), "tools/t.c")
	if err := os.WriteFile(file, []byte("saved dense edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(sparseCone(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("dense undo under narrow cone = %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused undo materialized outside file: %v", err)
	}
	var count int
	if err := co.db.QueryRow("SELECT count(*) FROM vfile").Scan(&count); err != nil || count != len(sparseCovered) {
		t.Fatalf("refused undo changed managed rows: %d, %v", count, err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "saved dense edit\n" {
		t.Fatalf("widening lost saved undo bytes: %q, %v", got, err)
	}
}

func TestSparseDenseRedoRequiresWidening(t *testing.T) {
	f := newSparseRepo(t, false)
	target := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "redo target\n"}), f.c1)
	co := f.open(t, nil, f.c1)
	if _, err := co.Update(UpdateOpts{TargetRID: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(sparseCone(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Redo(); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("dense redo under narrow cone = %v", err)
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), "tools/t.c")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused redo materialized outside file: %v", err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(co.Dir(), "tools/t.c")); err != nil || string(got) != "redo target\n" {
		t.Fatalf("widening lost redo target: %q, %v", got, err)
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

func TestSparseMergeConflictReported(t *testing.T) {
	f := newSparseRepo(t, false)
	ours := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "ours\n"}), f.c1)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "theirs\n"}), f.c1)
	co := f.open(t, sparseCone(t), ours)
	if _, err := co.Merge(MergeOpts{Version: theirs}); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("outside both-changed merge = %v, want explicit widening refusal", err)
	}
	conflicts, err := co.Conflicts()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("outside conflicts = %q: %v", conflicts, err)
	}
}

func TestSparseBothChangedRequiresWideningWithoutSynthesis(t *testing.T) {
	f := newSparseRepo(t, false)
	base := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"}), f.c1)
	ours := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "ours\ntwo\nthree\nfour\nfive\nsix\nseven\n"}), base)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "one\ntwo\nthree\nfour\nfive\nsix\ntheirs\n"}), base)
	co := f.open(t, sparseCone(t), ours)
	var before int
	if err := f.repo.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: theirs}); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("expected widening refusal: %v", err)
	}
	var after, outside int
	if err := f.repo.DB().QueryRow("SELECT count(*) FROM blob").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("outside merge synthesized content: %d blobs became %d", before, after)
	}
	if err := co.db.QueryRow("SELECT count(*) FROM vfile WHERE pathname='tools/t.c'").Scan(&outside); err != nil || outside != 0 {
		t.Fatalf("outside managed rows = %d: %v", outside, err)
	}
	for _, table := range []string{"vmerge", "sparse_pending"} {
		var count int
		if err := co.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("refused merge left %s rows: %d, %v", table, count, err)
		}
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: theirs}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(co.Dir(), "tools/t.c"))
	if err != nil || string(data) != "ours\ntwo\nthree\nfour\nfive\nsix\ntheirs\n" {
		t.Fatalf("widened merge = %q: %v", data, err)
	}
	if conflicts, err := co.Conflicts(); err != nil || len(conflicts) != 0 {
		t.Fatalf("clean widened merge conflicts = %v: %v", conflicts, err)
	}
}

func TestSparseMergeFailureRecordsNoMerge(t *testing.T) {
	f := newSparseRepo(t, false)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "theirs\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.db.Exec(`CREATE TRIGGER reject_merge BEFORE INSERT ON sparse_pending
	 BEGIN SELECT RAISE(ABORT, 'injected merge failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: theirs}); err == nil {
		t.Fatal("merge succeeded despite injected metadata failure")
	}
	var parents int
	if err := co.db.QueryRow("SELECT count(*) FROM vmerge").Scan(&parents); err != nil || parents != 0 {
		t.Fatalf("failed merge left %d parent records: %v", parents, err)
	}
}

func TestSparseMergeDoesNotFollowOutsideSymlink(t *testing.T) {
	for _, tree := range []bool{false, true} {
		f := newSparseRepo(t, tree)
		remoteFiles := maps(sparseFiles, map[string]string{"tools/t.c": "remote\n", "tools/new.c": "new\n"})
		remote := f.commit(t, remoteFiles, f.c1)
		co := f.open(t, sparseCone(t), f.c1)
		outside := t.TempDir()
		sentinel := filepath.Join(outside, "t.c")
		if err := os.WriteFile(sentinel, []byte("do not touch\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(co.Dir(), "tools")); err != nil {
			t.Fatal(err)
		}
		if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
			t.Fatal(err)
		}
		if err := co.Revert(RevertOpts{}); err != nil {
			t.Fatal(err)
		}
		if _, err := co.Undo(); err != nil {
			t.Fatal(err)
		}
		rid, _, err := co.Commit(CommitOpts{Message: "record-only symlink", User: "test", Time: f.time.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		assertCheckinContent(t, f.repo, rid, remoteFiles)
		data, err := os.ReadFile(sentinel)
		if err != nil || string(data) != "do not touch\n" {
			t.Fatalf("outside sentinel changed: %q, %v", data, err)
		}
		if _, err := os.Lstat(filepath.Join(outside, "new.c")); !os.IsNotExist(err) {
			t.Fatalf("outside added path materialized: %v", err)
		}
	}
}

func TestSparseMergeCancel(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "remote\n", "tools/new.c": "new\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{Paths: []string{"tools/t.c"}}); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "cancel merge", User: "test", AllowEmpty: true, Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, sparseFiles)
}

func TestSparseMergeUndoRedo(t *testing.T) {
	f := newSparseRepo(t, false)
	remoteFiles := maps(sparseFiles, map[string]string{"tools/t.c": "remote\n", "src/a.c": "remote source\n"})
	remote := f.commit(t, remoteFiles, f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	var parents int
	if err := co.db.QueryRow("SELECT count(*) FROM vmerge").Scan(&parents); err != nil || parents != 0 {
		t.Fatalf("undo merge parents=%d: %v", parents, err)
	}
	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "redo merge", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, remoteFiles)
}
