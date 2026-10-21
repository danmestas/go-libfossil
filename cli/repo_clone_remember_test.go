package cli

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// repo clone remembers its source as last-sync-url, as fossil clone does, so
// widening and required-content recovery find their peer without --remote.
// Method: clone from an HTTP peer through the command, then read the setting.
func TestRepoCloneRemembersSyncURL(t *testing.T) {
	source := requiredHTTPSource(t)
	var requests atomic.Int64
	server := requiredHTTPServer(t, source, &requests)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "clone.fossil")
	requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: server.URL, Path: path}).Run(&Globals{})
	})
	r, err := libfossil.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := r.Config("last-sync-url"); err != nil || got != server.URL {
		t.Fatalf("last-sync-url = %q, %v; want %q", got, err, server.URL)
	}
}

// A password in the clone URL is never stored; the user name is.
func TestRememberedSyncURLDropsPassword(t *testing.T) {
	for raw, want := range map[string]string{
		"https://u:secret@example.org/repo": "https://u@example.org/repo",
		"https://u@example.org/repo":        "https://u@example.org/repo",
		"http://127.0.0.1:8080":             "http://127.0.0.1:8080",
	} {
		if got := rememberedSyncURL(raw); got != want {
			t.Errorf("rememberedSyncURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
