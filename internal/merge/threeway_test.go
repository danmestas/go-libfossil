package merge

import (
	"strings"
	"testing"
)

func TestThreeWayIdentical(t *testing.T) {
	base := []byte("same\n")
	r, err := (&ThreeWayText{}).Merge(base, base, base)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean {
		t.Fatal("expected clean merge")
	}
	if string(r.Content) != "same\n" {
		t.Fatalf("got %q", r.Content)
	}
}

func TestThreeWayOnlyLocalChanged(t *testing.T) {
	base := []byte("line1\nline2\nline3\n")
	local := []byte("line1\nMODIFIED\nline3\n")
	remote := []byte("line1\nline2\nline3\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean {
		t.Fatal("expected clean merge")
	}
	if string(r.Content) != "line1\nMODIFIED\nline3\n" {
		t.Fatalf("got %q", r.Content)
	}
}

func TestThreeWayOnlyRemoteChanged(t *testing.T) {
	base := []byte("line1\nline2\nline3\n")
	local := []byte("line1\nline2\nline3\n")
	remote := []byte("line1\nline2\nREMOTE\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean {
		t.Fatal("expected clean merge")
	}
	if string(r.Content) != "line1\nline2\nREMOTE\n" {
		t.Fatalf("got %q", r.Content)
	}
}

func TestThreeWayBothChangedSame(t *testing.T) {
	base := []byte("old\n")
	local := []byte("new\n")
	remote := []byte("new\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean {
		t.Fatal("expected clean merge (both made same change)")
	}
}

func TestThreeWayNonOverlapping(t *testing.T) {
	base := []byte("aaa\nbbb\nccc\n")
	local := []byte("AAA\nbbb\nccc\n")
	remote := []byte("aaa\nbbb\nCCC\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean {
		t.Fatalf("expected clean merge, got %d conflicts", len(r.Conflicts))
	}
	if string(r.Content) != "AAA\nbbb\nCCC\n" {
		t.Fatalf("got %q", r.Content)
	}
}

func TestThreeWayConflict(t *testing.T) {
	base := []byte("original\n")
	local := []byte("local version\n")
	remote := []byte("remote version\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if r.Clean {
		t.Fatal("expected conflict")
	}
	if len(r.Conflicts) == 0 {
		t.Fatal("expected at least one conflict")
	}
	if !HasConflictMarkers(r.Content) {
		t.Fatalf("expected conflict markers, got %q", r.Content)
	}
	if !strings.Contains(string(r.Content), "local version") {
		t.Fatalf("expected local content in markers")
	}
	if !strings.Contains(string(r.Content), "remote version") {
		t.Fatalf("expected remote content in markers")
	}
}

func TestThreeWayLocalAddsLines(t *testing.T) {
	base := []byte("a\nc\n")
	local := []byte("a\nb\nc\n")
	remote := []byte("a\nc\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean {
		t.Fatalf("expected clean merge, got %d conflicts", len(r.Conflicts))
	}
	if string(r.Content) != "a\nb\nc\n" {
		t.Fatalf("got %q", r.Content)
	}
}

func TestThreeWayRemoteAddsLines(t *testing.T) {
	base := []byte("a\nc\n")
	local := []byte("a\nc\n")
	remote := []byte("a\nb\nc\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean {
		t.Fatal("expected clean merge")
	}
	if string(r.Content) != "a\nb\nc\n" {
		t.Fatalf("got %q", r.Content)
	}
}

func TestThreeWayEmptyBase(t *testing.T) {
	base := []byte("")
	local := []byte("same line\n")
	remote := []byte("different line\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	// Both added different content to empty base — conflict.
	// (Both sides insert at position 0, overlapping hunk.)
	if r.Clean {
		// If the algorithm treats both as non-overlapping inserts, that's
		// also acceptable for empty base — just verify both are present.
		if !strings.Contains(string(r.Content), "same line") || !strings.Contains(string(r.Content), "different line") {
			t.Fatalf("expected both additions, got %q", r.Content)
		}
	}
}

func TestThreeWayBothAddDifferentRegions(t *testing.T) {
	base := []byte("line1\nline2\nline3\nline4\nline5\n")
	local := []byte("LOCAL\nline1\nline2\nline3\nline4\nline5\n")
	remote := []byte("line1\nline2\nline3\nline4\nline5\nREMOTE\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean {
		t.Fatalf("expected clean merge (additions in different regions), got %d conflicts", len(r.Conflicts))
	}
	if !strings.Contains(string(r.Content), "LOCAL") {
		t.Fatal("missing local addition")
	}
	if !strings.Contains(string(r.Content), "REMOTE") {
		t.Fatal("missing remote addition")
	}
}

// fossil 2.28's marker lines, spelled out so the tests pin them literally.
const (
	fsBegin     = "<<<<<<< BEGIN MERGE CONFLICT: local copy shown first <<<<<<<<<<<<"
	fsSuggested = "####### SUGGESTED CONFLICT RESOLUTION follows ###################"
	fsAncestor  = "||||||| COMMON ANCESTOR content follows |||||||||||||||||||||||||"
	fsMergedIn  = "======= MERGED IN content follows ==============================="
	fsEnd       = ">>>>>>> END MERGE CONFLICT >>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>"
)

// TestThreeWayConflictUsesFossilMarkers pins the conflict block to what
// fossil 2.28's merge3 writes for the same inputs (captured from
// "fossil 3-way-merge"): line-numbered markers, the suggested resolution, the
// ancestor and the merged-in content, with the file's line endings.
func TestThreeWayConflictUsesFossilMarkers(t *testing.T) {
	cases := []struct {
		name, base, local, remote, want string
	}{
		{
			name:   "lf",
			base:   "a\nbase line here\nz\n",
			local:  "a\nlocal line here\nz\n",
			remote: "a\nremote text there\nz\n",
			want: "a\n" +
				fsBegin + " (line 2)\n" + "local line here\n" +
				fsSuggested + "\n" + "remote text there\n" +
				fsAncestor + " (line 2)\n" + "base line here\n" +
				fsMergedIn + " (line 2)\n" + "remote text there\n" +
				fsEnd + "\n" + "z\n",
		},
		{
			name:   "crlf",
			base:   "a\r\nbase\r\nz\r\n",
			local:  "a\r\nlocal one\r\nz\r\n",
			remote: "a\r\nremote two\r\nz\r\n",
			want: "a\r\n" +
				fsBegin + " (line 2)\r\n" + "local one\r\n" +
				fsSuggested + "\r\n" + "remote two\r\n" +
				fsAncestor + " (line 2)\r\n" + "base\r\n" +
				fsMergedIn + " (line 2)\r\n" + "remote two\r\n" +
				fsEnd + "\r\n" + "z\r\n",
		},
		{
			name:   "no trailing newline",
			base:   "a\nb",
			local:  "a\nx y",
			remote: "a\nx z",
			want: "a\n" +
				fsBegin + " (line 2)\n" + "x y\n" +
				fsSuggested + "\n" + "x z\n" +
				fsAncestor + " (line 2)\n" + "b\n" +
				fsMergedIn + " (line 2)\n" + "x z\n" +
				fsEnd + "\n",
		},
		{ // Different words of one line: the token merge resolves it.
			name:   "token resolvable",
			base:   "one two three\n",
			local:  "ONE two three\n",
			remote: "one two THREE\n",
			want: fsBegin + " (line 1)\n" + "ONE two three\n" +
				fsSuggested + "\n" + "ONE two THREE\n" +
				fsAncestor + " (line 1)\n" + "one two three\n" +
				fsMergedIn + " (line 1)\n" + "one two THREE\n" +
				fsEnd + "\n",
		},
	}
	for _, tc := range cases {
		r, err := (&ThreeWayText{}).Merge([]byte(tc.base), []byte(tc.local), []byte(tc.remote))
		if err != nil {
			t.Fatal(err)
		}
		if r.Clean || len(r.Conflicts) != 1 {
			t.Errorf("%s: Clean=%v conflicts=%d, want one conflict", tc.name, r.Clean, len(r.Conflicts))
		}
		if string(r.Content) != tc.want {
			t.Errorf("%s: conflict block:\n%q\nwant:\n%q", tc.name, r.Content, tc.want)
		}
	}
}

// TestThreeWayBinaryIsNotMerged mirrors fossil: content its diff cannot read
// as text (a NUL byte, or a line longer than 32767 bytes) is not merged, so
// the local copy is kept and the merge is not clean.
func TestThreeWayBinaryIsNotMerged(t *testing.T) {
	long := strings.Repeat("x", 32768) + "\n"
	cases := []struct{ name, base, local, remote string }{
		{"nul in local", "a\n", "a\x00b\n", "c\n"},
		{"nul in base", "a\x00\n", "b\n", "c\n"},
		{"long line in remote", "a\n", "b\n", long},
	}
	for _, tc := range cases {
		r, err := (&ThreeWayText{}).Merge([]byte(tc.base), []byte(tc.local), []byte(tc.remote))
		if err != nil {
			t.Fatal(err)
		}
		if r.Clean {
			t.Errorf("%s: binary merge reported clean", tc.name)
		}
		if string(r.Content) != tc.local {
			t.Errorf("%s: content = %q, want the local copy", tc.name, r.Content)
		}
		if HasConflictMarkers(r.Content) {
			t.Errorf("%s: binary merge wrote conflict markers", tc.name)
		}
	}
}

// TestHasConflictMarkers checks detection the way fossil's
// contains_merge_marker does it: a marker must start a line, and the closing
// marker alone does not count.
func TestHasConflictMarkers(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"empty", "", false},
		{"plain text", "a\nb\n", false},
		{"begin at start", markerBegin + "\n", true},
		{"ancestor mid-file", "x\n" + markerAncestor + "\ny\n", true},
		{"merged-in without newline", "x\n" + markerMergedIn, true},
		{"suggested section", markerSuggested + "\n", true},
		{"end marker alone", markerEnd + "\n", false},
		{"not at line start", " " + markerBegin + "\n", false},
		{"git-style marker", "<<<<<<< HEAD\n", false},
		{"crlf lines", "x\r\n" + markerBegin + "\r\n", true},
		{"after blank crlf lines", "x\r\n\r\r\n" + markerAncestor + "\r\n", true},
		{"after a lone cr", "x\n\r" + markerBegin + "\n", true},
		{"marker with line number", "x\n" + markerMergedIn + " (line 7)\n", true},
	}
	for _, tc := range cases {
		if got := HasConflictMarkers([]byte(tc.data)); got != tc.want {
			t.Errorf("%s: HasConflictMarkers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
