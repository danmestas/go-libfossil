package fsltype_test

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestHashDigits checks the setting is clamped as fossil's hash_digits
// clamps it: 6 to HNAME_MAX (64) for display, display+6 floored at 16 and
// capped at 64 for links.
func TestHashDigits(t *testing.T) {
	d, err := db.OpenSQL(filepath.Join(t.TempDir(), "c.db"), db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Exec("CREATE TABLE config(name TEXT PRIMARY KEY, value)"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		setting    any
		human, url int
	}{{nil, 10, 16}, {3, 6, 16}, {12, 12, 18}, {50, 50, 56}, {60, 60, 64}, {99, 64, 64}}
	for _, c := range cases {
		if _, err := d.Exec("REPLACE INTO config VALUES('hash-digits', ?)", c.setting); err != nil {
			t.Fatal(err)
		}
		if human, url := fsltype.HashDigits(d); human != c.human || url != c.url {
			t.Errorf("hash-digits %v: got (%d, %d), want (%d, %d)", c.setting, human, url, c.human, c.url)
		}
	}
}
