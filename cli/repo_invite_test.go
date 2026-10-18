package cli_test

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"
)

// An invite's expiry is stored in user.cexpire as fossil keeps that column:
// a Julian day.
func TestRepoInviteStoresJulianExpiry(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "r.fossil")
	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	before := libfossil.TimeToJulian(time.Now().Add(24 * time.Hour))
	cmd := &cli.RepoInviteCmd{Login: "bob", Cap: "oi", TTL: 24 * time.Hour}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("invite: %v", err)
	}
	after := libfossil.TimeToJulian(time.Now().Add(24 * time.Hour))

	r, err = libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var typ string
	var jd float64
	if err := r.DB().QueryRow("SELECT typeof(cexpire), CAST(cexpire AS REAL) FROM user WHERE login='bob'").
		Scan(&typ, &jd); err != nil {
		t.Fatal(err)
	}
	const slack = 1.0 / 86400 // the stored value is whole milliseconds
	if typ != "real" || jd < before-slack || jd > after+slack || math.IsNaN(jd) {
		t.Fatalf("cexpire = %s %v, want a Julian day in [%v, %v]", typ, jd, before, after)
	}
}
