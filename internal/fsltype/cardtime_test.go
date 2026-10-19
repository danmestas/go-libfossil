package fsltype

import (
	"testing"
	"time"
)

// fossilCardTimes are D-card times the fossil 2.28 binary wrote for a given
// date, from `fossil test-date-format`: to the nearest millisecond, except
// that a fraction past .999 stays in its second.
var fossilCardTimes = []struct {
	in   time.Time
	want string
}{
	{time.Date(2026, 10, 18, 12, 34, 56, 789600000, time.UTC), "2026-10-18T12:34:56.790"},
	{time.Date(2026, 10, 18, 12, 34, 56, 789499999, time.UTC), "2026-10-18T12:34:56.789"},
	{time.Date(2026, 10, 18, 23, 59, 59, 999600000, time.UTC), "2026-10-18T23:59:59.999"},
	{time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC), "2026-10-18T12:34:56.000"},
	{time.Date(2026, 10, 18, 12, 34, 56, 123456700, time.UTC), "2026-10-18T12:34:56.123"},
}

func TestCardTimeMatchesFossil(t *testing.T) {
	for _, c := range fossilCardTimes {
		got := CardTime(c.in).Format("2006-01-02T15:04:05.000")
		if got != c.want {
			t.Errorf("CardTime(%s) = %s, fossil writes %s",
				c.in.Format(time.RFC3339Nano), got, c.want)
		}
	}
}

// A time in another zone is the same instant in UTC, as fossil reads it.
func TestCardTimeIsUTC(t *testing.T) {
	in := time.Date(2026, 10, 18, 7, 34, 56, 789600000, time.FixedZone("CDT", -5*3600))
	got := CardTime(in)
	if got.Location() != time.UTC {
		t.Fatalf("CardTime location = %v, want UTC", got.Location())
	}
	if want := time.Date(2026, 10, 18, 12, 34, 56, 790000000, time.UTC); !got.Equal(want) {
		t.Fatalf("CardTime = %v, want %v", got, want)
	}
}

// "now" is the clock to the millisecond, the rest dropped: SQLite reads the
// clock in whole milliseconds.
func TestCardTimeNowTruncates(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 34, 56, 789999999, time.UTC)
	want := time.Date(2026, 10, 18, 12, 34, 56, 789000000, time.UTC)
	if got := CardTimeNow(now); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("CardTimeNow(%v) = %v, want %v", now, got, want)
	}
}

// A time already on the millisecond is left alone, so the check-in path can
// apply CardTime to a CardTimeNow reading.
func TestCardTimeKeepsMilliseconds(t *testing.T) {
	base := time.Date(2026, 10, 18, 12, 34, 0, 0, time.UTC)
	for ms := 0; ms < 60000; ms++ {
		in := base.Add(time.Duration(ms) * time.Millisecond)
		if got := CardTime(in); !got.Equal(in) {
			t.Fatalf("CardTime(%v) = %v, want it unchanged", in, got)
		}
	}
}

// An exact half millisecond rounds up. Fossil's answer there depends on how
// its bundled SQLite prints a double (2.28 gives .124 for .1245 and .020 for
// .0195), so CardTime does not follow it.
func TestCardTimeRoundsTiesUp(t *testing.T) {
	for _, c := range []struct {
		nsec int
		want string
	}{
		{789500000, "12:34:56.790"},
		{124500000, "12:34:56.125"},
		{19500000, "12:34:56.020"},
		{500000, "12:34:56.001"},
		{998500000, "12:34:56.999"},
	} {
		in := time.Date(2026, 10, 18, 12, 34, 56, c.nsec, time.UTC)
		if got := CardTime(in).Format("15:04:05.000"); got != c.want {
			t.Errorf("CardTime(%d ns) = %s, want %s", c.nsec, got, c.want)
		}
	}
}
