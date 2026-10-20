package libfossil

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// Public narrowing keeps bytes; only an explicit compaction reclaims them.
func TestRepoCompactStore(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "compact.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, _, err = r.Commit(CommitOpts{Files: []FileToCommit{
		{Name: "docs/out", Content: []byte("outside")},
		{Name: "src/in", Content: []byte("inside")},
	}, Comment: "files", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate acknowledged publication. Compaction protects unpublished bytes.
	if _, err := r.DB().Exec("DELETE FROM unsent"); err != nil {
		t.Fatal(err)
	}
	c, err := NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetStoreCone(c); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := r.DB().QueryRow("SELECT count(*) FROM phantom").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("narrowing created %d phantoms", before)
	}
	if n, err := r.CompactStore(context.Background()); err != nil || n != 1 {
		t.Fatalf("CompactStore = %d, %v; want 1, nil", n, err)
	}
	if n, err := r.CompactStore(context.Background()); err != nil || n != 0 {
		t.Fatalf("repeat CompactStore = %d, %v; want 0, nil", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := r.CompactStore(ctx); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled CompactStore = %d, %v", n, err)
	}
}
