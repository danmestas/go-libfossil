//go:build !js && !windows

package conformance

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/clearsign/signtest"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
)

// TestSignedCheckin signs check-ins as fossil's commit does: with clearsign
// on, a check-in's manifest is stored in the envelope pgp-command makes, and
// the check-in is named by the signed bytes; NoSign stores one unsigned; a
// signer that fails abandons the commit and stores nothing. fossil and zit
// clone the signed check-ins and read them, and a check-in zit signs with
// the same key verifies here, so the SSH envelope is the same bytes in each.
func TestSignedCheckin(t *testing.T) {
	f := newSCSyncRepo(t, scSyncFiles())
	command, pub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	key, err := clearsign.ParseKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{clearsign.SettingClearsign, "on"}, {clearsign.SettingPGPCommand, command}} {
		if err := f.r.SetConfig(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	signedRID := hzCommit(t, f, "signed", []libfossil.FslID{f.c1}, nil)
	signed := sigStored(t, f.r, signedRID)
	if err := key.Verify(context.Background(), signed); err != nil {
		t.Fatalf("the signed check-in does not verify: %v\n%s", err, signed)
	}

	f.when = f.when.Add(time.Hour)
	plainRID, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{
		Files: []manifest.File{{Name: "plain", Content: []byte("plain\n")}}, Comment: "plain",
		User: "u", Parent: signedRID, Time: f.when, NoSign: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Verify(context.Background(), sigStored(t, f.r, plainRID)); err != clearsign.ErrUnsigned {
		t.Fatalf("NoSign check-in: Verify = %v, want ErrUnsigned", err)
	}

	// A command that runs and fails: "false" alone is fossil's spelling of off.
	if err := f.r.SetConfig(clearsign.SettingPGPCommand, "/usr/bin/false"); err != nil {
		t.Fatal(err)
	}
	before := sigCheckins(t, f.r)
	f.when = f.when.Add(time.Hour)
	if _, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{
		Files: []manifest.File{{Name: "failed", Content: []byte("failed\n")}}, Comment: "failed",
		User: "u", Parent: plainRID, Time: f.when,
	}); err == nil {
		t.Fatal("a check-in whose signer failed was committed")
	}
	if after := sigCheckins(t, f.r); after != before {
		t.Fatalf("a failed signer left %d check-ins, want %d", after, before)
	}
	if err := f.r.SetConfig(clearsign.SettingPGPCommand, command); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(sync.XferHandler(f.r, sync.HandleSync))
	defer server.Close()
	uuid, err := f.r.UUIDByRID(int64(signedRID))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("fossil", func(t *testing.T) { sigFossilReads(t, server.URL, uuid, signed) })
	t.Run("zit", func(t *testing.T) { sigZitBothWays(t, f, server.URL, uuid, command, key) })
}

func sigStored(t *testing.T, r *repo.Repo, rid libfossil.FslID) []byte {
	t.Helper()
	data, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sigCheckins(t *testing.T, r *repo.Repo) int {
	t.Helper()
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sigFossilReads has fossil 2.28 clone the origin: the signed check-in
// arrives byte for byte, fossil links it as a check-in, and the clone passes
// fossil's integrity check.
func sigFossilReads(t *testing.T, url, uuid string, signed []byte) {
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("fossil not installed")
	}
	path := filepath.Join(t.TempDir(), "fossil.fossil")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := ref.command(ctx, "clone", url, path).CombinedOutput(); err != nil {
		t.Fatalf("fossil clone: %v\n%s", err, out)
	}
	got, err := ref.command(ctx, "artifact", uuid, "-R", path).Output()
	if err != nil || !bytes.Equal(got, signed) {
		t.Fatalf("fossil artifact %.10s (%v):\n%s\nwant:\n%s", uuid, err, got, signed)
	}
	out, err := ref.command(ctx, "timeline", "-R", path, "-t", "ci", "-n", "0").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "["+uuid[:10]+"] signed") {
		t.Fatalf("fossil does not list the signed check-in (%v):\n%s", err, out)
	}
	if out, err := ref.command(ctx, "test-integrity", "-R", path).CombinedOutput(); err != nil {
		t.Fatalf("fossil test-integrity: %v\n%s", err, out)
	}
}

// sigZitBothWays has zit clone the origin and verify its store, then commit
// a check-in signed with the same key through its own signer, push it, and
// the signature verifies here.
func sigZitBothWays(t *testing.T, f *scRepo, url, uuid, command string, key clearsign.Key) {
	bin := zitBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	zc := filepath.Join(dir, "zclone")
	zitRun(t, bin, home, dir, "clone", url, zc)
	zitRun(t, bin, home, zc, "verify")
	// zit links the signed manifest as a check-in: info names its parent.
	parent, err := f.r.UUIDByRID(int64(f.c1))
	if err != nil {
		t.Fatal(err)
	}
	info, err := zitCommand(bin, home, zc, "info", uuid)
	if err != nil || !strings.Contains(info, "parent:  "+parent) {
		t.Fatalf("zit info of the signed check-in (%v):\n%s", err, info)
	}
	if err := os.WriteFile(filepath.Join(zc, "zsigned"), []byte("zit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zitRun(t, bin, home, zc, "add", "zsigned")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	commit := exec.CommandContext(ctx, bin, "commit", "-m", "zsigned", "--user", "u", "--sign")
	commit.Dir = zc
	commit.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "FOSSIL_HOME="+home,
		"USER=u", "ZIT_PGP_COMMAND="+command)
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("zit commit --sign: %v\n%s", err, out)
	}
	zitRun(t, bin, home, zc, "push", url)
	var rid libfossil.FslID
	err = f.r.DB().QueryRow("SELECT objid FROM event WHERE type='ci' AND comment='zsigned'").Scan(&rid)
	if err != nil {
		t.Fatalf("zit's signed check-in did not arrive: %v", err)
	}
	zitSigned := sigStored(t, f.r, rid)
	if err := key.Verify(context.Background(), zitSigned); err != nil {
		t.Fatalf("zit's signed check-in does not verify here: %v\n%s", err, zitSigned)
	}
}
