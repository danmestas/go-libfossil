package repo_test

import (
	"runtime"
	"testing"
)

// TestFilenamesCaseSensitive: the repository's case-sensitive setting wins,
// and without one fossil's platform default applies: off on macOS and
// Windows, on elsewhere.
func TestFilenamesCaseSensitive(t *testing.T) {
	r := newRepo(t)
	if got, want := r.FilenamesCaseSensitive(), runtime.GOOS != "darwin" && runtime.GOOS != "windows"; got != want {
		t.Errorf("no setting on %s: case-sensitive = %t, want the platform default %t", runtime.GOOS, got, want)
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{{"on", true}, {"OFF", false}, {"yes", true}, {"0", false}} {
		if err := r.SetConfig("case-sensitive", tc.value); err != nil {
			t.Fatal(err)
		}
		if got := r.FilenamesCaseSensitive(); got != tc.want {
			t.Errorf("case-sensitive=%q: %t, want %t", tc.value, got, tc.want)
		}
	}
}
