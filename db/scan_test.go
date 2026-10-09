package db

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// TestScanJulianDayFloatBranchIsBitExact is the cursor-safety regression.
//
// Timeline's pagination cursor carries the mtime float64 exactly as read off
// the row, and its predicate `e.mtime < ? OR (e.mtime = ? AND b.rid < ?)`
// depends on that value comparing exactly equal to the row it came from. If
// ScanJulianDay perturbs a scanned float64 by even one ulp, the boundary breaks
// silently and in both directions: raise it and the last row of a page repeats
// as the first row of the next; lower it and rows are skipped entirely.
//
// Two forms of the same millisecond are stored (#258): SQLite's julianday()
// in tagxref, and fossil's 16-digit text form in event and plink. This test
// sweeps instants where the two differ — first asserting there are some, so
// it can never pass vacuously — and asserts both scanners return either form
// verbatim.
func TestScanJulianDayFloatBranchIsBitExact(t *testing.T) {
	base := time.Date(2024, 3, 9, 14, 25, 17, 123_000_000, time.UTC).UnixMilli()

	var divergent int
	for i := 0; i < 5000; i++ {
		ms := base + int64(i)*1000
		exact, text := julianDayFromMillis(ms), fsltype.TextJulianDayFromMillis(ms)
		if exact != text {
			divergent++
		}
		for _, stored := range []float64{exact, text} {
			for name, scan := range map[string]func(any) (float64, bool){
				"ScanJulianDay": ScanJulianDay, "ScanTextJulianDay": ScanTextJulianDay,
			} {
				got, ok := scan(stored)
				if !ok {
					t.Fatalf("%s(float64) returned ok=false for %.20f", name, stored)
				}
				if got != stored {
					t.Fatalf("%s altered a stored mtime: got %.20f, want %.20f (bit-exact). "+
						"The Timeline cursor compares this value against the row it came from.",
						name, got, stored)
				}
			}
		}
	}

	// Guard the guard: if the two forms agreed on every instant, the sweep
	// above would prove nothing about values off one grid or the other.
	if divergent == 0 {
		t.Fatal("no instant where the exact and text forms differ — the test would pass vacuously")
	}
	t.Logf("%d of 5000 swept instants store differently as julianday() and as text", divergent)
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

// ScanTextJulianDay rebuilds the text form fossil stores in event and plink
// from the time.Time the ncruces driver returns for those columns, through
// the same sub-millisecond noise ScanJulianDay tolerates.
func TestScanTextJulianDayFromDriverTime(t *testing.T) {
	instant := time.Date(2020, 12, 1, 13, 59, 16, 77_000_000, time.UTC)
	want := fsltype.TextJulianDayFromMillis(instant.UnixMilli())
	if want == julianDayFromMillis(instant.UnixMilli()) {
		t.Fatal("test instant does not tell the exact and text forms apart")
	}
	for _, noise := range []time.Duration{-13 * time.Microsecond, 0, 13 * time.Microsecond} {
		got, ok := ScanTextJulianDay(instant.Add(noise))
		if !ok || got != want {
			t.Errorf("ScanTextJulianDay(%v) = %.17g, %v; want %.17g", noise, got, ok, want)
		}
	}
}
