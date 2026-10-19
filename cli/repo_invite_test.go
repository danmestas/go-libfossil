package cli_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"
	"github.com/danmestas/go-libfossil/internal/auth"
)

// An invite's expiry goes to go-libfossil's own config row, in UTC, and
// user.cexpire, fossil's web-login cookie expiry, is left alone (#441). The
// local zone is set away from UTC: the expiry is an instant, so it must not
// move with it.
func TestRepoInviteStoresExpiryApartFromCookie(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC-6", -6*3600)
	defer func() { time.Local = saved }()

	repoPath := filepath.Join(t.TempDir(), "r.fossil")
	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	cmd := &cli.RepoInviteCmd{Login: "bob", Cap: "oi", TTL: 24 * time.Hour}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("invite: %v", err)
	}
	after := time.Now().Add(24 * time.Hour)

	r, err = libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var cexpire sql.NullString
	if err := r.DB().QueryRow("SELECT cexpire FROM user WHERE login='bob'").Scan(&cexpire); err != nil {
		t.Fatal(err)
	}
	if cexpire.Valid {
		t.Errorf("invite wrote user.cexpire = %q; it is fossil's cookie expiry", cexpire.String)
	}
	var value string
	if err := r.DB().QueryRow("SELECT value FROM config WHERE name=?",
		auth.InviteExpiryKey("bob")).Scan(&value); err != nil {
		t.Fatalf("invite expiry row: %v", err)
	}
	expiry, err := time.Parse(time.RFC3339, value)
	if err != nil || expiry.Location() != time.UTC {
		t.Fatalf("invite expiry %q, want an RFC 3339 time in UTC", value)
	}
	if expiry.Before(before) || expiry.After(after) {
		t.Fatalf("invite expiry %v, want within [%v, %v]", expiry, before, after)
	}
}
