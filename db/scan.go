package db

import (
	"time"

	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// ScanJulianDay converts a scanned mtime value to a float64 Julian Day Number.
// SQLite drivers return mtime differently: modernc returns float64, ncruces
// returns time.Time for DATETIME/TIMESTAMP/DATE columns. Scan the column into
// an `any` variable, then pass it here.
//
// The float64 branch returns the scanned value verbatim, and that exactness is
// load-bearing rather than incidental. A float64 read off the row IS the stored
// value, so any "canonicalizing" transform can only move it away from what the
// row holds. Two consumers compare a scanned mtime against other stored mtimes
// and would silently return a wrong answer (never an error) if the value were
// perturbed even by one ulp:
//
//   - Timeline's pagination cursor, whose predicate `e.mtime < ? OR (e.mtime = ?
//     AND b.rid < ?)` needs the cursor to compare exactly equal to the row it
//     came from. Raise it by an ulp and that row repeats as the next page's
//     first row; lower it and rows are skipped.
//   - tag propagation's `tagxref.mtime < ?`, where SQLite compares the bound
//     value against raw stored floats.
//
// Neither is hypothetical for values this library did not write: a value
// another writer computed differently, or at finer than millisecond
// resolution, is not on the grid TimeToJulian writes, and snapping it to that
// grid changes it.
//
// The time.Time branch is the one case where a transform is required rather than
// harmful — see timeToJulian.
func ScanJulianDay(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case time.Time:
		return timeToJulian(v), true
	case int64:
		// Integer columns only appear as the COALESCE(mtime, 0) "no mtime"
		// sentinel; pass it through so callers' `mtime != 0` guard holds.
		return float64(v), true
	default:
		return 0, false
	}
}

// ScanTextJulianDay is ScanJulianDay for a column fossil writes as SQL text:
// event, plink, attachment and forumpost times. Those hold the 16-digit form
// (fsltype.TextJulianDayFromMillis), so a time.Time a driver hands back is
// converted to that form rather than to the exact julianday() ScanJulianDay
// gives for tagxref.mtime. A float64 passes through unchanged either way.
func ScanTextJulianDay(v any) (float64, bool) {
	if t, ok := v.(time.Time); ok {
		return fsltype.TextJulianDayFromMillis(t.Round(time.Millisecond).UTC().UnixMilli()), true
	}
	return ScanJulianDay(v)
}

// ScanTime converts a scanned mtime value to time.Time, at the millisecond
// SQLite's date functions give it: the time a check-in's D card records.
// Same driver-compatibility rationale as ScanJulianDay.
func ScanTime(v any) (time.Time, bool) {
	switch v := v.(type) {
	case time.Time:
		// The driver converted the stored day through its own REAL
		// arithmetic; see timeToJulian.
		return v.Round(time.Millisecond).UTC(), true
	case float64:
		return fsltype.JulianToTime(v), true
	case int64:
		return fsltype.JulianToTime(float64(v)), true
	default:
		return time.Time{}, false
	}
}

// ScanInt converts a scanned value to int.
// SQLite drivers differ on BOOLEAN columns: modernc returns int64,
// ncruces returns bool. Scan the column into an `any` variable, then pass it here.
func ScanInt(v any) (int, bool) {
	switch v := v.(type) {
	case int64:
		return int(v), true
	case int:
		return v, true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

// julianDayFromMillis converts whole milliseconds since the Unix epoch to a
// Julian Day Number with the same function TimeToJulian writes with, so a
// value recovered through it reproduces the original write bit-for-bit.
func julianDayFromMillis(millis int64) float64 {
	return fsltype.JulianDayFromMillis(millis)
}

// timeToJulian converts a driver-scanned time.Time back to a julian day
// float64. Rounds to the nearest millisecond rather than truncating: the
// ncruces/WASM sqlite driver hands back DATETIME columns as a time.Time
// via its own internal REAL conversion, which carries sub-millisecond
// noise (observed ~13us) around the true millisecond-aligned instant every
// mtime in this codebase is written as. A plain UnixMilli() truncates that
// noise into a full missed millisecond when it lands just below the true
// value; rounding recovers the exact intended millisecond, which is what
// makes ScanJulianDay(scannedTime) reproduce the same float64 bit-for-bit
// as the value TimeToJulian originally wrote, across both drivers. This
// matters beyond cosmetics: Timeline's pagination cursor is built from
// exactly this value and depends on it matching the stored row exactly.
//
// The recovery is exact only for mtimes written at millisecond resolution,
// which is every mtime this codebase writes. An mtime written by upstream
// Fossil via julianday() can carry finer precision, and this branch cannot
// reproduce it — event.mtime is declared DATETIME, so ncruces has already
// converted it through a time.Time before this code sees the value. That is a
// driver limitation with no fix available here; rounding the float64 branch to
// match would not recover the precision, only discard it on both drivers and
// break the exact-cursor invariant in the process.
func timeToJulian(t time.Time) float64 {
	return julianDayFromMillis(t.Round(time.Millisecond).UTC().UnixMilli())
}
