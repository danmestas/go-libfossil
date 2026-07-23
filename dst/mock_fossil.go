package dst

import (
	"context"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/repo"
	libsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// MockFossil simulates a Fossil master server using the real HandleSync
// handler. It manages its own repo and implements sync.Transport by
// dispatching xfer messages to HandleSyncWithOpts.
type MockFossil struct {
	repo    *repo.Repo
	buggify libsync.BuggifyChecker
}

// Verify interface compliance at compile time.
var _ libsync.Transport = (*MockFossil)(nil)

// NewMockFossil creates a MockFossil backed by the given repo.
func NewMockFossil(r *repo.Repo) *MockFossil {
	if r == nil {
		panic("dst.NewMockFossil: r must not be nil")
	}
	return &MockFossil{repo: r}
}

// SetBuggify configures fault injection for the handler.
func (f *MockFossil) SetBuggify(b libsync.BuggifyChecker) {
	f.buggify = b
}

// Repo returns the mock fossil's repository (for seeding and invariants).
func (f *MockFossil) Repo() *repo.Repo {
	return f.repo
}

// Exchange handles one xfer request/response round by delegating to
// the real HandleSyncWithOpts. This ensures the DST tests exercise the
// same code path as production servers.
func (f *MockFossil) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	if req == nil {
		panic("MockFossil.Exchange: req must not be nil")
	}
	return libsync.HandleSyncWithOpts(ctx, f.repo, req, libsync.HandleOpts{
		Buggify: f.buggify,
	})
}

// StoreArtifact adds a raw artifact to the mock fossil's repo.
// Returns the UUID. Used by tests to seed the master with content.
func (f *MockFossil) StoreArtifact(data []byte) (string, error) {
	if data == nil {
		panic("MockFossil.StoreArtifact: data must not be nil")
	}
	var uuid string
	err := f.repo.WithTx(func(tx *db.Tx) error {
		_, u, err := blob.Store(tx, data)
		if err != nil {
			return err
		}
		uuid = u
		return nil
	})
	return uuid, err
}
