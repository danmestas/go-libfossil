package repo

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

// Before config.mtime and user.mtime were written in seconds since 1970,
// go-libfossil wrote a Julian day there, which fossil reads as a moment in
// January 1970. Open converts such rows to the seconds they stand for,
// leaves every other row alone, and converts nothing on a second open.
func TestOpenConvertsJulianConfigAndUserTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.fossil")
	r, err := Create(path, "admin", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 2461332.5 is 2026-10-19T00:00:00Z, 1792368000 seconds; .25 more is
	// 6 hours.
	for _, stmt := range []string{
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES('old', 'x', 2461332.5)",
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES('new', 'y', 1792368000)",
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES('none', 'z', NULL)",
		"INSERT INTO user(login, pw, cap, mtime) VALUES('olduser', '', '', 2461332.75)",
		"INSERT INTO user(login, pw, cap, mtime) VALUES('newuser', '', '', 1792368000)",
	} {
		if _, err := r.DB().Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"config old": "integer 1792368000", "config new": "integer 1792368000",
		"config none": "null ", "user olduser": "integer 1792389600",
		"user newuser": "integer 1792368000",
	}
	for pass := 1; pass <= 2; pass++ {
		r, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := r.DB().Query(`
			SELECT 'config '||name, typeof(mtime)||' '||coalesce(mtime, '') FROM config
			 WHERE name IN ('old','new','none')
			UNION ALL
			SELECT 'user '||login, typeof(mtime)||' '||coalesce(mtime, '') FROM user
			 WHERE login IN ('olduser','newuser')`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var key, got string
			if err := rows.Scan(&key, &got); err != nil {
				t.Fatal(err)
			}
			if got != want[key] {
				t.Errorf("open %d: %s mtime = %q, want %q", pass, key, got, want[key])
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
