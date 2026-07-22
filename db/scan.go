package db

import "time"

// julianEpoch is the Julian Day Number for the Unix epoch (1970-01-01 12:00:00 UTC).
const julianEpoch = 2440587.5

// millisPerDay is the number of milliseconds in one day. Every mtime this
// codebase writes is at millisecond resolution (TimeToJulian rounds to whole
// millis), so it is the grid the time.Time branch recovers a scanned instant
// onto.
const millisPerDay = 86400.0 * 1000.0

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
// Neither is hypothetical for values this library did not write: SQLite's own
// julianday() computes iJD/86400000.0, a different expression from TimeToJulian's
// julianEpoch + ms/86400000.0, and the two land one ulp apart on a real fraction
// of instants. Snapping such a value to the millisecond grid changes it.
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

// ScanTime converts a scanned mtime value to time.Time.
// Same driver-compatibility rationale as ScanJulianDay.
func ScanTime(v any) (time.Time, bool) {
	switch v := v.(type) {
	case time.Time:
		return v, true
	case float64:
		return julianToTime(v), true
	case int64:
		return julianToTime(float64(v)), true
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

// julianToTime converts a julian day to a time.Time. Note this truncates toward
// zero where timeToJulian rounds; the two are not inverses at sub-millisecond
// resolution. Pre-existing and left alone deliberately — ScanTime's consumers
// are display paths with no ordering boundary, and changing a conversion used
// there is a separate change from the exactness ScanJulianDay's callers need.
func julianToTime(jd float64) time.Time {
	millis := int64((jd - julianEpoch) * millisPerDay)
	return time.UnixMilli(millis).UTC()
}

// julianDayFromMillis converts whole milliseconds since the Unix epoch to a
// Julian Day Number, using the same expression TimeToJulian writes with, so a
// value recovered through it reproduces the original write bit-for-bit.
func julianDayFromMillis(millis int64) float64 {
	return julianEpoch + float64(millis)/millisPerDay
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
