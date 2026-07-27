package libfossil

import (
	"context"
	"path/filepath"
	"testing"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Supplying credentials must not make SyncOpts.ProjectCode mandatory — the
// *Repo being synced already knows its own project code (issue #199).
func TestRepoSyncWithCredentialsAndNoProjectCode(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "repo.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	var sawLogin bool
	transport := TransportFunc(func(_ context.Context, payload []byte) ([]byte, error) {
		req, err := xfer.Decode(payload, xfer.ContentTypeCompressed)
		if err != nil {
			return nil, err
		}
		for _, c := range req.Cards {
			if _, ok := c.(*xfer.LoginCard); ok {
				sawLogin = true
			}
		}
		return (&xfer.Message{}).Encode()
	})

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Sync panicked: %v", p)
		}
	}()
	res, err := r.Sync(context.Background(), transport, SyncOpts{
		Push:     true,
		User:     "syncer",
		Password: "pw123",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res == nil {
		t.Fatal("Sync returned nil result on success")
	}
	if !sawLogin {
		t.Fatal("no login card reached the transport")
	}
}
