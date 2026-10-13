package sync

import "testing"

func TestLinkURLIsHTTP(t *testing.T) {
	for url, want := range map[string]bool{
		"http://example.com":            true,
		"https://example.com/r":         true,
		"https://example.com:8080/r":    true,
		"https://example.com:":          true, // fossil reads an empty port as 0
		"https://u:p@example.com:443/r": true,
		"https://[::1]:8080/r":          true,
		"https://[::1]/r":               true,
		"https://":                      true, // an empty host still parses
		"https://example.com:8x/r":      false,
		"https://u@example.com:x":       false,
		"HTTP://example.com/r":          false,
		"ssh://example.com/r":           false,
		"file:/r":                       false,
		"example.com/r":                 false,
		"":                              false,
	} {
		if got := linkURLIsHTTP(url); got != want {
			t.Errorf("linkURLIsHTTP(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestLinkMTime(t *testing.T) {
	for field, want := range map[string]int64{
		"1767225600": 1767225600, "1": 1, "007": 7,
		"0": 0, "-5": 0, "+5": 0, "0x10": 0, "12z": 0, "": 0, " 5": 0,
		"99999999999999999999": 0,
	} {
		got, ok := linkMTime(field)
		if ok != (want > 0) || (ok && got != want) {
			t.Errorf("linkMTime(%q) = %d, %v; want %d", field, got, ok, want)
		}
	}
}

// TestSendsReqLinks checks the cases fossil's client_sync leaves out: a
// push-only session, a later round, and another project's code.
func TestSendsReqLinks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  SyncOpts
		cycle int
		want  bool
	}{
		{"off", SyncOpts{Pull: true}, 0, false},
		{"pull", SyncOpts{Pull: true, ShareLinks: true}, 0, true},
		{"later round", SyncOpts{Pull: true, ShareLinks: true}, 1, false},
		{"push only", SyncOpts{Push: true, ShareLinks: true}, 0, false},
		{"own code", SyncOpts{Pull: true, ShareLinks: true, ProjectCode: testPC}, 0, true},
		{"other code", SyncOpts{Pull: true, ShareLinks: true, ProjectCode: "abc"}, 0, false},
	} {
		s, _ := newTestSession(t, tc.opts)
		if got := s.sendsReqLinks(tc.cycle); got != tc.want {
			t.Errorf("%s: sendsReqLinks(%d) = %v, want %v", tc.name, tc.cycle, got, tc.want)
		}
	}
}
