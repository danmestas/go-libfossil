package cli

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// A dense HTTP clone narrowed to src with zzz/b elided, saved as the repo
// at the returned path; savedURL, when set, becomes last-sync-url.
func coneStoreTarget(t *testing.T, savedURL bool) (string, *atomic.Int64) {
	t.Helper()
	source, err := libfossil.Create(filepath.Join(t.TempDir(), "source.fossil"), libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.Commit(libfossil.CommitOpts{User: "test", Files: []libfossil.FileToCommit{
		{Name: "src/a", Content: []byte("inside\n")}, {Name: "zzz/b", Content: []byte("outside\n")},
	}}); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := requiredHTTPServer(t, source, &requests)
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "target.fossil")
	target, _, err := libfossil.Clone(context.Background(), path,
		libfossil.NewHTTPTransport(server.URL), libfossil.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	c, err := libfossil.NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.SetStoreCone(c); err != nil {
		t.Fatal(err)
	}
	uuid := hash.SHA1([]byte("outside\n"))
	for _, stmt := range []string{
		"UPDATE blob SET size=-1,content=NULL WHERE uuid=?",
		"INSERT OR IGNORE INTO phantom SELECT rid FROM blob WHERE uuid=?",
	} {
		if _, err := target.Inner().DB().Exec(stmt, uuid); err != nil {
			t.Fatal(err)
		}
	}
	if !savedURL {
		if _, err := target.Inner().DB().Exec("DELETE FROM config WHERE name='last-sync-url'"); err != nil {
			t.Fatal(err)
		}
	} else if err := target.SetConfig("last-sync-url", server.URL); err != nil {
		t.Fatal(err)
	}
	requests.Store(0)
	return path, &requests
}

func coneStoreHoldsOutside(t *testing.T, path string) bool {
	t.Helper()
	r, err := libfossil.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, held, err := content.CheckAvailableByUUID(r.Inner().DB(), hash.SHA1([]byte("outside\n")))
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// `repo cone store --dense` fetches through the saved last-sync-url peer.
func TestRepoConeStoreFetchesFromSavedPeer(t *testing.T) {
	path, requests := coneStoreTarget(t, true)
	output := requiredHTTPStdout(t, func() error {
		return (&RepoConeStoreCmd{Dense: true}).Run(&Globals{Repo: path})
	})
	if requests.Load() == 0 || !strings.HasPrefix(output, "fetched ") {
		t.Fatalf("requests=%d stdout=%q", requests.Load(), output)
	}
	if !coneStoreHoldsOutside(t, path) {
		t.Fatal("widened store still elides zzz/b")
	}
	output = requiredHTTPStdout(t, func() error {
		return (&RepoConeStoreCmd{}).Run(&Globals{Repo: path})
	})
	if output != "dense\n" {
		t.Fatalf("store cone after widening = %q", output)
	}
}

func TestRepoConeStoreRefusesWithoutPeer(t *testing.T) {
	path, requests := coneStoreTarget(t, false)
	err := (&RepoConeStoreCmd{Dense: true}).Run(&Globals{Repo: path})
	if err == nil || !strings.Contains(err.Error(), "1 elided artifact(s); no configured peer") {
		t.Fatalf("error = %v, want the no-peer refusal", err)
	}
	if requests.Load() != 0 || coneStoreHoldsOutside(t, path) {
		t.Fatalf("refused widening made %d request(s) or fetched content", requests.Load())
	}
	output := requiredHTTPStdout(t, func() error {
		return (&RepoConeStoreCmd{}).Run(&Globals{Repo: path})
	})
	if output != "src\n" {
		t.Fatalf("store cone after refusal = %q", output)
	}
}
