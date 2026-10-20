package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// A real byte-protocol HTTP peer must satisfy required structure even when
// optional autosync is disabled; ordinary check-in stdout stays unchanged.
func TestRepoCiRequiredContentNoSync(t *testing.T) {
	source := requiredHTTPSource(t)
	var requests atomic.Int64
	server := requiredHTTPServer(t, source, &requests)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "target.fossil")
	target, _, err := libfossil.Clone(context.Background(), path,
		libfossil.NewHTTPTransport(server.URL), libfossil.CloneOpts{PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := target.ResolveVersion("tip")
	if err != nil {
		t.Fatal(err)
	}
	coverage := requiredHTTPElide(t, target, parent)
	dir := t.TempDir()
	co, err := target.CreateCheckout(dir, libfossil.CheckoutCreateOpts{Cone: coverage})
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(parent, libfossil.ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, coverage.Include()[0])
	if err := os.WriteFile(name, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := co.Close(); err != nil {
		t.Fatal(err)
	}
	if err := target.SetConfig("last-sync-url", server.URL); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	requests.Store(0)
	output := requiredHTTPStdout(t, func() error {
		return (&RepoCiCmd{NoSync: true, Message: "required recovery", User: "test", Dir: dir}).Run(
			&Globals{Repo: path})
	})
	if requests.Load() == 0 {
		t.Fatal("--no-sync suppressed required recovery")
	}
	if !strings.HasPrefix(output, "checkin ") || strings.Count(output, "\n") != 1 {
		t.Fatalf("requests=%d, stdout=%q", requests.Load(), output)
	}
}

func requiredHTTPServer(
	t *testing.T, source *libfossil.Repo, requests *atomic.Int64,
) *httptest.Server {
	t.Helper()
	handler := source.XferHandler()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
}

func requiredHTTPStdout(t *testing.T, run func() error) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	previous := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = previous }()
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func requiredHTTPSource(t *testing.T) *libfossil.Repo {
	t.Helper()
	r, err := libfossil.Create(filepath.Join(t.TempDir(), "source.fossil"),
		libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	for key, value := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
	} {
		if err := r.SetConfig(key, value); err != nil {
			t.Fatal(err)
		}
	}
	var files []libfossil.FileToCommit
	for _, name := range []string{"f0000", "f0003", "f0004", "f0005", "f0006", "f0008",
		"f0010", "f0014", "f0015", "f0016", "f0017", "f0018", "f0019", "f0021",
		"f0022", "f0023", "f0024"} {
		files = append(files, libfossil.FileToCommit{Name: name, Content: []byte(name)})
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{Files: files, User: "test"}); err != nil {
		t.Fatal(err)
	}
	return r
}

func requiredHTTPElide(t *testing.T, r *libfossil.Repo, parent int64) *libfossil.Cone {
	t.Helper()
	d, err := manifest.GetManifest(r.Inner(), fsltype.FslID(parent))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.S) < 2 {
		t.Fatal("fixture requires actual sibling structural nodes")
	}
	rid, held := content.AvailableByUUID(r.Inner().DB(), d.S[0].UUID)
	if !held {
		t.Fatal("fixture first subtree unavailable")
	}
	data, err := content.Expand(r.Inner().DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := deck.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range leaf.F {
		names = append(names, file.Name)
	}
	coverage, err := libfossil.NewCone(names, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetStoreCone(coverage); err != nil {
		t.Fatal(err)
	}
	for _, sub := range d.S[1:] {
		if _, err := r.Inner().DB().Exec(
			"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", sub.UUID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Inner().DB().Exec(
			"INSERT OR IGNORE INTO phantom SELECT rid FROM blob WHERE uuid=?", sub.UUID); err != nil {
			t.Fatal(err)
		}
	}
	return coverage
}
