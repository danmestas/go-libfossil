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

// fossilX87EventTimes are event.mtime values the pinned linux-x64 fossil 2.28
// binary stored in CI, where its printf runs in 80-bit x87 long double: each
// differs from the value a macOS arm64 fossil stores for the same time.
var fossilX87EventTimes = []struct {
	millis int64
	stored float64
}{
	{1500607170788, 2457955.6385507882}, // 2017-07-21T03:19:30.788
	{1703782442848, 2460307.2041996298}, // 2023-12-28T16:54:02.848
	{1213801528799, 2454636.1288055442}, // 2008-06-18T15:05:28.799
	{2749748851292, 2472413.2968899538}, // 2057-02-18T19:07:31.292
	{398468886501, 2445199.4084085771},  // 1982-08-17T21:48:06.501
	{2913052665765, 2474303.3873352441}, // 2062-04-23T21:17:45.765
	{4035926687809, 2487299.6144422339}, // 2097-11-22T02:44:47.809
	{2325908021947, 2467507.7317354982}, // 2043-09-15T05:33:41.947
	{2797736607283, 2472968.7107324428}, // 2058-08-28T05:03:27.283
	{1437264789630, 2457222.5091392361}, // 2015-07-19T00:13:09.630
}

// The text form follows the long double fossil's printf computes in: a
// 64-bit double reproduces macOS arm64 fossil's values, an 80-bit x87 long
// double (64-bit significand) linux-x64 fossil's.
func TestTextJulianDayPerLongDouble(t *testing.T) {
	for _, c := range fossilEventTimes {
		if got := textJulianDay(c.millis, 53); got != c.stored {
			t.Errorf("53-bit: %d = %.17g, macOS fossil stored %.17g", c.millis, got, c.stored)
		}
	}
	for _, c := range fossilX87EventTimes {
		if got := textJulianDay(c.millis, 64); got != c.stored {
			t.Errorf("64-bit: %d = %.17g, linux-x64 fossil stored %.17g", c.millis, got, c.stored)
		}
		if textJulianDay(c.millis, 53) == c.stored {
			t.Errorf("%d does not tell the long doubles apart", c.millis)
		}
	}
}

func TestPlatformLongDoubleBits(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         uint
	}{
		{"darwin", "arm64", 53}, {"windows", "amd64", 53}, {"windows", "arm64", 53},
		{"linux", "amd64", 64}, {"darwin", "amd64", 64}, {"freebsd", "amd64", 64},
		{"linux", "arm64", 113}, {"linux", "riscv64", 113}, {"js", "wasm", 53},
	}
	for _, c := range cases {
		if got := platformLongDoubleBits(c.goos, c.goarch); got != c.want {
			t.Errorf("%s/%s: %d bits, want %d", c.goos, c.goarch, got, c.want)
		}
	}
}
