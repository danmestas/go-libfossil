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
	// From `fossil rebuild` of fossil's own repository: times whose 16-digit
	// text SQLite 3.52 (the SQLite fossil 2.28 bundles) reads back as a
	// different double than a correctly rounded parse gives.
	{1302747281361, 2455665.593534270768},
	{1313399551889, 2455788.883702418767}, // 2011-08-15T09:12:31.889
	{1428972125358, 2457126.529228680767},
	{1456907938278, 2457449.860396736767},
	{1478475358139, 2457699.483311793766},
	{1499253127655, 2457939.966755266766},
	{1503993508436, 2457994.832273564767},
	{1500607170788, 2457955.638550786767},
	{1509807383510, 2458062.122494328768},
	{1567626976988, 2458731.330752175766},
	{1577212764809, 2458842.277370474767},
	{1597715665130, 2459079.579457523766},
	{1645384477112, 2459631.301818425768},
	{1703782442848, 2460307.204199630768},
	{1733858293638, 2460655.304324513767},
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
