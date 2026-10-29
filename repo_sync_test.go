package libfossil

import (
	"context"
	"path/filepath"
	"strings"
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

// The partial-mirror setting is the public switch for §5.3's clone-origin
// refusal: unset, a sparse store refuses a dense clone; set, it serves one.
func TestPartialMirrorSettingDrivesCloneRefusal(t *testing.T) {
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, _, err := source.Commit(CommitOpts{User: "test", Files: []FileToCommit{
		{Name: "src/a", Content: []byte("inside\n")}, {Name: "zzz/b", Content: []byte("outside\n")},
	}}); err != nil {
		t.Fatal(err)
	}
	c, err := NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SetStoreCone(c); err != nil {
		t.Fatal(err)
	}
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return source.HandleSync(ctx, payload)
	})
	for _, setting := range []string{"", "off", "on"} {
		if setting != "" {
			if err := source.SetConfig(SettingPartialMirror, setting); err != nil {
				t.Fatal(err)
			}
		}
		clone, _, err := Clone(context.Background(),
			filepath.Join(t.TempDir(), "clone-"+setting+".fossil"), peer, CloneOpts{})
		if setting != "on" {
			if err == nil || !strings.Contains(err.Error(), "configure a partial mirror") {
				t.Fatalf("partial-mirror=%q: clone error = %v, want the origin refusal", setting, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("partial-mirror=on: clone refused: %v", err)
		}
		clone.Close()
	}
}

// HandleOpts.MaxStoredBytes reaches the server: a push whose artifact stores
// over it is refused with the budget's error and leaves the origin as it was.
func TestHandleOptsMaxStoredBytesRefusesPush(t *testing.T) {
	origin, err := Create(filepath.Join(t.TempDir(), "origin.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	if err := origin.SetCaps("nobody", "gijoryz"); err != nil {
		t.Fatal(err)
	}
	serve := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return origin.HandleSyncWithOpts(ctx, payload, HandleOpts{MaxStoredBytes: 1024})
	})
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"),
		serve, CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	big := make([]byte, 4096)
	for i := range big {
		big[i] = byte(i*7919>>3 ^ i)
	}
	if _, _, err := clone.Commit(CommitOpts{User: "test", Comment: "big",
		Files: []FileToCommit{{Name: "big.bin", Content: big}}}); err != nil {
		t.Fatal(err)
	}
	res, err := clone.Sync(context.Background(), serve, SyncOpts{Push: true})
	refused := err != nil && strings.Contains(err.Error(), "stores at most 1024 bytes per artifact")
	if res != nil {
		for _, e := range res.Errors {
			refused = refused || strings.Contains(e, "stores at most 1024 bytes per artifact")
		}
	}
	if !refused {
		t.Fatalf("push of a big artifact was not refused by the budget: res=%+v err=%v", res, err)
	}
	if _, err := origin.ResolveVersion("tip"); err == nil {
		t.Fatal("the refused push left a check-in on the origin")
	}
}
