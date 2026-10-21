package libfossil

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// badRCardChild commits two files and stores a child naming the same files
// under a wrong R card, as fossil's test-content-put would.
func badRCardChild(t *testing.T) (*Repo, int64) {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	parent, _, err := r.Commit(CommitOpts{User: "u", Comment: "good", Files: []FileToCommit{
		{Name: "src/a.c", Content: []byte("a\n")}, {Name: "tools/t.c", Content: []byte("t\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := manifest.GetManifest(r.inner, fsltype.FslID(parent))
	if err != nil {
		t.Fatal(err)
	}
	parentUUID, err := r.inner.UUIDByRID(parent)
	if err != nil {
		t.Fatal(err)
	}
	d.C, d.R, d.D, d.P, d.Z = "bad", strings.Repeat("0", 32), d.D.Add(time.Hour), []string{parentUUID}, ""
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var rid fsltype.FslID
	if err := r.inner.WithTx(func(tx *db.Tx) error {
		rid, _, err = blob.Store(tx, data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(r.inner); err != nil {
		t.Fatal(err)
	}
	return r, int64(rid)
}

func TestExtractWarnsOnBadRCard(t *testing.T) {
	for _, setting := range []string{"", "off"} {
		r, bad := badRCardChild(t)
		if setting != "" {
			if err := r.SetConfig("repo-cksum", setting); err != nil {
				t.Fatal(err)
			}
		}
		narrow, err := NewCone([]string{"src"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		co, err := r.CreateCheckout(t.TempDir(), CheckoutCreateOpts{Cone: narrow})
		if err != nil {
			t.Fatal(err)
		}
		var warned []string
		err = co.Extract(bad, ExtractOpts{Warn: func(m string) { warned = append(warned, m) }})
		co.Close()
		if err != nil {
			t.Fatalf("repo-cksum=%q: extract of a bad R card failed: %v", setting, err)
		}
		want := 1
		if setting == "off" {
			want = 0
		}
		if len(warned) != want {
			t.Fatalf("repo-cksum=%q: warnings %q, want %d", setting, warned, want)
		}
	}
}
