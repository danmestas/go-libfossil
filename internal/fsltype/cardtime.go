package fsltype

import (
	"strconv"
	"time"
)

// Card times the way fossil writes them. Fossil turns a check-in or tag date
// into its D card with date_in_standard_format (src/checkin.c), which is
// SQLite's strftime('%Y-%m-%dT%H:%M:%f', DATE): the date is written in UTC
// with exactly three fractional digits.

// CardTime returns the time fossil records for t given as the date of a new
// artifact, as `fossil commit --date-override` takes one. SQLite reads the
// seconds as a double s, holding a fraction past .999 at .999 so that it never
// carries into the next minute, and %f prints s with printf("%06.3f"). That
// rounds half up, on s's decimal expansion to 18 significant digits, so a
// fraction such as .7895, whose double lies just below it, goes down.
func CardTime(t time.Time) time.Time {
	t = t.UTC()
	frac := float64(t.Nanosecond()) / 1e9
	if frac > 0.999 {
		frac = 0.999
	}
	ms := roundMillis(float64(t.Second()) + frac)
	minute := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	return minute.Add(time.Duration(ms) * time.Millisecond)
}

// roundMillis is SQLite's printf("%.3f", s) for 0 <= s < 60, as a count of
// milliseconds: sqlite3FpDecode takes s to 18 significant digits and rounds
// half up at the third decimal.
func roundMillis(s float64) int64 {
	if s < 0 || s >= 60 {
		panic("fsltype.roundMillis: seconds out of range")
	}
	e := strconv.FormatFloat(s, 'e', 17, 64) // d.ddddddddddddddddde±XX
	digits := e[:1] + e[2:19]
	exp, err := strconv.Atoi(e[20:])
	if err != nil {
		panic("fsltype.roundMillis: " + err.Error())
	}
	keep := exp + 1 + 3 // digits up to and including the third decimal
	if keep < 0 {
		return 0
	}
	var ms int64
	for i := 0; i < keep; i++ {
		ms = ms*10 + int64(digits[i]-'0')
	}
	if digits[keep] >= '5' {
		ms++
	}
	return ms
}

// CardTimeNow returns the time fossil records for "now", the default date of
// a new artifact, given the clock reading now: SQLite reads the clock in whole
// milliseconds, so the rest is dropped rather than rounded.
func CardTimeNow(now time.Time) time.Time {
	return now.UTC().Truncate(time.Millisecond)
}
