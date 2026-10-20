package fsltype

import (
	"testing"
	"time"
)

// fossilEventTimes are event.mtime values the fossil 2.28 binary (macOS arm64)
// stored, with the D-card time of each in milliseconds: the first six from a
// `fossil rebuild` of the althttpd repository, the last two from commits.
var fossilEventTimes = []struct {
	millis int64
	stored float64
}{
	{1606831156077, 2459185.082824966}, // 2020-12-01T13:59:16.077
	{1606831367825, 2459185.085275753}, // 2020-12-01T14:02:47.825
	{1607520762942, 2459193.064385903}, // 2020-12-09T13:32:42.942
	{1606831563360, 2459185.087538889}, // 2020-12-01T14:06:03.360
	{1607520969888, 2459193.066781112}, // 2020-12-09T13:36:09.888
	{1607779347305, 2459196.057260475}, // 2020-12-12T13:22:27.305
	{1791557956502, 2461323.124496551}, // 2026-10-09T14:59:16.502
	{1791557956580, 2461323.124497454}, // 2026-10-09T14:59:16.580
}

// TextJulianDayFromMillis reproduces the doubles fossil stores for event
// times, bit for bit.
func TestTextJulianDayMatchesFossil(t *testing.T) {
	for _, c := range fossilEventTimes {
		if got := TextJulianDayFromMillis(c.millis); got != c.stored {
			t.Errorf("TextJulianDayFromMillis(%d) = %.17g, fossil stored %.17g",
				c.millis, got, c.stored)
		}
	}
}

// JulianDayFromMillis is SQLite's julianday(): whole milliseconds over
// 86400000. The first althttpd time is one where it and the text form
// differ, which is the difference #258 reported.
func TestJulianDayIsSQLiteJulianday(t *testing.T) {
	const millis = 1606831156077
	want := float64(millis+210866760000000) / 86400000.0
	if got := JulianDayFromMillis(millis); got != want {
		t.Fatalf("JulianDayFromMillis = %.17g, want %.17g", got, want)
	}
	if JulianDayFromMillis(millis) == TextJulianDayFromMillis(millis) {
		t.Fatal("test case does not tell the exact and text forms apart")
	}
}

// Both forms convert back to the same millisecond, so the timeline's
// (mtime, rid) cursor still round-trips.
func TestJulianFormsRoundTrip(t *testing.T) {
	for _, c := range fossilEventTimes {
		want := time.UnixMilli(c.millis).UTC()
		forms := []float64{JulianDayFromMillis(c.millis), TextJulianDayFromMillis(c.millis)}
		for _, jd := range forms {
			if got := JulianToTime(jd); !got.Equal(want) {
				t.Errorf("JulianToTime(%.17g) = %v, want %v", jd, got, want)
			}
		}
	}
}

// MillisFromJulianDay converts as SQLite's date.c does,
// (sqlite3_int64)(r*86400000.0+0.5): fossil's stored event times recover
// their D-card millisecond, and a value half a millisecond either side of the
// grid rounds the way SQLite rounds it.
func TestMillisFromJulianDayMatchesSQLite(t *testing.T) {
	for _, c := range fossilEventTimes {
		if got := MillisFromJulianDay(c.stored); got != c.millis {
			t.Errorf("MillisFromJulianDay(%.17g) = %d, want the D card's %d", c.stored, got, c.millis)
		}
		if got := JulianToTime(c.stored).UnixMilli(); got != c.millis {
			t.Errorf("JulianToTime(%.17g) = %d ms, want the D card's %d", c.stored, got, c.millis)
		}
	}

	const iJD = julianEpochMillis + 1791557956502
	cases := []struct {
		jd   float64
		want int64
	}{
		{float64(iJD) / millisPerDay, 1791557956502},
		{(float64(iJD) + 0.4) / millisPerDay, 1791557956502},
		{(float64(iJD) - 0.4) / millisPerDay, 1791557956502},
		{(float64(iJD) + 0.6) / millisPerDay, 1791557956503},
		{(float64(iJD) - 0.6) / millisPerDay, 1791557956501},
		{julianEpoch, 0},
		{julianEpoch + 1, 86400000},
	}
	for _, c := range cases {
		jd := c.jd
		sqlite := int64(jd*86400000.0+0.5) - julianEpochMillis
		if sqlite != c.want {
			t.Fatalf("case %.17g: SQLite's own formula gives %d, the case wants %d", jd, sqlite, c.want)
		}
		if got := MillisFromJulianDay(jd); got != c.want {
			t.Errorf("MillisFromJulianDay(%.17g) = %d, want %d", jd, got, c.want)
		}
	}
}
