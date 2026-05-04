package libfossil

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func commit(t *testing.T, r *Repo, parent int64, comment string, files []FileToCommit) int64 {
	t.Helper()
	rid, _, err := r.Commit(CommitOpts{
		ParentID: parent,
		Files:    files,
		Comment:  comment,
		User:     "test",
	})
	if err != nil {
		t.Fatalf("Commit %q: %v", comment, err)
	}
	return rid
}

func TestDiff_Modification(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nworld\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nbrave new world\n")},
	})

	entries, err := r.Diff(a, b, "hello.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Name != "hello.txt" {
		t.Errorf("Name = %q, want %q", e.Name, "hello.txt")
	}
	if !strings.Contains(e.Unified, "a/hello.txt") || !strings.Contains(e.Unified, "b/hello.txt") {
		t.Errorf("missing headers in unified output:\n%s", e.Unified)
	}
	if !strings.Contains(e.Unified, "-world") || !strings.Contains(e.Unified, "+brave new world") {
		t.Errorf("missing hunk lines in unified output:\n%s", e.Unified)
	}
}

func TestDiff_Identical(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\n")},
		{Name: "other.txt", Content: []byte("first\n")},
	})
	b := commit(t, r, a, "touch other", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\n")},
		{Name: "other.txt", Content: []byte("second\n")},
	})

	entries, err := r.Diff(a, b, "hello.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("want empty slice for identical content, got %d entries:\n%+v", len(entries), entries)
	}
}

func TestDiff_Addition(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "keep.txt", Content: []byte("anchor\n")},
	})
	b := commit(t, r, a, "add new", []FileToCommit{
		{Name: "new.txt", Content: []byte("line one\nline two\n")},
	})

	entries, err := r.Diff(a, b, "new.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	out := entries[0].Unified
	if !strings.Contains(out, "+line one") || !strings.Contains(out, "+line two") {
		t.Errorf("expected insertion lines, got:\n%s", out)
	}
	if strings.Contains(out, "-line one") {
		t.Errorf("unexpected deletion marker for pure addition:\n%s", out)
	}
}

func TestDiff_Deletion(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "doomed.txt", Content: []byte("goodbye\ncruel world\n")},
		{Name: "keep.txt", Content: []byte("anchor\n")},
	})
	b := commit(t, r, a, "drop doomed", []FileToCommit{
		{Name: "keep.txt", Content: []byte("anchor\n")},
	})

	entries, err := r.Diff(a, b, "doomed.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	out := entries[0].Unified
	if !strings.Contains(out, "-goodbye") || !strings.Contains(out, "-cruel world") {
		t.Errorf("expected deletion lines, got:\n%s", out)
	}
	if strings.Contains(out, "+goodbye") {
		t.Errorf("unexpected insertion marker for pure deletion:\n%s", out)
	}
}

func TestDiff_AbsentBothSides(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "only.txt", Content: []byte("x\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "only.txt", Content: []byte("y\n")},
	})

	entries, err := r.Diff(a, b, "never-existed.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("want empty slice for file absent in both checkins, got %d entries:\n%+v", len(entries), entries)
	}
}

func TestReadFile_Present(t *testing.T) {
	r := newTestRepo(t)
	rid := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nworld\n")},
		{Name: "other.txt", Content: []byte("other\n")},
	})

	data, err := r.ReadFile(rid, "hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello\nworld\n" {
		t.Errorf("content = %q, want %q", data, "hello\nworld\n")
	}
}

func TestReadFile_AcrossRevs(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "f.txt", Content: []byte("first\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "f.txt", Content: []byte("second\n")},
	})

	got, err := r.ReadFile(a, "f.txt")
	if err != nil {
		t.Fatalf("ReadFile(a): %v", err)
	}
	if string(got) != "first\n" {
		t.Errorf("rev a = %q, want %q", got, "first\n")
	}
	got, err = r.ReadFile(b, "f.txt")
	if err != nil {
		t.Fatalf("ReadFile(b): %v", err)
	}
	if string(got) != "second\n" {
		t.Errorf("rev b = %q, want %q", got, "second\n")
	}
}

func TestReadFile_NotInCheckin(t *testing.T) {
	r := newTestRepo(t)
	rid := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "only.txt", Content: []byte("x\n")},
	})

	_, err := r.ReadFile(rid, "missing.txt")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("err = %v, want errors.Is ErrFileNotFound", err)
	}
}

func TestReadFile_EmptyFilePath(t *testing.T) {
	r := newTestRepo(t)
	rid := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "x.txt", Content: []byte("x\n")},
	})

	_, err := r.ReadFile(rid, "")
	if err == nil {
		t.Fatal("expected error for empty filePath, got nil")
	}
	if !strings.Contains(err.Error(), "filePath is required") {
		t.Errorf("err = %q, want message mentioning filePath", err.Error())
	}
}

func TestDiff_WholeCheckin_Identical(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\n")},
		{Name: "other.txt", Content: []byte("other\n")},
	})

	entries, err := r.Diff(a, a, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if entries == nil {
		t.Fatal("entries = nil, want empty non-nil slice for identical checkins")
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0:\n%+v", len(entries), entries)
	}
}

func TestDiff_WholeCheckin_SingleFileChange(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nworld\n")},
		{Name: "stable.txt", Content: []byte("unchanged\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nbrave new world\n")},
		{Name: "stable.txt", Content: []byte("unchanged\n")},
	})

	entries, err := r.Diff(a, b, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1:\n%+v", len(entries), entries)
	}
	if entries[0].Name != "hello.txt" {
		t.Errorf("Name = %q, want %q", entries[0].Name, "hello.txt")
	}
	if !strings.Contains(entries[0].Unified, "+brave new world") {
		t.Errorf("missing expected hunk in unified diff:\n%s", entries[0].Unified)
	}
}

func TestDiff_WholeCheckin_MultiFileChange(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "a.txt", Content: []byte("a-old\n")},
		{Name: "b.txt", Content: []byte("b-old\n")},
		{Name: "c.txt", Content: []byte("c-stable\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "a.txt", Content: []byte("a-new\n")},
		{Name: "b.txt", Content: []byte("b-new\n")},
		{Name: "c.txt", Content: []byte("c-stable\n")},
	})

	entries, err := r.Diff(a, b, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2:\n%+v", len(entries), entries)
	}
	// Deterministic order: sorted by Name.
	if entries[0].Name != "a.txt" || entries[1].Name != "b.txt" {
		t.Errorf("entry order = [%q, %q], want [a.txt, b.txt]", entries[0].Name, entries[1].Name)
	}
	if !strings.Contains(entries[0].Unified, "-a-old") || !strings.Contains(entries[0].Unified, "+a-new") {
		t.Errorf("a.txt hunk missing expected lines:\n%s", entries[0].Unified)
	}
	if !strings.Contains(entries[1].Unified, "-b-old") || !strings.Contains(entries[1].Unified, "+b-new") {
		t.Errorf("b.txt hunk missing expected lines:\n%s", entries[1].Unified)
	}
}

func TestDiff_WholeCheckin_AdditionOnly(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "anchor.txt", Content: []byte("anchor\n")},
	})
	b := commit(t, r, a, "add new", []FileToCommit{
		{Name: "anchor.txt", Content: []byte("anchor\n")},
		{Name: "new.txt", Content: []byte("brand new\n")},
	})

	entries, err := r.Diff(a, b, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1:\n%+v", len(entries), entries)
	}
	if entries[0].Name != "new.txt" {
		t.Errorf("Name = %q, want %q", entries[0].Name, "new.txt")
	}
	if !strings.Contains(entries[0].Unified, "+brand new") {
		t.Errorf("expected addition hunk, got:\n%s", entries[0].Unified)
	}
	if strings.Contains(entries[0].Unified, "-brand new") {
		t.Errorf("unexpected deletion marker for pure addition:\n%s", entries[0].Unified)
	}
}

func TestDiff_WholeCheckin_DeletionOnly(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "anchor.txt", Content: []byte("anchor\n")},
		{Name: "doomed.txt", Content: []byte("goodbye\n")},
	})
	b := commit(t, r, a, "drop doomed", []FileToCommit{
		{Name: "anchor.txt", Content: []byte("anchor\n")},
	})

	entries, err := r.Diff(a, b, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1:\n%+v", len(entries), entries)
	}
	if entries[0].Name != "doomed.txt" {
		t.Errorf("Name = %q, want %q", entries[0].Name, "doomed.txt")
	}
	if !strings.Contains(entries[0].Unified, "-goodbye") {
		t.Errorf("expected deletion hunk, got:\n%s", entries[0].Unified)
	}
	if strings.Contains(entries[0].Unified, "+goodbye") {
		t.Errorf("unexpected insertion marker for pure deletion:\n%s", entries[0].Unified)
	}
}
