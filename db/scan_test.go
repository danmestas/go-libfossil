package db

import (
	"testing"
	"time"
)

// sqliteJulianDay reproduces the expression SQLite's own julianday() evaluates:
// it stores an instant as integer milliseconds since julian day 0 (iJD) and
// returns iJD/86400000.0. That is a different expression from the one
// TimeToJulian writes with (julianEpoch + ms/86400000.0), and the two land one
// ulp apart on a real fraction of instants — which is the whole reason the
// float64 branch of ScanJulianDay must not "canonicalize" what it is handed.
//
// 210866760000000 is iJD for the Unix epoch (2440587.5 days * 86400000 ms).
func sqliteJulianDay(unixMillis int64) float64 {
	return float64(210866760000000+unixMillis) / millisPerDay
}

// TestScanJulianDayFloatBranchIsBitExact is the cursor-safety regression.
//
// Timeline's pagination cursor carries the mtime float64 exactly as read off
// the row, and its predicate `e.mtime < ? OR (e.mtime = ? AND b.rid < ?)`
// depends on that value comparing exactly equal to the row it came from. If
// ScanJulianDay perturbs a scanned float64 by even one ulp, the boundary breaks
// silently and in both directions: raise it and the last row of a page repeats
// as the first row of the next; lower it and rows are skipped entirely.
//
// The values that expose this are not exotic — they are every mtime written by
// upstream Fossil via SQLite's julianday(), i.e. the library's primary
// real-world input. This test sweeps such values, first asserting the case is
// genuinely divergent (so it can never pass vacuously) and then asserting
// ScanJulianDay returns each one verbatim.
func TestScanJulianDayFloatBranchIsBitExact(t *testing.T) {
	base := time.Date(2024, 3, 9, 14, 25, 17, 123_000_000, time.UTC).UnixMilli()

	var divergent int
	for i := 0; i < 5000; i++ {
		ms := base + int64(i)*1000

		stored := sqliteJulianDay(ms)
		if stored != julianDayFromMillis(ms) {
			// This instant is one where julianday() and TimeToJulian disagree
			// — exactly the input a millisecond-snapping normalization would
			// alter, and therefore the input that breaks the cursor.
			divergent++
		}

		got, ok := ScanJulianDay(stored)
		if !ok {
			t.Fatalf("ScanJulianDay(float64) returned ok=false for %.20f", stored)
		}
		if got != stored {
			t.Fatalf("ScanJulianDay altered a julianday()-computed mtime: got %.20f, want %.20f (bit-exact). "+
				"The Timeline cursor compares this value against the row it came from; any change repeats or skips rows.",
				got, stored)
		}
	}

	// Guard the guard: if julianday() and TimeToJulian ever agreed on every
	// instant, the sweep above would prove nothing.
	if divergent == 0 {
		t.Fatal("no divergent julianday() values in the sweep — the test would pass vacuously; pick a different range")
	}
	t.Logf("%d of 5000 swept julianday() mtimes differ from the millisecond grid (these are the cursor-breaking inputs)", divergent)
}

// TestScanJulianDayIsDriverIndependent proves that a value this codebase wrote
// scans back to the identical julian day regardless of which representation the
// compiled-in driver hands back. modernc returns the column as a float64;
// ncruces returns it as a time.Time carrying that driver's own sub-millisecond
// REAL-conversion noise (~13us observed) around the same instant.
//
// Millisecond alignment is the precondition, not an incidental choice of
// fixture: TimeToJulian rounds every mtime this codebase writes to whole
// milliseconds, and that is what lets the time.Time branch recover the exact
// original despite the noise. The perturbation below is deliberately
// sub-millisecond — that is the only interval this bug class lives in, and it
// is why two prior truncation bugs survived suites that only compared
// timestamps hours apart.
//
// Note the asymmetry this test does NOT claim: for an mtime written at finer
// than millisecond resolution (upstream Fossil via julianday()), the time.Time
// branch cannot reproduce the stored value, because event.mtime is declared
// DATETIME and ncruces converts it through time.Time before this code sees it.
// That is a pre-existing driver limitation, not something the float64 branch
// can or should compensate for — see TestScanJulianDayFloatBranchIsBitExact.
func TestScanJulianDayIsDriverIndependent(t *testing.T) {
	instant := time.Date(2024, 3, 9, 14, 25, 17, 123_000_000, time.UTC)
	canonical := julianDayFromMillis(instant.UnixMilli())

	// modernc-style: the column arrives as the float64 that was written.
	fromFloat, ok := ScanJulianDay(canonical)
	if !ok {
		t.Fatal("ScanJulianDay(float64) returned ok=false")
	}

	// ncruces-style: the same instant arrives as a time.Time carrying the
	// driver's sub-millisecond conversion noise, in both directions.
	for _, noise := range []time.Duration{300 * time.Microsecond, -300 * time.Microsecond} {
		fromTime, ok := ScanJulianDay(instant.Add(noise))
		if !ok {
			t.Fatalf("ScanJulianDay(time.Time) returned ok=false (noise %v)", noise)
		}
		if fromTime != fromFloat {
			t.Fatalf("driver-dependent scan with %v noise: time.Time branch = %.20f, float64 branch = %.20f",
				noise, fromTime, fromFloat)
		}
		if fromTime != canonical {
			t.Fatalf("time.Time branch did not recover the written millisecond (noise %v): got %.20f, want %.20f",
				noise, fromTime, canonical)
		}
	}
}

// TestScanJulianDaySentinelZero asserts the "no mtime" sentinel survives
// unchanged, since bisect's `mtime != 0` guard depends on an exact zero passing
// through both the int64 (COALESCE(mtime, 0)) and float64 representations.
func TestScanJulianDaySentinelZero(t *testing.T) {
	for _, v := range []any{int64(0), float64(0)} {
		got, ok := ScanJulianDay(v)
		if !ok {
			t.Fatalf("ScanJulianDay(%T(0)) returned ok=false", v)
		}
		if got != 0 {
			t.Fatalf("ScanJulianDay(%T(0)) = %.20f, want exactly 0", v, got)
		}
	}
}
