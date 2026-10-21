package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

func TestCommitRequiredContentBeforeScan(t *testing.T) {
	f := newSparseRepo(t, true)
	co := f.open(t, sparseCone(t), f.c1)
	if err := os.WriteFile(filepath.Join(co.Dir(), "src/a.c"), []byte("unscanned edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := co.Enqueue(EnqueueOpts{Paths: []string{"src/a.c"}}); err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	deck, err := manifest.GetManifest(f.repo, f.c1)
	if err != nil || len(deck.S) == 0 {
		t.Fatalf("tree manifest = %+v: %v", deck, err)
	}
	if _, err := f.repo.DB().Exec("UPDATE blob SET size=-1, content=NULL WHERE uuid=?", deck.S[0].UUID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := co.Commit(CommitOpts{Message: "missing structure", User: "test"}); err == nil {
		t.Fatal("commit accepted missing parent structure")
	} else {
		var required *manifest.RequiredContentError
		if !errors.As(err, &required) {
			t.Fatalf("commit refusal = %v, want required-content error", err)
		}
	}
	after, err := co.RecoverySnapshot()
	if err != nil || after != before {
		t.Fatalf("readiness refusal mutated checkout snapshot: %q != %q: %v", before, after, err)
	}
}

func TestCommitContextCanceledBeforeMutation(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := co.CommitContext(ctx, CommitOpts{Message: "canceled", User: "test"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled commit = %v", err)
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("cancellation mutated checkout snapshot: %q != %q: %v", before, after, err)
	}
}

type publicationCancelContext struct {
	context.Context
	published func() bool
}

func (c publicationCancelContext) Err() error {
	if c.published() {
		return context.Canceled
	}
	return c.Context.Err()
}

func TestCommitContextFinalizesPublishedCheckin(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	if err := os.WriteFile(filepath.Join(co.Dir(), "src/a.c"), []byte("published edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := db.OpenSQL(f.repo.Path(), db.OpenConfig{}, map[string]string{"mode": "ro"})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	checkins := func() int {
		t.Helper()
		var count int
		if err := reader.QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := checkins()
	ctx := publicationCancelContext{Context: context.Background(), published: func() bool { return checkins() > before }}
	rid, uuid, err := co.CommitContext(ctx, CommitOpts{Message: "publication boundary", User: "test"})
	if err != nil {
		t.Fatalf("published checkin did not finalize: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("fixture did not cancel after publication")
	}
	gotRID, gotUUID, err := co.Version()
	if err != nil || gotRID != rid || gotUUID != uuid || rid == f.c1 {
		t.Fatalf("checkout version = %d %s, want published %d %s: %v", gotRID, gotUUID, rid, uuid, err)
	}
}

func TestCommitCarriedFilesWithoutParent(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, nil, f.c1)
	if _, err := co.db.Exec("UPDATE vvar SET value='0' WHERE name='checkout'"); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec("DELETE FROM vvar WHERE name='checkout-hash'"); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec("UPDATE vfile SET vid=0"); err != nil {
		t.Fatal(err)
	}
	if files, err := co.CommitCarriedFiles(context.Background(), []string{"src/a.c"}); err != nil || len(files) != 0 {
		t.Fatalf("parentless carried files = %+v, %v", files, err)
	}
}

func TestCommitCarriedFilesSelectionWithoutMutation(t *testing.T) {
	f := newSparseRepo(t, true)
	co := f.open(t, sparseCone(t), f.c1)
	files, err := manifest.ListFiles(f.repo, f.c1)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Name == "src/b.c" {
			if _, err := f.repo.DB().Exec("UPDATE blob SET size=-1,content=NULL WHERE uuid=?", file.UUID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := co.Enqueue(EnqueueOpts{Paths: []string{"src/b.c"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(co.Dir(), "src/a.c"), []byte("unscanned edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		paths []string
		want  string
	}{
		{"explicit", []string{"src/a.c"}, "src/vendor/keep/k.c"},
		{"queue", nil, "src/a.c,src/vendor/keep/k.c"},
		{"directory", []string{"src"}, ""},
		{"all", []string{"."}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := co.CommitCarriedFiles(context.Background(), tc.paths)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, file := range files {
				if file.UUID == "" {
					t.Fatalf("carried file lacks immutable identity: %+v", file)
				}
				names = append(names, file.Name)
			}
			if got := strings.Join(names, ","); got != tc.want {
				t.Fatalf("carried files = %q, want %q", got, tc.want)
			}
		})
	}
	after, err := co.RecoverySnapshot()
	if err != nil || after != before {
		t.Fatalf("carried-file readiness mutated checkout: %q != %q: %v", before, after, err)
	}
}

func TestRecoverySnapshotTracksPendingAndQueue(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "pending\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	last, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	assertChanged := func() {
		t.Helper()
		next, err := co.RecoverySnapshot()
		if err != nil || next == last {
			t.Fatalf("snapshot did not observe metadata mutation: %q, %v", next, err)
		}
		last = next
	}
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	if _, err := co.db.Exec("UPDATE sparse_pending SET deleted=1 WHERE pathname='tools/t.c'"); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	if err := co.Enqueue(EnqueueOpts{Paths: []string{"src/a.c"}}); err != nil {
		t.Fatal(err)
	}
	assertChanged()
}
