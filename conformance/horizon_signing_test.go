//go:build !js && !windows

package conformance

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/clearsign/signtest"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// The signing cases (§5.2.4) run over hzStruct: an origin signs the seed it
// mints when its clearsign setting is on, with its pgp-command, as fossil
// and zit sign check-ins; a clone given the origin's key verifies every seed
// it holds against it and falls back to the content horizon from one that is
// unsigned or signed by another key. The cases sign with ssh-keygen, which
// clearsign verifies in process; TestHorizonSeedSigningPGP signs with gpg.

var (
	horizonSeedSigned   = Case{ID: "horizon.5.2.4.seed-signed", Req: "HZ-5.2-5"}
	horizonSeedVerified = Case{ID: "horizon.5.2.4.seed-key-verified", Req: "HZ-5.2-6"}
)

func horizonSigningCases() []Case { return []Case{horizonSeedSigned, horizonSeedVerified} }

func TestHorizonSeedSigning(t *testing.T) {
	check(t, horizonSeedSigned, func() error { return hzSeedSigned(t) })
	check(t, horizonSeedVerified, func() error { return hzSeedKeyVerified(t) })
}

// hzSigningOrigin turns on signing at x's origin with command.
func hzSigningOrigin(t *testing.T, x *hzStruct, command string) {
	t.Helper()
	for _, kv := range [][2]string{{clearsign.SettingClearsign, "on"}, {clearsign.SettingPGPCommand, command}} {
		if err := x.s.f.r.SetConfig(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
}

// keyedClone clones x's origin at C's structural horizon through peer with
// key as the clone's seed key; "" gives it none.
func (x *hzStruct) keyedClone(
	t *testing.T, peer sync.Transport, key string,
) (*repo.Repo, *sync.CloneResult, error) {
	t.Helper()
	r, result, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "structural.fossil"),
		peer, sync.CloneOpts{StoreCone: x.cone(t), PullPhantoms: true, SeedKey: key})
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { r.Close() })
	return r, result, nil
}

// hzSignedSeed checks that r holds the seed x's origin mints, signed by key
// in the envelope header names, and derives K densely from it.
func hzSignedSeed(
	t *testing.T, x *hzStruct, r *repo.Repo, result *sync.CloneResult, key, header string,
) ([]byte, error) {
	t.Helper()
	if result.HorizonFallback || result.StructuralServed == 0 {
		return nil, expectf("keyed clone fell back %v, structural in %d of %d rounds",
			result.HorizonFallback, result.StructuralServed, result.Rounds)
	}
	held, err := seed.Held(r.DB())
	if err != nil || held == nil {
		return nil, expectf("no seed held (%v)", err)
	}
	if !bytes.HasPrefix(held, []byte(header)) {
		return nil, expectf("the held seed is not signed:\n%s", held)
	}
	k, err := clearsign.ParseKey(key)
	if err != nil {
		return nil, err
	}
	if err := k.Verify(context.Background(), held); err != nil {
		return nil, expectf("the held seed does not verify: %v", err)
	}
	_, unsigned, err := x.s.mint(seedLoginGo)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(deck.StripClearsign(held), unsigned) {
		return nil, expectf("the signed seed wraps other cards than the origin mints:\n%s", held)
	}
	return held, x.s.derivesDense(t, r)
}

// hzSeedSigned: an origin with clearsign on serves its seed signed, a clone
// keyed with its public key verifies it and derives K densely, and a second
// clone is sent the same signed seed, since the origin keeps one signature
// per seed and tag state.
func hzSeedSigned(t *testing.T) error {
	x := newHZStruct(t)
	command, pub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	hzSigningOrigin(t, x, command)
	r, result, err := x.keyedClone(t, x.peer(), pub)
	if err != nil {
		return err
	}
	held, err := hzSignedSeed(t, x, r, result, pub, "-----BEGIN SSH SIGNED MESSAGE-----")
	if err != nil {
		return err
	}
	again, _, err := x.keyedClone(t, x.peer(), pub)
	if err != nil {
		return err
	}
	if second, _ := seed.Held(again.DB()); !bytes.Equal(second, held) {
		return expectf("a second clone holds another seed:\n%s\nthe first:\n%s", second, held)
	}
	var kept int
	if err := x.s.f.r.DB().QueryRow("SELECT count(*) FROM horizonseedsig").Scan(&kept); err != nil || kept != 1 {
		return expectf("origin keeps %d signed seeds (%v), want 1", kept, err)
	}
	return nil
}

// hzFellBack checks a keyed structural clone that fell back to the content
// horizon (§6.3) and holds no seed.
func hzFellBack(r *repo.Repo, result *sync.CloneResult, err error) error {
	if err != nil {
		return err
	}
	if !result.HorizonFallback {
		return expectf("the clone did not fall back")
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		return expectf("the clone holds a seed its key did not verify")
	}
	if d, err := hzStructDepth(r); err != nil || d != cone.DepthContent {
		return expectf("stored depth %v (%v), want content", d, err)
	}
	return nil
}

// hzSeedKeyVerified: a clone given its origin's key discards an unsigned
// seed, and one signed by another key, and falls back to the content
// horizon; a clone given no key accepts a signed seed; the key is the
// clone's own setting, so a rebuild re-verifies the held seed against it.
func hzSeedKeyVerified(t *testing.T) error {
	x := newHZStruct(t)
	command, pub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	_, otherPub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	if err := hzFellBack(x.keyedClone(t, x.peer(), pub)); err != nil {
		return expectf("unsigned seed, keyed clone: %v", err)
	}
	hzSigningOrigin(t, x, command)
	if err := hzFellBack(x.keyedClone(t, x.peer(), otherPub)); err != nil {
		return expectf("seed signed by another key: %v", err)
	}
	unkeyed, result, err := x.keyedClone(t, x.peer(), "")
	if err != nil {
		return err
	}
	if result.HorizonFallback {
		return expectf("a clone with no key refused a signed seed")
	}
	r, result, err := x.keyedClone(t, x.peer(), pub)
	if err != nil {
		return err
	}
	if _, err := hzSignedSeed(t, x, r, result, pub, "-----BEGIN SSH SIGNED MESSAGE-----"); err != nil {
		return err
	}
	if v, err := r.Config(clearsign.SettingSeedKey); err != nil || v != pub {
		return expectf("clone keeps seed key %q (%v), want %q", v, err, pub)
	}
	if v, _ := unkeyed.Config(clearsign.SettingSeedKey); v != "" {
		return expectf("an unkeyed clone keeps seed key %q", v)
	}
	if err := r.SetConfig(clearsign.SettingSeedKey, otherPub); err != nil {
		return err
	}
	if _, err := verify.Rebuild(r); err != nil {
		return err
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		return expectf("a rebuild kept a seed the clone's key does not verify")
	}
	return nil
}

// TestHorizonSeedSigningPGP signs the seed with gpg --clearsign, fossil's
// default signer. A PGP signature carries its time, so the second clone's
// seed equals the first's only because the origin keeps the signature.
func TestHorizonSeedSigningPGP(t *testing.T) {
	x := newHZStruct(t)
	command, fpr := signtest.GPGKey(t)
	hzSigningOrigin(t, x, command)
	r, result, err := x.keyedClone(t, x.peer(), fpr)
	if err != nil {
		t.Fatal(err)
	}
	held, err := hzSignedSeed(t, x, r, result, fpr, "-----BEGIN PGP SIGNED MESSAGE-----")
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := x.keyedClone(t, x.peer(), fpr)
	if err != nil {
		t.Fatal(err)
	}
	if second, _ := seed.Held(again.DB()); !bytes.Equal(second, held) {
		t.Fatalf("a second clone holds another seed:\n%s\nthe first:\n%s", second, held)
	}
}

// TestZitHorizonSeedSigned runs signed seeds against zit, which signs check-
// ins but not seeds: zit clones at the structural horizon from a signing
// go-libfossil origin, holds the signed seed and derives K as a dense clone
// does; and a go-libfossil clone keyed for zit's origin, whose seed is
// unsigned, falls back to the content horizon.
func TestZitHorizonSeedSigned(t *testing.T) {
	bin := zitHorizonBinary(t)
	x := newHZStruct(t)
	command, pub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	hzSigningOrigin(t, x, command)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(sync.XferHandler(x.s.f.r, sync.HandleSync))
	defer server.Close()

	zs := filepath.Join(dir, "zstructural")
	if out, err := zitCommand(bin, home, dir, "clone", server.URL, zs, "--no-open", "--once",
		"--horizon="+x.c3, "--structural"); err != nil || strings.Contains(out, "content horizon instead") {
		t.Fatalf("zit's structural clone from a signing origin: %v\n%s", err, out)
	}
	zitSeed, err := os.ReadFile(filepath.Join(zs, ".zit-cache", "horizon-seed"))
	if err != nil {
		t.Fatalf("zit held no seed: %v", err)
	}
	k, err := clearsign.ParseKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Verify(context.Background(), zitSeed); err != nil {
		t.Fatalf("zit holds a seed that does not verify against the origin's key: %v\n%s", err, zitSeed)
	}
	zd := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", server.URL, zd, "--no-open", "--once")
	if err := hzZitSameState(t, bin, home, zs, zd, []string{x.c3, x.c4, x.feat2}); err != nil {
		t.Fatalf("zit's structural clone of a signing origin: %v", err)
	}

	// zit serves its dense clone; its seeds are unsigned.
	zitURL := zitServe(t, bin, home, zd)
	if err := hzFellBack(x.keyedClone(t, &sync.HTTPTransport{URL: zitURL}, pub)); err != nil {
		t.Fatalf("keyed clone of zit's unsigned seed: %v", err)
	}
	r, result, err := x.keyedClone(t, &sync.HTTPTransport{URL: zitURL}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.HorizonFallback {
		t.Fatal("an unkeyed clone of zit fell back")
	}
	if err := x.s.derivesDense(t, r); err != nil {
		t.Fatalf("unkeyed clone of zit: %v", err)
	}
}

// TestHorizonSeedSigningNeedsRead: an origin signs a seed only for a
// requester that may read (§5.2.4 with §6.2): a request from one that may
// not runs no signer and writes nothing. The signer here leaves a marker and
// fails, so the control half, with read restored, shows a signer that ran.
func TestHorizonSeedSigningNeedsRead(t *testing.T) {
	x := newHZStruct(t)
	marker := filepath.Join(t.TempDir(), "signer-ran")
	hzSigningOrigin(t, x, "touch '"+marker+"'; false")
	if _, err := x.s.f.r.DB().Exec("UPDATE user SET cap='' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.keyedClone(t, x.peer(), ""); err == nil {
		t.Fatal("a requester that may not read cloned")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the origin ran its signer for a requester that may not read")
	}
	var tables int
	if err := x.s.f.r.DB().QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE name='horizonseedsig'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("the origin wrote signed seeds (%d tables, %v)", tables, err)
	}
	if _, err := x.s.f.r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.keyedClone(t, x.peer(), ""); err == nil {
		t.Fatal("a clone succeeded although the origin's signer fails")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the signer did not run for a requester that may read: %v", err)
	}
}

// TestHorizonSeedSigningKeyRotation: an origin keeps one signature per seed
// and command, so replacing the key behind an unchanged pgp-command would
// keep serving the old key's signature. With its own public key as its
// horizon-seed-key, the origin checks a kept signature against it and signs
// again when it no longer verifies.
func TestHorizonSeedSigningKeyRotation(t *testing.T) {
	x := newHZStruct(t)
	command, oldPub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	hzSigningOrigin(t, x, command)
	if err := x.s.f.r.SetConfig(clearsign.SettingSeedKey, oldPub); err != nil {
		t.Fatal(err)
	}
	r, result, err := x.keyedClone(t, x.peer(), oldPub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hzSignedSeed(t, x, r, result, oldPub, "-----BEGIN SSH SIGNED MESSAGE-----"); err != nil {
		t.Fatalf("before rotation: %v", err)
	}
	// Rotate: a new key at the path the unchanged command names.
	newCommand, newPub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	path := func(c string) string { return strings.Trim(c[strings.LastIndex(c, " -f ")+4:], "'") }
	for _, suffix := range []string{"", ".pub"} {
		data, err := os.ReadFile(path(newCommand) + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path(command)+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.s.f.r.SetConfig(clearsign.SettingSeedKey, newPub); err != nil {
		t.Fatal(err)
	}
	r, result, err = x.keyedClone(t, x.peer(), newPub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hzSignedSeed(t, x, r, result, newPub, "-----BEGIN SSH SIGNED MESSAGE-----"); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
}
