package checkout

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/manifest"
)

func TestSparseUpdateRefusesBothChangedOutcome(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "merged\n"}), f.c1)
	target := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "target\n", "src/a.c": "target source\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(UpdateOpts{TargetRID: target}); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("both-changed outside update = %v", err)
	}
	if vid, _, err := co.Version(); err != nil || vid != f.c1 {
		t.Fatalf("refused update changed version: %d, %v", vid, err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "still pending merge", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, maps(sparseFiles, map[string]string{"tools/t.c": "merged\n"}))
}

func TestSparseModeMergeRevert(t *testing.T) {
	for _, named := range []bool{false, true} {
		f := newSparseRepo(t, false)
		baseline, err := manifest.ListFiles(f.repo, f.c1)
		if err != nil {
			t.Fatal(err)
		}
		files := make([]manifest.File, len(baseline))
		for i, file := range baseline {
			files[i] = manifest.File{Name: file.Name, UUID: file.UUID, Perm: file.Perm}
			if file.Name == "tools/t.c" {
				files[i].Perm = "x"
			}
		}
		remote, _, err := manifest.Checkin(f.repo, manifest.CheckinOpts{Files: files, Comment: "executable", User: "test", Parent: f.c1, Time: f.time.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		co := f.open(t, sparseCone(t), f.c1)
		if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
			t.Fatal(err)
		}
		opts := RevertOpts{}
		if named {
			opts.Paths = []string{"tools/t.c"}
		}
		if err := co.Revert(opts); err != nil {
			t.Fatal(err)
		}
		rid, _, err := co.Commit(CommitOpts{Message: "reverted mode", User: "test", AllowEmpty: true, Time: f.time.Add(2 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		got, err := manifest.ListFiles(f.repo, rid)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range got {
			if file.Name == "tools/t.c" && file.Perm != "" {
				t.Fatalf("named=%v: reverted merge leaked permission %q", named, file.Perm)
			}
		}
	}
}

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

func TestSparseUpdatePreservesPendingOutcome(t *testing.T) {
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
	for table, want := range map[string]int{"sparse_pending": 1, "vmerge": 0} {
		var count int
		if err := co.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("update %s rows: %d, want %d: %v", table, count, want, err)
		}
	}
	rid, _, err := co.Commit(CommitOpts{Message: "carried outside edit", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, maps(sparseFiles, map[string]string{"tools/t.c": "merged\n", "src/a.c": "target\n"}))
}
