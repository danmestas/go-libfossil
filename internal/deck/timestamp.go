package deck

import (
	"fmt"
	"strconv"
	"time"
)

// Timestamp parsing for the D and E cards, per §4.6.4 as amended by the
// §4.6.4 erratum. The token is never escape-decoded. fossil parses these with
// SQLite's julianday(); this accepts the same forms SQLite does for the shapes
// an artifact carries -- a date-time, a date only, and a bare Julian-day
// number -- with the field ranges the erratum scopes to forms 1 through 3.
//
// The relative literals now/subsec/subsecond (form 4) and the bare time-only
// form (form 3) are deliberately not accepted: no stored artifact carries
// them (a producer MUST emit the absolute date-time form), and resolving "now"
// against the parse-time clock would make Parse non-deterministic.

// julianEpochMillis is the Unix epoch expressed as SQLite's integer Julian day
// in milliseconds (2440587.5 days). It mirrors fsltype.julianEpochMillis and
// is repeated here to keep the deck package free of that dependency.
const julianEpochMillis = 210866760000000

const millisPerJulianDay = 86400000

// parseTimestamp parses a D/E timestamp token. It returns an error for a token
// no accepted form admits, or one whose fields are out of range.
func parseTimestamp(s string) (time.Time, error) {
	if t, matched, err := parseCalendarTimestamp(s); matched {
		return t, err
	}
	if t, matched, err := parseJulianTimestamp(s); matched {
		return t, err
	}
	return time.Time{}, fmt.Errorf("cannot parse timestamp %q", s)
}

// tsCursor is a byte cursor over a timestamp token.
type tsCursor struct {
	s string
	i int
}

func (c *tsCursor) atEnd() bool { return c.i >= len(c.s) }

func (c *tsCursor) peek() byte {
	if c.i < len(c.s) {
		return c.s[c.i]
	}
	return 0
}

// digits reads exactly n decimal digits and returns their value. It fails if
// fewer than n digits are present, so a field of the wrong width is rejected.
func (c *tsCursor) digits(n int) (int, bool) {
	if c.i+n > len(c.s) {
		return 0, false
	}
	v := 0
	for k := 0; k < n; k++ {
		d := c.s[c.i+k]
		if d < '0' || d > '9' {
			return 0, false
		}
		v = v*10 + int(d-'0')
	}
	c.i += n
	return v, true
}

// parseCalendarTimestamp parses forms 1 (date-time) and 2 (date only). matched
// reports whether the token looked like a calendar timestamp at all; once the
// leading "YYYY-" is seen it is matched, and any later problem is an error
// rather than a fall-through to the Julian form.
func parseCalendarTimestamp(s string) (t time.Time, matched bool, err error) {
	c := &tsCursor{s: s}
	neg := c.peek() == '-'
	if neg {
		c.i++
	}
	year, ok := c.digits(4)
	if !ok || c.peek() != '-' {
		return time.Time{}, false, nil
	}
	if neg {
		year = -year
	}
	c.i++ // the '-' after the year

	bad := func() (time.Time, bool, error) {
		return time.Time{}, true, fmt.Errorf("invalid timestamp %q", s)
	}

	// §4.6.4: month 01-12, day 01-31 with no calendar-aware check, each
	// exactly two digits.
	month, ok := c.digits(2)
	if !ok || month < 1 || month > 12 || c.peek() != '-' {
		return bad()
	}
	c.i++
	day, ok := c.digits(2)
	if !ok || day < 1 || day > 31 {
		return bad()
	}
	// A date or date-time MUST denote a date strictly after -4713-11-24, its
	// (year, month, day) lexicographically greater, independent of any time.
	if !afterJulianEpoch(year, month, day) {
		return bad()
	}

	hour, min, sec, nsec := 0, 0, 0, 0
	loc := time.UTC
	if !c.atEnd() {
		// §4.6.4: one or more separator bytes, each a whitespace-class byte or
		// the uppercase T. A lowercase t is rejected.
		seps := 0
		for !c.atEnd() && isTimestampSeparator(c.peek()) {
			seps++
			c.i++
		}
		if seps == 0 {
			return bad()
		}
		if hour, min, sec, nsec, loc, ok = c.parseTimeAndZone(); !ok {
			return bad()
		}
	}

	// time.Date normalizes out-of-day-range values the §4.6.4 grammar admits:
	// hour 24 rolls to the next day's midnight, and the uchecked day (e.g.
	// 2024-02-31) rolls forward, matching SQLite's julianday().
	return time.Date(year, time.Month(month), day, hour, min, sec, nsec, loc).UTC(), true, nil
}

// isTimestampSeparator reports whether b separates the date and time: a
// whitespace-class byte or the uppercase letter T (never lowercase t).
func isTimestampSeparator(b byte) bool {
	switch b {
	case 'T', ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

// parseTimeAndZone parses HH:MM, HH:MM:SS or HH:MM:SS.frac with an optional
// trailing timezone, consuming to the end of the token. The hour is 00-24, the
// minute and second 00-59, the fraction one or more digits.
func (c *tsCursor) parseTimeAndZone() (hour, min, sec, nsec int, loc *time.Location, ok bool) {
	loc = time.UTC
	if hour, ok = c.digits(2); !ok || hour > 24 {
		return 0, 0, 0, 0, nil, false
	}
	if c.peek() != ':' {
		return 0, 0, 0, 0, nil, false
	}
	c.i++
	if min, ok = c.digits(2); !ok || min > 59 {
		return 0, 0, 0, 0, nil, false
	}
	if c.peek() == ':' {
		c.i++
		if sec, ok = c.digits(2); !ok || sec > 59 {
			return 0, 0, 0, 0, nil, false
		}
		if c.peek() == '.' {
			c.i++
			if nsec, ok = c.fractionNanos(); !ok {
				return 0, 0, 0, 0, nil, false
			}
		}
	}
	// Optional timezone: optional whitespace, then +HH:MM, -HH:MM, Z or z.
	for !c.atEnd() && isTimestampSeparator(c.peek()) && c.peek() != 'T' {
		c.i++
	}
	if !c.atEnd() {
		switch z := c.peek(); {
		case z == 'Z' || z == 'z':
			c.i++
		case z == '+' || z == '-':
			c.i++
			zh, ok1 := c.digits(2)
			if !ok1 || c.peek() != ':' {
				return 0, 0, 0, 0, nil, false
			}
			c.i++
			zm, ok2 := c.digits(2)
			if !ok2 {
				return 0, 0, 0, 0, nil, false
			}
			offset := (zh*60 + zm) * 60
			if z == '-' {
				offset = -offset
			}
			loc = time.FixedZone("", offset)
		default:
			return 0, 0, 0, 0, nil, false
		}
	}
	if !c.atEnd() {
		return 0, 0, 0, 0, nil, false // trailing bytes
	}
	return hour, min, sec, nsec, loc, true
}

// fractionNanos reads one or more fractional-second digits and returns them as
// nanoseconds, truncating past nine digits.
func (c *tsCursor) fractionNanos() (int, bool) {
	start := c.i
	for c.i < len(c.s) && c.s[c.i] >= '0' && c.s[c.i] <= '9' {
		c.i++
	}
	if c.i == start {
		return 0, false
	}
	frac := c.s[start:c.i]
	if len(frac) > 9 {
		frac = frac[:9]
	}
	nsec := 0
	for k := 0; k < 9; k++ {
		nsec *= 10
		if k < len(frac) {
			nsec += int(frac[k] - '0')
		}
	}
	return nsec, true
}

// afterJulianEpoch reports whether (year, month, day) is lexicographically
// greater than (-4713, 11, 24), the §4.6.4 lower bound.
func afterJulianEpoch(year, month, day int) bool {
	if year != -4713 {
		return year > -4713
	}
	if month != 11 {
		return month > 11
	}
	return day > 24
}

// parseJulianTimestamp parses form 5, a bare decimal Julian day number, valid
// only for 0 < r < 5373484.5. matched reports whether the token is a bare
// number at all.
func parseJulianTimestamp(s string) (time.Time, bool, error) {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return time.Time{}, false, nil // no integer digits: not a number
	}
	if i < len(s) {
		if s[i] != '.' {
			return time.Time{}, false, nil
		}
		i++
		fracStart := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == fracStart || i < len(s) {
			return time.Time{}, false, nil
		}
	}
	r, convErr := strconv.ParseFloat(s, 64)
	if convErr != nil || !(r > 0 && r < 5373484.5) {
		return time.Time{}, true, fmt.Errorf("Julian day out of range: %q", s)
	}
	millis := int64(r*millisPerJulianDay+0.5) - julianEpochMillis
	return time.UnixMilli(millis).UTC(), true, nil
}
