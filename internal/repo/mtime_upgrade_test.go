package repo

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
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

// The conversion is best-effort: while another connection holds the write
// lock, Open still succeeds and leaves the rows for a later open.
func TestOpenSkipsJulianConversionWhenLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.fossil")
	r, err := Create(path, "admin", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB().Exec("INSERT OR REPLACE INTO config(name, value, mtime) VALUES('old', 'x', 2461332.5)"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	holder, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	locked, err := Open(path)
	if err != nil {
		t.Fatalf("Open while another connection holds the write lock: %v", err)
	}
	var mtime float64
	if err := locked.DB().QueryRow("SELECT CAST(mtime AS REAL) FROM config WHERE name='old'").Scan(&mtime); err != nil {
		t.Fatal(err)
	}
	if mtime != 2461332.5 {
		t.Errorf("mtime = %v while locked, want it left for a later open", mtime)
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	r, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var secs int64
	if err := r.DB().QueryRow("SELECT CAST(mtime AS INTEGER) FROM config WHERE name='old'").Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs != 1792368000 {
		t.Errorf("mtime after the lock is released = %d, want 1792368000", secs)
	}
}
