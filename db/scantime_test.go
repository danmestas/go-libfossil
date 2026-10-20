package db_test

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestScanTimeMatchesSQLite uses SQLite as the oracle: for both forms fossil
// stores a millisecond in (julianday() and its 16-digit text), and for values
// just either side of the half-millisecond, ScanTime of a Julian day prints
// the same millisecond as strftime('%f') of it.
func TestScanTimeMatchesSQLite(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/oracle.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	const iJDEpoch = 210866760000000
	base := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC).UnixMilli()
	var jds []float64
	for i := int64(0); i < 2000; i++ {
		ms := base + i*37
		jds = append(jds,
			fsltype.JulianDayFromMillis(ms),
			fsltype.TextJulianDayFromMillis(ms),
			(float64(ms+iJDEpoch)+0.49)/86400000.0,
			(float64(ms+iJDEpoch)+0.51)/86400000.0,
		)
	}
	var mismatched int
	for _, jd := range jds {
		var want string
		if err := d.QueryRow("SELECT strftime('%Y-%m-%dT%H:%M:%f', ?)", jd).Scan(&want); err != nil {
			t.Fatal(err)
		}
		got, ok := db.ScanTime(jd)
		if !ok {
			t.Fatalf("ScanTime(%.17g) ok=false", jd)
		}
		if g := got.UTC().Format("2006-01-02T15:04:05.000"); g != want {
			mismatched++
			if mismatched <= 5 {
				t.Errorf("ScanTime(%.17g) = %s, SQLite's strftime gives %s", jd, g, want)
			}
		}
	}
	if mismatched > 0 {
		t.Errorf("%d of %d Julian days convert differently from SQLite", mismatched, len(jds))
	}
}
