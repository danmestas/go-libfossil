//go:build !js && !windows

package libfossil

import (
	"bytes"
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/clearsign/signtest"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// TestCommitSigning drives signing through the public API: with
// SettingClearsign on, Commit stores the manifest signed by
// SettingPGPCommand and NoSign stores it plain.
func TestCommitSigning(t *testing.T) {
	r := newTestRepo(t)
	command, pub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	key, err := clearsign.ParseKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetConfig(SettingClearsign, "on"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetConfig(SettingPGPCommand, command); err != nil {
		t.Fatal(err)
	}
	files := []FileToCommit{{Name: "a.txt", Content: []byte("a\n")}}
	signed, _, err := r.Commit(CommitOpts{Files: files, Comment: "signed", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := r.Commit(CommitOpts{
		Files: files, Comment: "plain", User: "test", ParentID: signed, NoSign: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := func(rid int64) []byte {
		data, err := content.Expand(r.Inner().DB(), fsltype.FslID(rid))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := key.Verify(context.Background(), clearsign.SSHNamespace, stored(signed)); err != nil {
		t.Errorf("signed check-in: %v", err)
	}
	if data := stored(plain); !bytes.HasPrefix(data, []byte("C plain")) {
		t.Errorf("NoSign check-in is not plain:\n%s", data)
	}
}

// TestSetConfigSeedKey: SetConfig keeps an SSH key or an OpenPGP
// fingerprint as SettingHorizonSeedKey and refuses anything else.
func TestSetConfigSeedKey(t *testing.T) {
	r := newTestRepo(t)
	_, pub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	for _, ok := range []string{pub, "0123456789ABCDEF0123456789ABCDEF01234567", ""} {
		if err := r.SetConfig(SettingHorizonSeedKey, ok); err != nil {
			t.Errorf("SetConfig(%q): %v", ok, err)
		}
	}
	if err := r.SetConfig(SettingHorizonSeedKey, "not a key"); err == nil {
		t.Error("SetConfig accepted a value that is no key")
	}
}
