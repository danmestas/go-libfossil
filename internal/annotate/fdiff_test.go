package annotate

import (
	"reflect"
	"strings"
	"testing"
)

func lines(t *testing.T, s string) []dline {
	t.Helper()
	d, ok := breakIntoLines([]byte(s))
	if !ok {
		t.Fatalf("breakIntoLines(%q) refused", s)
	}
	return d
}

// breakIntoLines splits as fossil's break_into_lines does: a final newline
// ends the last line, a trailing CR is dropped, NUL and over-long lines are
// refused.
func TestBreakIntoLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\n\n", []string{"a", ""}},
		{"\n", []string{""}},
		{"a\r\nb", []string{"a", "b"}},
	}
	for _, c := range cases {
		var got []string
		for _, d := range lines(t, c.in) {
			got = append(got, d.text)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("breakIntoLines(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, ok := breakIntoLines([]byte("a\x00")); ok {
		t.Error("breakIntoLines accepted a NUL byte")
	}
	if _, ok := breakIntoLines([]byte(strings.Repeat("x", lengthMask+1))); ok {
		t.Error("breakIntoLines accepted a line longer than lengthMask")
	}
}

// diffAll's edit script copies common lines and deletes and inserts the
// rest, ending in 0, 0, 0.
func TestDiffAll(t *testing.T) {
	from := lines(t, "a\nb\nc\nd\n")
	to := lines(t, "a\nx\nc\nd\ny\n")
	got := diffAll(from, to)
	want := []int{1, 1, 1, 2, 0, 1, 0, 0, 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diffAll = %v, want %v", got, want)
	}
}

// appendTriple folds a triple into the previous one where fossil does.
func TestAppendTripleFolds(t *testing.T) {
	p := &diffContext{}
	p.appendTriple(2, 0, 0)
	p.appendTriple(1, 0, 0) // follows a pure copy: folded
	p.appendTriple(0, 1, 0)
	p.appendTriple(0, 0, 3) // only inserts: added to the last triple
	want := []int{3, 1, 3}
	if !reflect.DeepEqual(p.edit, want) {
		t.Fatalf("edit = %v, want %v", p.edit, want)
	}
}
