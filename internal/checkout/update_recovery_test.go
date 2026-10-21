package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/manifest"
)

func TestUpdateRequiredFilesRetainsTargetOnStructuralRefusal(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "incoming\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := f.repo.DB().Exec(
		"UPDATE blob SET size=-1,content=NULL WHERE rid=?", remote,
	); err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := co.UpdateRequiredFiles(context.Background(), UpdateOpts{TargetRID: remote})
	var required *manifest.RequiredContentError
	if !errors.As(err, &required) {
		t.Fatalf("readiness error = %v, want structural required content", err)
	}
	if target != remote {
		t.Fatalf("refused target = %d, want resolved target %d", target, remote)
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("structural refusal mutated checkout: %q != %q: %v", before, after, err)
	}
}

func TestUpdateRequiredFilesDoesNotMutate(t *testing.T) {
	f := newSparseRepo(t, true)
	remote := f.commit(t, maps(sparseFiles, map[string]string{
		"src/a.c": "incoming\n", "tools/t.c": "outside incoming\n",
	}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	files, err := manifest.ListFiles(f.repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	var wanted string
	for _, file := range files {
		if file.Name == "src/a.c" || file.Name == "tools/t.c" {
			if file.Name == "src/a.c" {
				wanted = file.UUID
			}
			_, err := f.repo.DB().Exec(
				"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", file.UUID,
			)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	target, needs, err := co.UpdateRequiredFiles(context.Background(), UpdateOpts{TargetRID: remote})
	if err != nil || target != remote {
		t.Fatalf("update readiness target = %d, %v", target, err)
	}
	found := false
	for _, file := range needs {
		if !co.covers(file.Name) {
			t.Fatalf("update requested excluded payload: %+v", file)
		}
		found = found || file.UUID == wanted
	}
	if !found {
		t.Fatal("update readiness omitted unavailable covered target")
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("update readiness mutated checkout: %q != %q: %v", before, after, err)
	}
}

func TestSparseUpdateDoesNotRequireExcludedPayload(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "elided incoming\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	files, err := manifest.ListFiles(f.repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Name == "tools/t.c" {
			_, err := f.repo.DB().Exec(
				"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", file.UUID,
			)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := co.Update(UpdateOpts{TargetRID: remote}); err != nil {
		t.Fatalf("record-only update demanded outside payload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), "tools/t.c")); !os.IsNotExist(err) {
		t.Fatalf("update materialized excluded path: %v", err)
	}
}

func TestUpdateRequiredFilesIncludesMergeBase(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "incoming\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	name := filepath.Join(co.Dir(), "src/a.c")
	if err := os.WriteFile(name, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	base, err := manifest.ListFiles(f.repo, f.c1)
	if err != nil {
		t.Fatal(err)
	}
	_, needs, err := co.UpdateRequiredFiles(context.Background(), UpdateOpts{TargetRID: remote})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range base {
		if file.Name != "src/a.c" {
			continue
		}
		found := false
		for _, need := range needs {
			found = found || need.UUID == file.UUID
		}
		if !found {
			t.Fatal("update readiness omitted local merge base")
		}
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("readiness stored local scan: %q != %q: %v", before, after, err)
	}
}

func TestUpdateContextCancellationRollsBackFiles(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "incoming\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	name := filepath.Join(co.Dir(), "src/a.c")
	beforeBytes, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	_, err = co.UpdateContext(ctx, UpdateOpts{
		TargetRID: remote,
		Callback: func(string, UpdateChange) error {
			called = true
			cancel()
			return nil
		},
	})
	if !called || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled update callback=%v, error=%v", called, err)
	}
	afterBytes, err := os.ReadFile(name)
	if err != nil || string(beforeBytes) != string(afterBytes) {
		t.Fatalf("canceled update changed bytes: %q != %q: %v", beforeBytes, afterBytes, err)
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("canceled update changed metadata: %q != %q: %v", before, after, err)
	}
}

func TestUpdateContextCancellationRestoresBackup(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"src/new.c": "incoming\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	original := map[string]string{
		"src/new.c": "untracked\n", "src/new.c-original": "existing backup\n",
	}
	for name, data := range original {
		if err := os.WriteFile(filepath.Join(co.Dir(), name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := co.UpdateContext(ctx, UpdateOpts{
		TargetRID: remote,
		Callback:  func(string, UpdateChange) error { cancel(); return nil },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled update = %v", err)
	}
	for name, want := range original {
		data, err := os.ReadFile(filepath.Join(co.Dir(), name))
		if err != nil || string(data) != want {
			t.Fatalf("rollback %s = %q, %v; want %q", name, data, err, want)
		}
	}
}

func TestUpdateContextFinalizationFailureRestoresFiles(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "incoming\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	name := filepath.Join(co.Dir(), "src/a.c")
	beforeBytes, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec(`CREATE TRIGGER reject_update BEFORE INSERT ON vvar
		WHEN NEW.name='checkout' BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = co.UpdateContext(context.Background(), UpdateOpts{TargetRID: remote})
	if err == nil || !strings.Contains(err.Error(), "injected update failure") {
		t.Fatalf("update error = %v, want injected finalization failure", err)
	}
	afterBytes, err := os.ReadFile(name)
	if err != nil || string(beforeBytes) != string(afterBytes) {
		t.Fatalf("failed finalization changed bytes: %q != %q: %v", beforeBytes, afterBytes, err)
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("failed finalization changed metadata: %q != %q: %v", before, after, err)
	}
}

func TestUpdateCancellationRemovesRenamedDestination(t *testing.T) {
	r, a, b, _ := forkRepo(t,
		[]manifest.File{{Name: "old.txt", Content: []byte("a\nb\nc\n")}},
		[]manifest.File{{Name: "new.txt", OldName: "old.txt", Content: []byte("A\nb\nc\n")}},
		nil)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "old.txt", "a\nb\nC\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := co.UpdateContext(ctx, UpdateOpts{
		TargetRID: b,
		Callback:  func(string, UpdateChange) error { cancel(); return nil },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled renamed update = %v", err)
	}
	if data, _ := readWorking(t, co, "old.txt"); data != "a\nb\nC\n" {
		t.Fatalf("rollback lost old path edits: %q", data)
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("rollback retained renamed destination: %v", err)
	}
}

func TestUpdateFinalizationFailureRestoresMode(t *testing.T) {
	r, a, b, _ := forkRepo(t,
		[]manifest.File{{Name: "run.sh", Content: []byte("echo\n")}},
		[]manifest.File{{Name: "run.sh", Content: []byte("echo\n"), Perm: "x"}},
		nil)
	co := editsCheckout(t, r, a)
	name := filepath.Join(co.Dir(), "run.sh")
	before, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec(`CREATE TRIGGER reject_update BEFORE INSERT ON vvar
		WHEN NEW.name='checkout' BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = co.UpdateContext(context.Background(), UpdateOpts{TargetRID: b})
	if err == nil || !strings.Contains(err.Error(), "injected update failure") {
		t.Fatalf("update error = %v, want injected finalization failure", err)
	}
	after, err := os.Stat(name)
	if err != nil || before.Mode() != after.Mode() {
		t.Fatalf("failed finalization changed mode: %v -> %v: %v", before, after, err)
	}
}

func TestUpdateContextCanceledBeforeMutation(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "incoming\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = co.UpdateContext(ctx, UpdateOpts{TargetRID: remote})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled update = %v", err)
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("canceled update mutated checkout: %q != %q: %v", before, after, err)
	}
}
