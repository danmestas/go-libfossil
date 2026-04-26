package diff

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		want         []string
		wantHasNL    bool
	}{
		{"empty", "", nil, true},
		{"single no newline", "hello", []string{"hello"}, false},
		{"single with newline", "hello\n", []string{"hello"}, true},
		{"two lines", "a\nb\n", []string{"a", "b"}, true},
		{"no trailing newline", "a\nb", []string{"a", "b"}, false},
		{"crlf normalized", "a\r\nb\r\n", []string{"a", "b"}, true},
		{"mixed eol", "a\nb\r\nc\n", []string{"a", "b", "c"}, true},
		{"blank lines", "a\n\nb\n", []string{"a", "", "b"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotHasNL := splitLines([]byte(tt.input))
			if len(got) != len(tt.want) {
				t.Fatalf("splitLines(%q) = %v (len %d), want %v (len %d)",
					tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("line %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
			if gotHasNL != tt.wantHasNL {
				t.Errorf("endsWithNewline = %v, want %v", gotHasNL, tt.wantHasNL)
			}
		})
	}
}

func TestIsBinary(t *testing.T) {
	if isBinary([]byte("hello world")) {
		t.Fatal("text should not be binary")
	}
	if !isBinary([]byte("hello\x00world")) {
		t.Fatal("null byte should be binary")
	}
	if isBinary(nil) {
		t.Fatal("nil should not be binary")
	}
	if isBinary([]byte{}) {
		t.Fatal("empty should not be binary")
	}
}

func TestMyersIdentical(t *testing.T) {
	ops := myers([]string{"a", "b", "c"}, []string{"a", "b", "c"})
	for _, op := range ops {
		if op.kind != opEqual {
			t.Fatalf("identical inputs should produce only opEqual, got %v", op.kind)
		}
	}
	if len(ops) != 3 {
		t.Fatalf("got %d ops, want 3", len(ops))
	}
}

func TestMyersInsert(t *testing.T) {
	ops := myers([]string{"a", "c"}, []string{"a", "b", "c"})
	var inserts int
	for _, op := range ops {
		if op.kind == opInsert {
			inserts++
			if op.text != "b" {
				t.Fatalf("inserted text = %q, want %q", op.text, "b")
			}
		}
	}
	if inserts != 1 {
		t.Fatalf("got %d inserts, want 1", inserts)
	}
}

func TestMyersDelete(t *testing.T) {
	ops := myers([]string{"a", "b", "c"}, []string{"a", "c"})
	var deletes int
	for _, op := range ops {
		if op.kind == opDelete {
			deletes++
			if op.text != "b" {
				t.Fatalf("deleted text = %q, want %q", op.text, "b")
			}
		}
	}
	if deletes != 1 {
		t.Fatalf("got %d deletes, want 1", deletes)
	}
}

func TestMyersEmpty(t *testing.T) {
	ops := myers(nil, []string{"a", "b"})
	var inserts int
	for _, op := range ops {
		if op.kind == opInsert {
			inserts++
		}
	}
	if inserts != 2 {
		t.Fatalf("got %d inserts, want 2", inserts)
	}

	ops = myers([]string{"a", "b"}, nil)
	var deletes int
	for _, op := range ops {
		if op.kind == opDelete {
			deletes++
		}
	}
	if deletes != 2 {
		t.Fatalf("got %d deletes, want 2", deletes)
	}
}

func TestMyersMixed(t *testing.T) {
	src := []string{"a", "b", "c", "d", "e"}
	dst := []string{"a", "x", "c", "e", "f"}
	ops := myers(src, dst)

	// Verify we get a valid edit script: applying ops to src produces dst.
	var result []string
	for _, op := range ops {
		switch op.kind {
		case opEqual, opInsert:
			result = append(result, op.text)
		}
	}
	if len(result) != len(dst) {
		t.Fatalf("applying ops: got %v, want %v", result, dst)
	}
	for i := range result {
		if result[i] != dst[i] {
			t.Fatalf("line %d: got %q, want %q", i, result[i], dst[i])
		}
	}
}

func TestUnifiedIdentical(t *testing.T) {
	a := []byte("hello\nworld\n")
	got := Unified(a, a, Options{ContextLines: 3})
	if got != "" {
		t.Fatalf("identical inputs should return empty string, got:\n%s", got)
	}
}

func TestUnifiedSimpleChange(t *testing.T) {
	a := []byte("line1\nline2\nline3\n")
	b := []byte("line1\nchanged\nline3\n")
	got := Unified(a, b, Options{
		ContextLines: 3,
		SrcName:      "a/file.txt",
		DstName:      "b/file.txt",
	})
	if got == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(got, "--- a/file.txt") {
		t.Fatalf("missing src header in:\n%s", got)
	}
	if !strings.Contains(got, "+++ b/file.txt") {
		t.Fatalf("missing dst header in:\n%s", got)
	}
	if !strings.Contains(got, "-line2") {
		t.Fatalf("missing deletion in:\n%s", got)
	}
	if !strings.Contains(got, "+changed") {
		t.Fatalf("missing insertion in:\n%s", got)
	}
	if !strings.Contains(got, " line1") {
		t.Fatalf("missing context in:\n%s", got)
	}
	if !strings.Contains(got, "@@") {
		t.Fatalf("missing hunk header in:\n%s", got)
	}
}

func TestUnifiedInsertOnly(t *testing.T) {
	a := []byte("a\nc\n")
	b := []byte("a\nb\nc\n")
	got := Unified(a, b, Options{ContextLines: 3})
	if !strings.Contains(got, "+b") {
		t.Fatalf("expected +b in:\n%s", got)
	}
}

func TestUnifiedDeleteOnly(t *testing.T) {
	a := []byte("a\nb\nc\n")
	b := []byte("a\nc\n")
	got := Unified(a, b, Options{ContextLines: 3})
	if !strings.Contains(got, "-b") {
		t.Fatalf("expected -b in:\n%s", got)
	}
}

func TestUnifiedZeroContext(t *testing.T) {
	a := []byte("a\nb\nc\nd\ne\n")
	b := []byte("a\nB\nc\nd\nE\n")
	got := Unified(a, b, Options{ContextLines: 0})
	// Two changes separated by 2 unchanged lines -- with 0 context, must be 2 hunks.
	hunkCount := strings.Count(got, "@@ -")
	if !strings.HasPrefix(got, "---") {
		t.Fatalf("expected diff header, got:\n%s", got)
	}
	if hunkCount != 2 {
		t.Fatalf("expected 2 hunks with 0 context, got %d:\n%s", hunkCount, got)
	}
	// No context lines should appear (lines starting with a space).
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 0 && line[0] == ' ' {
			t.Fatalf("zero-context diff should not have context lines, found %q in:\n%s", line, got)
		}
	}
}

func TestUnifiedBinary(t *testing.T) {
	a := []byte("hello\x00world")
	b := []byte("different")
	got := Unified(a, b, Options{SrcName: "bin.dat", DstName: "bin.dat"})
	if !strings.Contains(got, "cannot compute difference between binary files") {
		t.Fatalf("binary input should report binary message, got:\n%s", got)
	}
	if !strings.Contains(got, "--- bin.dat") {
		t.Fatalf("binary diff should include headers, got:\n%s", got)
	}
}

func TestUnifiedBothEmpty(t *testing.T) {
	got := Unified(nil, nil, Options{})
	if got != "" {
		t.Fatalf("both nil should return empty, got: %q", got)
	}
}

func TestUnifiedOneEmpty(t *testing.T) {
	a := []byte("hello\n")
	got := Unified(nil, a, Options{SrcName: "old", DstName: "new"})
	if !strings.Contains(got, "+hello") {
		t.Fatalf("expected insertion in:\n%s", got)
	}

	got2 := Unified(a, nil, Options{SrcName: "old", DstName: "new"})
	if !strings.Contains(got2, "-hello") {
		t.Fatalf("expected deletion in:\n%s", got2)
	}
}

func TestStatSimple(t *testing.T) {
	a := []byte("a\nb\nc\n")
	b := []byte("a\nB\nc\nd\n")
	stat := Stat(a, b)
	// b changed -> 1 deletion + 1 insertion, d added -> 1 insertion
	if stat.Insertions < 1 {
		t.Fatalf("Insertions = %d, want >= 1", stat.Insertions)
	}
	if stat.Deletions < 1 {
		t.Fatalf("Deletions = %d, want >= 1", stat.Deletions)
	}
	if stat.Binary {
		t.Fatal("should not be binary")
	}
}

func TestStatBinary(t *testing.T) {
	stat := Stat([]byte("a\x00b"), []byte("c"))
	if !stat.Binary {
		t.Fatal("should detect binary")
	}
}

func TestStatIdentical(t *testing.T) {
	a := []byte("same\n")
	stat := Stat(a, a)
	if stat.Insertions != 0 || stat.Deletions != 0 {
		t.Fatalf("identical: got %+v", stat)
	}
}

func TestUnifiedLargeFile(t *testing.T) {
	// 10K lines, change every 100th line.
	var a, b strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&a, "line %d original\n", i)
		if i%100 == 50 {
			fmt.Fprintf(&b, "line %d CHANGED\n", i)
		} else {
			fmt.Fprintf(&b, "line %d original\n", i)
		}
	}

	got := Unified([]byte(a.String()), []byte(b.String()), Options{ContextLines: 3})
	if got == "" {
		t.Fatal("expected non-empty diff for large file")
	}

	stat := Stat([]byte(a.String()), []byte(b.String()))
	// 100 lines changed = 100 deletions + 100 insertions
	if stat.Insertions != 100 {
		t.Fatalf("Insertions = %d, want 100", stat.Insertions)
	}
	if stat.Deletions != 100 {
		t.Fatalf("Deletions = %d, want 100", stat.Deletions)
	}
}

func TestUnifiedWhitespaceOnly(t *testing.T) {
	a := []byte("  hello  \n  world  \n")
	b := []byte("  hello\n  world  \n")
	got := Unified(a, b, Options{ContextLines: 3})
	if got == "" {
		t.Fatal("whitespace change should produce diff")
	}
}

func TestUnifiedEmptyLinesOnly(t *testing.T) {
	a := []byte("\n\n\n")
	b := []byte("\n\n\n\n")
	got := Unified(a, b, Options{ContextLines: 3})
	if got == "" {
		t.Fatal("added empty line should produce diff")
	}
}

func TestUnifiedSingleChar(t *testing.T) {
	a := []byte("x")
	b := []byte("y")
	got := Unified(a, b, Options{ContextLines: 3})
	if !strings.Contains(got, "-x") || !strings.Contains(got, "+y") {
		t.Fatalf("single char diff:\n%s", got)
	}
}

func TestUnifiedLongLine(t *testing.T) {
	long := strings.Repeat("x", 10000)
	a := []byte(long + "\n")
	b := []byte(long + "y\n")
	got := Unified(a, b, Options{ContextLines: 3})
	if got == "" {
		t.Fatal("long line change should produce diff")
	}
}

func TestUnifiedUnicode(t *testing.T) {
	a := []byte("hello 世界\n")
	b := []byte("hello 🌍\n")
	got := Unified(a, b, Options{ContextLines: 3})
	if got == "" {
		t.Fatal("unicode change should produce diff")
	}
	if !strings.Contains(got, "-hello 世界") {
		t.Fatalf("missing unicode deletion:\n%s", got)
	}
}

func TestUnifiedNoTrailingNewlineBoth(t *testing.T) {
	a := []byte("line1\nline2")
	b := []byte("line1\nchanged")
	got := Unified(a, b, Options{ContextLines: 3})
	if got == "" {
		t.Fatal("expected diff for no-trailing-newline")
	}
}

// TestNoNewlineMarker_WithToWithout: file ends with newline, new version does not.
// Marker should appear on the + side only.
func TestNoNewlineMarker_WithToWithout(t *testing.T) {
	a := []byte("line1\nline2\n")   // ends with newline
	b := []byte("line1\nchanged")   // no trailing newline
	got := Unified(a, b, Options{ContextLines: 3, SrcName: "a/f", DstName: "b/f"})
	const marker = "\\ No newline at end of file"
	if !strings.Contains(got, marker) {
		t.Fatalf("expected no-newline marker on + side, got:\n%s", got)
	}
	// Marker must follow the last + line, not the - line.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	for i, line := range lines {
		if line == marker {
			if i == 0 {
				t.Fatalf("marker at line 0, expected after a +/- line")
			}
			prev := lines[i-1]
			if !strings.HasPrefix(prev, "+") {
				t.Fatalf("marker must follow a + line; instead follows %q", prev)
			}
		}
	}
	// No marker on the - side (src ended with newline).
	markerCount := strings.Count(got, marker)
	if markerCount != 1 {
		t.Fatalf("expected exactly 1 marker, got %d:\n%s", markerCount, got)
	}
}

// TestNoNewlineMarker_WithoutToWith: old file has no trailing newline, new one does.
// Marker should appear on the - side only.
func TestNoNewlineMarker_WithoutToWith(t *testing.T) {
	a := []byte("line1\nline2")     // no trailing newline
	b := []byte("line1\nchanged\n") // ends with newline
	got := Unified(a, b, Options{ContextLines: 3, SrcName: "a/f", DstName: "b/f"})
	const marker = "\\ No newline at end of file"
	if !strings.Contains(got, marker) {
		t.Fatalf("expected no-newline marker on - side, got:\n%s", got)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	for i, line := range lines {
		if line == marker {
			if i == 0 {
				t.Fatalf("marker at line 0, expected after a +/- line")
			}
			prev := lines[i-1]
			if !strings.HasPrefix(prev, "-") {
				t.Fatalf("marker must follow a - line; instead follows %q", prev)
			}
		}
	}
	markerCount := strings.Count(got, marker)
	if markerCount != 1 {
		t.Fatalf("expected exactly 1 marker, got %d:\n%s", markerCount, got)
	}
}

// TestNoNewlineMarker_BothWithout: both sides lack trailing newline.
// Marker should appear on both the - and + sides.
func TestNoNewlineMarker_BothWithout(t *testing.T) {
	a := []byte("line1\nline2")   // no trailing newline
	b := []byte("line1\nchanged") // no trailing newline
	got := Unified(a, b, Options{ContextLines: 3, SrcName: "a/f", DstName: "b/f"})
	const marker = "\\ No newline at end of file"
	markerCount := strings.Count(got, marker)
	if markerCount != 2 {
		t.Fatalf("expected 2 markers (one per side), got %d:\n%s", markerCount, got)
	}
	// First marker follows a - line, second follows a + line.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	markerFollows := []string{}
	for i, line := range lines {
		if line == marker && i > 0 {
			markerFollows = append(markerFollows, lines[i-1])
		}
	}
	if len(markerFollows) != 2 {
		t.Fatalf("could not find preceding lines for both markers in:\n%s", got)
	}
	if !strings.HasPrefix(markerFollows[0], "-") {
		t.Fatalf("first marker should follow a - line, follows %q", markerFollows[0])
	}
	if !strings.HasPrefix(markerFollows[1], "+") {
		t.Fatalf("second marker should follow a + line, follows %q", markerFollows[1])
	}
}

// TestNoNewlineMarker_IdenticalNoNewline: identical files with no trailing newline
// produce no diff (short-circuit still works).
func TestNoNewlineMarker_IdenticalNoNewline(t *testing.T) {
	a := []byte("hello\nworld")
	got := Unified(a, a, Options{ContextLines: 3})
	if got != "" {
		t.Fatalf("identical no-newline inputs should return empty string, got:\n%s", got)
	}
}

// TestNoNewlineMarker_Absent: normal files with trailing newlines must not have the marker.
func TestNoNewlineMarker_Absent(t *testing.T) {
	a := []byte("line1\nline2\n")
	b := []byte("line1\nchanged\n")
	got := Unified(a, b, Options{ContextLines: 3})
	const marker = "\\ No newline at end of file"
	if strings.Contains(got, marker) {
		t.Fatalf("normal diff must not contain no-newline marker, got:\n%s", got)
	}
}

func TestUnifiedAllDifferent(t *testing.T) {
	// Worst case for Myers: every line differs.
	var a, b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&a, "old line %d\n", i)
		fmt.Fprintf(&b, "new line %d\n", i)
	}
	got := Unified([]byte(a.String()), []byte(b.String()), Options{ContextLines: 3})
	if got == "" {
		t.Fatal("all-different should produce diff")
	}
	stat := Stat([]byte(a.String()), []byte(b.String()))
	if stat.Insertions != 100 || stat.Deletions != 100 {
		t.Fatalf("all-different stat: %+v", stat)
	}
}

func TestUnifiedNegativeContextPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for negative ContextLines")
		}
	}()
	Unified([]byte("a\n"), []byte("b\n"), Options{ContextLines: -1})
}

func BenchmarkMyers(b *testing.B) {
	var src, dst []string
	for i := 0; i < 1000; i++ {
		src = append(src, fmt.Sprintf("line %d", i))
		if i%10 == 5 {
			dst = append(dst, fmt.Sprintf("line %d changed", i))
		} else {
			dst = append(dst, fmt.Sprintf("line %d", i))
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		myers(src, dst)
	}
}
