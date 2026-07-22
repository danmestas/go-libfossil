package libfossil

import (
	"errors"
	"strings"
	"testing"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestUUIDFromRID_HappyPath(t *testing.T) {
	r := newTestRepo(t)
	rid, uuid, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "hello.txt", Content: []byte("hello\n")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := r.UUIDFromRID(rid)
	if err != nil {
		t.Fatalf("UUIDFromRID(%d): %v", rid, err)
	}
	if got != uuid {
		t.Errorf("UUIDFromRID(%d) = %q, want %q", rid, got, uuid)
	}
}

func TestUUIDFromRID_NotFound_ZeroRID(t *testing.T) {
	r := newTestRepo(t)

	_, err := r.UUIDFromRID(0)
	if err == nil {
		t.Fatal("expected error for rid=0, got nil")
	}
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Errorf("err = %v, want errors.Is ErrArtifactNotFound", err)
	}
	if !strings.Contains(err.Error(), "0") {
		t.Errorf("err = %q, want message to mention rid 0", err.Error())
	}
}

func TestUUIDFromRID_NotFound_LargeRID(t *testing.T) {
	r := newTestRepo(t)
	// Commit something so the repo has at least one blob; the target rid is
	// still well above any rid the repo could plausibly have allocated.
	_, _, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "x.txt", Content: []byte("x\n")}},
		Comment: "init",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	const missing = int64(999999999)
	_, err = r.UUIDFromRID(missing)
	if err == nil {
		t.Fatalf("expected error for rid=%d, got nil", missing)
	}
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Errorf("err = %v, want errors.Is ErrArtifactNotFound", err)
	}
	if !strings.Contains(err.Error(), "999999999") {
		t.Errorf("err = %q, want message to mention rid %d", err.Error(), missing)
	}
}
