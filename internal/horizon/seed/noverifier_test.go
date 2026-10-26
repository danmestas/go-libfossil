//go:build !js && !windows

package seed_test

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/clearsign/signtest"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
)

// TestSSHSeedKeyWithoutVerifierFailsClosed: this test binary links no SSH
// verifier. A clone with an SSH horizon-seed-key refuses a seed signed by
// that very key, as one it could not judge: it is never held, and one held
// before is neither applied nor discarded. With no key, the unsigned seed
// is taken as before.
func TestSSHSeedKeyWithoutVerifierFailsClosed(t *testing.T) {
	w := newWorld(t)
	clone := w.lacking(w.k(w.c3))
	_, data := w.mint(w.c3, "origin")
	command, pub := signtest.SSHKey(t, "ed25519", clearsign.SeedNamespace)
	signer := &clearsign.Signer{Command: command, Namespace: clearsign.SeedNamespace}
	signed, err := signer.Sign(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	name := hash.SHA3(signed)
	if err := clone.SetConfig(clearsign.SettingSeedKey, pub); err != nil {
		t.Fatal(err)
	}
	verify, err := seed.VerifierFor(clone.DB())
	if err != nil || verify == nil {
		t.Fatalf("VerifierFor = %v, %v; want a verifier for the configured key", verify, err)
	}
	err = hold(t, clone, name, signed, verify)
	if !errors.Is(err, clearsign.ErrSSHVerifierUnavailable) || errors.Is(err, seed.ErrProvenance) {
		t.Fatalf("Hold with no SSH verifier = %v, want ErrSSHVerifierUnavailable", err)
	}
	if held, _ := seed.Held(clone.DB()); held != nil {
		t.Fatal("a seed no verifier judged was held")
	}
	if err := hold(t, clone, name, signed, nil); err != nil {
		t.Fatal(err)
	}
	err = clone.WithTx(func(tx *db.Tx) error {
		applied, err := seed.Apply(tx, verify)
		if applied {
			t.Error("Apply with no SSH verifier applied the seed")
		}
		return err
	})
	if !errors.Is(err, clearsign.ErrSSHVerifierUnavailable) {
		t.Fatalf("Apply with no SSH verifier = %v, want ErrSSHVerifierUnavailable", err)
	}
	if held, _ := seed.Held(clone.DB()); held == nil {
		t.Error("a held seed was discarded for want of a verifier")
	}
}
