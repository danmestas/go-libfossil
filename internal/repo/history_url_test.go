package repo_test

import (
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
)

// draft-fossil-horizon-00 §6.2.5: the history URL rides pragma
// horizon-served as one token, so it holds no whitespace or control byte,
// and names an http or https interface.
func TestCheckHistoryURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://example.org/history", true},
		{"http://example.org:8080/h?c=1", true},
		{"HTTPS://example.org/", true},
		{"", false},
		{"ftp://example.org/", false},
		{"example.org/history", false},
		{"https://", false},
		{"https://example.org/a b", false},
		{"https://example.org/a\tb", false},
		{"https://example.org/\x7f", false},
		{"https://user:secret@example.org/", false},
		{"https://example.org/" + strings.Repeat("a", repo.MaxHistoryURLLen), false},
	} {
		if err := repo.CheckHistoryURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("CheckHistoryURL(%q) = %v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

// Setting the history URL refuses one an origin could not emit and leaves
// the stored value as it was; an empty value clears it.
func TestSetConfigHistoryURL(t *testing.T) {
	r := newRepo(t)
	const good = "https://example.org/history"
	if err := r.SetConfig(repo.SettingHorizonHistoryURL, good); err != nil {
		t.Fatalf("SetConfig(%q): %v", good, err)
	}
	for _, bad := range []string{"https://example.org/a b", "ftp://example.org/"} {
		err := r.SetConfig(repo.SettingHorizonHistoryURL, bad)
		if err == nil || !strings.Contains(err.Error(), repo.SettingHorizonHistoryURL) {
			t.Fatalf("SetConfig(%q) = %v, want an error naming the setting", bad, err)
		}
	}
	if got, err := r.Config(repo.SettingHorizonHistoryURL); err != nil || got != good {
		t.Fatalf("stored %q (%v), want %q", got, err, good)
	}
	if err := r.SetConfig(repo.SettingHorizonHistoryURL, ""); err != nil {
		t.Fatalf("clearing: %v", err)
	}
}
