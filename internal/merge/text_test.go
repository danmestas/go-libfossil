package merge

import (
	"bytes"
	"testing"
)

func TestIsText(t *testing.T) {
	long := bytes.Repeat([]byte("a"), lineLimit)
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"empty", []byte{}, true},
		{"lines", []byte("a\nb\n"), true},
		{"no final newline", []byte("a\nb"), true},
		{"NUL", []byte("a\x00b\n"), false},
		{"longest line", append(append([]byte{}, long...), '\n'), true},
		{"line too long", append(append([]byte{}, long...), 'a', '\n'), false},
		{"last line too long", append(append([]byte("x\n"), long...), 'a'), false},
	}
	for _, tc := range cases {
		if got := IsText(tc.data); got != tc.want {
			t.Errorf("%s: IsText = %v, want %v", tc.name, got, tc.want)
		}
	}
}
