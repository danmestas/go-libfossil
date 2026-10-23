package sync

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestServeClonesBesideAWriter has dense and horizon clones served at once
// while another connection holds the origin's write lock longer than its
// busy timeout, as a push's request does. A request that only reads must
// not wait for that lock: every request had run in a write transaction, so
// a clone arriving while another request was served got
// "db.WithTx begin: database is locked (SQLITE_BUSY)" and gave up.
func TestServeClonesBesideAWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("holds the write lock past the busy timeout")
	}
	w := newMemoWorld(t, 7)
	for range 24 {
		w.commit()
	}
	for len(w.pending) > 0 {
		w.push()
	}
	// A repository fossil made has no table-sync schema; serving one must
	// not create it.
	if _, err := w.r.DB().Exec("DROP TABLE IF EXISTS _sync_schema"); err != nil {
		t.Fatal(err)
	}
	horizon := w.checkins[len(w.checkins)/2]
	server := httptest.NewServer(XferHandler(w.r, HandleSync))
	t.Cleanup(server.Close)

	writer, err := repo.Open(w.r.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	held, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writer.WithTx(func(*db.Tx) error {
			close(held)
			select {
			case <-release:
			case <-time.After(time.Minute):
			}
			return nil
		})
	}()
	<-held

	modes := map[string]*cone.Cone{"dense": nil}
	for name, depth := range map[string]cone.HorizonDepth{
		"content": cone.DepthContent, "structural": cone.DepthStructural} {
		h, err := cone.NewHorizon(horizon, cone.RuleDescendants, depth)
		if err != nil {
			t.Fatal(err)
		}
		if modes[name], err = cone.NewWithHorizon(nil, nil, h); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	errs := make(chan error, len(modes))
	var wg gosync.WaitGroup
	for name, c := range modes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r, _, err := Clone(ctx, filepath.Join(dir, name+".fossil"),
				&HTTPTransport{URL: server.URL}, CloneOpts{StoreCone: c})
			if err != nil {
				errs <- fmt.Errorf("%s clone: %w", name, err)
				return
			}
			errs <- r.Close()
		}()
	}
	wg.Wait()
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatalf("writer: %v", err)
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
