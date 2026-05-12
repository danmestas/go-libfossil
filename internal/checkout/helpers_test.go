package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func newTestRepoWithCheckin(t *testing.T) (*repo.Repo, func()) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/test.fossil"
	r, err := repo.CreateWithEnv(path, "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "hello.txt", Content: []byte("hello world\n")},
			{Name: "src/main.go", Content: []byte("package main\n")},
			{Name: "README.md", Content: []byte("# Test\n")},
		},
		Comment: "initial checkin",
		User:    "test",
		Parent:  0,
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	return r, func() { r.Close() }
}

// testObserver is a flexible test observer with function fields.
type testObserver struct {
	onExtractStarted       func(context.Context, ExtractStart) context.Context
	onExtractFileCompleted func(context.Context, string, UpdateChange)
	onExtractCompleted     func(context.Context, ExtractEnd)
	onScanStarted          func(context.Context) context.Context
	onScanCompleted        func(context.Context, ScanEnd)
	onCommitStarted        func(context.Context, CommitStart) context.Context
	onCommitCompleted      func(context.Context, CommitEnd)
	onError                func(context.Context, error)
}

func (o *testObserver) ExtractStarted(ctx context.Context, e ExtractStart) context.Context {
	if o.onExtractStarted != nil {
		return o.onExtractStarted(ctx, e)
	}
	return ctx
}

func (o *testObserver) ExtractFileCompleted(ctx context.Context, name string, change UpdateChange) {
	if o.onExtractFileCompleted != nil {
		o.onExtractFileCompleted(ctx, name, change)
	}
}

func (o *testObserver) ExtractCompleted(ctx context.Context, e ExtractEnd) {
	if o.onExtractCompleted != nil {
		o.onExtractCompleted(ctx, e)
	}
}

func (o *testObserver) ScanStarted(ctx context.Context) context.Context {
	if o.onScanStarted != nil {
		return o.onScanStarted(ctx)
	}
	return ctx
}

func (o *testObserver) ScanCompleted(ctx context.Context, e ScanEnd) {
	if o.onScanCompleted != nil {
		o.onScanCompleted(ctx, e)
	}
}

func (o *testObserver) CommitStarted(ctx context.Context, e CommitStart) context.Context {
	if o.onCommitStarted != nil {
		return o.onCommitStarted(ctx, e)
	}
	return ctx
}

func (o *testObserver) CommitCompleted(ctx context.Context, e CommitEnd) {
	if o.onCommitCompleted != nil {
		o.onCommitCompleted(ctx, e)
	}
}

func (o *testObserver) Error(ctx context.Context, err error) {
	if o.onError != nil {
		o.onError(ctx, err)
	}
}
