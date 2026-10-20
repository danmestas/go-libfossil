package cli

import (
	"path/filepath"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// Selecting a recovery peer is read-only: an explicit URL wins over a saved
// one, and authentication remains in the returned options rather than config.
func TestRequiredContentFlags(t *testing.T) {
	r, err := libfossil.Create(filepath.Join(t.TempDir(), "repo.fossil"),
		libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	opts, err := (RecoveryFlags{}).requiredContent(r)
	if err != nil || opts.Peer != nil {
		t.Fatalf("no saved/explicit peer: %+v, %v", opts, err)
	}
	if err := r.SetConfig("last-sync-url", "https://saved@example.invalid/repo"); err != nil {
		t.Fatal(err)
	}
	opts, err = (RecoveryFlags{}).requiredContent(r)
	if err != nil || opts.Peer == nil || opts.Sync.User != "saved" {
		t.Fatalf("saved peer: %+v, %v", opts, err)
	}
	opts, err = (RecoveryFlags{Remote: "https://explicit@example.invalid/repo",
		SyncPass: "ephemeral"}).requiredContent(r)
	if err != nil || opts.Sync.User != "explicit" || opts.Sync.Password != "ephemeral" {
		t.Fatalf("explicit auth resolution: user=%q err=%v", opts.Sync.User, err)
	}
	if saved, err := r.Config("last-sync-url"); err != nil ||
		saved != "https://saved@example.invalid/repo" {
		t.Fatalf("recovery selection persisted a new peer: %q, %v", saved, err)
	}
	var credentials int
	if err := r.Inner().DB().QueryRow(
		"SELECT count(*) FROM config WHERE value='ephemeral'").Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if credentials != 0 {
		t.Fatal("recovery persisted credentials")
	}
}

func TestRequiredContentFlagsRefuseInvalidRemote(t *testing.T) {
	r, err := libfossil.Create(filepath.Join(t.TempDir(), "repo.fossil"),
		libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, remote := range []string{"file:///tmp/repo", "https://", "not a URL"} {
		if _, err := (RecoveryFlags{Remote: remote}).requiredContent(r); err == nil {
			t.Fatalf("accepted invalid explicit peer %q", remote)
		}
	}
}
