package sync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// newProjectCodeRepo creates a repo and returns it alongside its stored
// project-code.
func newProjectCodeRepo(t *testing.T) (*repo.Repo, string) {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "repo.fossil"), "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	code, err := r.Config("project-code")
	if err != nil {
		t.Fatalf("repo.Config(project-code): %v", err)
	}
	if code == "" {
		t.Fatal("repo.Create left project-code empty")
	}
	return r, code
}

// captureLogin runs one converged sync round and returns the login card the
// client put on the wire (nil when it sent none).
func captureLogin(t *testing.T, r *repo.Repo, opts SyncOpts) (*xfer.LoginCard, *SyncResult, error) {
	t.Helper()
	var login *xfer.LoginCard
	mt := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		for _, c := range req.Cards {
			if lc, ok := c.(*xfer.LoginCard); ok {
				login = lc
			}
		}
		return &xfer.Message{}
	}}
	res, err := Sync(context.Background(), r, mt, opts)
	return login, res, err
}

// A password does not make ProjectCode the caller's problem: the repo being
// synced already stores its own project-code, so the login card derives from
// it (issue #199).
func TestSyncDerivesProjectCodeForLoginCard(t *testing.T) {
	r, code := newProjectCodeRepo(t)

	login, res, err := captureLogin(t, r, SyncOpts{Pull: true, User: "syncer", Password: "pw123"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res == nil {
		t.Fatal("Sync returned nil result on success")
	}
	if login == nil {
		t.Fatal("no login card sent")
	}
	want := sha1hex(login.Nonce + sha1hex(code+"/syncer/pw123"))
	if login.Signature != want {
		t.Fatalf("Signature = %q, want %q (derived from repo project-code %s)", login.Signature, want, code)
	}
}

// An explicitly supplied project code still wins over the derived one.
func TestSyncExplicitProjectCodeWins(t *testing.T) {
	r, code := newProjectCodeRepo(t)
	const explicit = "0123456789abcdef0123456789abcdef01234567"
	if explicit == code {
		t.Fatal("test constant collides with the generated project-code")
	}

	login, _, err := captureLogin(t, r, SyncOpts{
		Pull: true, User: "syncer", Password: "pw123", ProjectCode: explicit,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if login == nil {
		t.Fatal("no login card sent")
	}
	want := sha1hex(login.Nonce + sha1hex(explicit+"/syncer/pw123"))
	if login.Signature != want {
		t.Fatalf("Signature = %q, want %q (explicit project code)", login.Signature, want)
	}
}

// A project code that genuinely cannot be determined is a caller error, not a
// crash.
func TestSyncUnderivableProjectCodeReturnsError(t *testing.T) {
	r, _ := newProjectCodeRepo(t)
	if _, err := r.DB().Exec("DELETE FROM config WHERE name='project-code'"); err != nil {
		t.Fatalf("delete project-code: %v", err)
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Sync panicked instead of returning an error: %v", p)
		}
	}()
	_, _, err := captureLogin(t, r, SyncOpts{Pull: true, User: "syncer", Password: "pw123"})
	if err == nil {
		t.Fatal("Sync succeeded without a project code")
	}
	if !strings.Contains(err.Error(), "project-code") {
		t.Fatalf("err = %v, want it to name project-code", err)
	}
}

// panicTransport panics on Exchange, standing in for any panic raised inside
// the round loop.
type panicTransport struct{ msg string }

func (p *panicTransport) Exchange(context.Context, *xfer.Message) (*xfer.Message, error) {
	panic(p.msg)
}

// Sync's result-not-nil invariant only holds on a normal return; asserting it
// while a panic unwinds replaces the real cause (issue #199).
func TestSyncPanicSurfacesOriginalCause(t *testing.T) {
	r, _ := newProjectCodeRepo(t)

	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("expected the transport panic to propagate")
		}
		if got, _ := p.(string); got != "boom" {
			t.Fatalf("recovered %v, want the original cause %q", p, "boom")
		}
	}()
	_, _ = Sync(context.Background(), r, &panicTransport{msg: "boom"}, SyncOpts{Pull: true})
}
