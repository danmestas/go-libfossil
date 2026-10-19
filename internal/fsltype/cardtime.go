package fsltype

import "time"

// Card times the way fossil writes them. Fossil turns a check-in or tag date
// into its D card with date_in_standard_format (src/checkin.c), which is
// SQLite's strftime('%Y-%m-%dT%H:%M:%f', DATE): the date is written in UTC
// with exactly three fractional digits.

// CardTime returns the time fossil records for t given as the date of a new
// artifact, as `fossil commit --date-override` takes one: t in UTC, rounded
// to the nearest millisecond, except that a fraction past .999 stays at .999
// rather than carrying into the next second, as SQLite holds it there.
//
// This matches fossil except at an exact half millisecond, which rounds up
// here. Fossil's result for a tie comes from how its bundled SQLite prints
// the seconds as a double, and differs between SQLite versions (fossil 2.28
// writes .124 for .1245 but .020 for .0195), so it is not followed.
func CardTime(t time.Time) time.Time {
	t = t.UTC()
	ms := (t.Nanosecond() + 500000) / 1000000
	if ms > 999 {
		ms = 999
	}
	return t.Truncate(time.Second).Add(time.Duration(ms) * time.Millisecond)
}

// CardTimeNow returns the time fossil records for "now", the default date of
// a new artifact, given the clock reading now: SQLite reads the clock in whole
// milliseconds, so the rest is dropped rather than rounded.
func CardTimeNow(now time.Time) time.Time {
	return now.UTC().Truncate(time.Millisecond)
}
