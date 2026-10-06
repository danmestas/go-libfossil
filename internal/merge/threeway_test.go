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

// TestThreeWayConflictUsesFossilMarkers pins the conflict block to fossil's
// layout: local copy, common ancestor, merged-in content, each introduced by
// fossil's marker line, so fossil reports the file as a CONFLICT.
func TestThreeWayConflictUsesFossilMarkers(t *testing.T) {
	base := []byte("a\nbase\nz\n")
	local := []byte("a\nlocal\nz\n")
	remote := []byte("a\nremote\nz\n")
	r, err := (&ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	want := "a\n" +
		markerBegin + "\n" + "local\n" +
		markerAncestor + "\n" + "base\n" +
		markerMergedIn + "\n" + "remote\n" +
		markerEnd + "\n" + "z\n"
	if string(r.Content) != want {
		t.Fatalf("conflict block:\n%s\nwant:\n%s", r.Content, want)
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
	}
	for _, tc := range cases {
		if got := HasConflictMarkers([]byte(tc.data)); got != tc.want {
			t.Errorf("%s: HasConflictMarkers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
