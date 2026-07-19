package db

import "time"

// julianEpoch is the Julian Day Number for the Unix epoch (1970-01-01 12:00:00 UTC).
const julianEpoch = 2440587.5

// ScanJulianDay converts a scanned mtime value to a float64 Julian Day Number.
// SQLite drivers return mtime differently: modernc returns float64,
// ncruces returns time.Time for DATETIME/TIMESTAMP/DATE columns.
// Scan the column into an `any` variable, then pass it here.
func ScanJulianDay(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case time.Time:
		return timeToJulian(v), true
	case int64:
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

func julianToTime(jd float64) time.Time {
	millis := int64((jd - julianEpoch) * 86400.0 * 1000.0)
	return time.UnixMilli(millis).UTC()
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
func timeToJulian(t time.Time) float64 {
	return julianEpoch + float64(t.Round(time.Millisecond).UTC().UnixMilli())/(86400.0*1000.0)
}
