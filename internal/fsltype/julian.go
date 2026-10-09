package fsltype

import (
	"math"
	"strconv"
)

// Julian day numbers the way fossil stores them (#258).
//
// Fossil computes a time's Julian day with SQLite's julianday(), which counts
// whole milliseconds (iJD) and divides: iJD/86400000.0. Where it binds that
// double to a statement (tag_insert, so tagxref.mtime) the exact value is
// stored. Where it formats it into SQL text with its own printf("%.17g") —
// every event, plink, attachment and forumpost time — the text carries 16
// significant digits, produced digit by digit in long double arithmetic, and
// SQLite reads it back. JulianDayFromMillis gives the first,
// TextJulianDayFromMillis the second.
//
// The second depends on the platform fossil was built for: long double is a
// 64-bit double on macOS arm64 and Windows, 80-bit on x86-64 Linux and 128-bit
// on arm64 Linux, and the last digit can differ between them. This follows
// the 64-bit case, so its values are bit-identical to those a macOS or
// Windows fossil writes; against other builds an occasional row differs in
// the 16th digit, as those builds' rows differ from each other.

// julianEpochMillis is the Unix epoch as SQLite's iJD: 2440587.5 days in
// milliseconds.
const julianEpochMillis = 210866760000000

const millisPerDay = 86400000.0

// fossilRealDigits is how many significant digits fossil's %.17g emits.
const fossilRealDigits = 16

// JulianDayFromMillis returns SQLite's julianday() for a time given as
// milliseconds since the Unix epoch: the value fossil binds for tagxref.mtime.
func JulianDayFromMillis(millis int64) float64 {
	return float64(millis+julianEpochMillis) / millisPerDay
}

// TextJulianDayFromMillis returns the Julian day fossil stores where it writes
// one as SQL text: event, plink, attachment and forumpost times.
func TextJulianDayFromMillis(millis int64) float64 {
	jd := JulianDayFromMillis(millis)
	v, err := strconv.ParseFloat(fossilPrintf17g(jd), 64)
	if err != nil {
		panic("fsltype.TextJulianDayFromMillis: " + err.Error())
	}
	return v
}

// fossilPrintf17g is fossil's printf("%.17g", v) (src/printf.c, etGENERIC)
// for a positive v, with long double taken as double. It normalizes v to
// [1, 10) by repeated scaling, adds a rounder at the 16th digit, and emits
// digits by truncate-and-multiply; past 16 significant digits it emits '0'.
// Each step rounds as fossil's does, so the text — and the double read back
// from it — matches fossil's.
func fossilPrintf17g(v float64) string {
	if !(v > 0) || math.IsInf(v, 0) {
		panic("fsltype.fossilPrintf17g: only finite positive values")
	}
	const precision = 17 - 1 // etGENERIC counts the leading digit
	rounder := 0.5
	for i := 0; i < precision; i++ {
		rounder *= 0.1
	}
	exp := 0
	for v >= 1e32 && exp <= 350 {
		v *= 1e-32
		exp += 32
	}
	for v >= 1e8 && exp <= 350 {
		v *= 1e-8
		exp += 8
	}
	for v >= 10.0 && exp <= 350 {
		v *= 0.1
		exp++
	}
	for v < 1e-8 && exp >= -350 {
		v *= 1e8
		exp -= 8
	}
	for v < 1.0 && exp >= -350 {
		v *= 10.0
		exp--
	}
	v += rounder
	if v >= 10.0 {
		v *= 0.1
		exp++
	}
	if exp < -4 || exp > precision {
		panic("fsltype.fossilPrintf17g: exponent form is not used for Julian days")
	}
	return fossilDigits(v, exp, precision-exp)
}

// fossilDigits emits fossil's fixed-point digits for a value normalized to
// [1, 10): exp+1 before the point, fraction after it, trailing zeros removed.
func fossilDigits(v float64, exp, fraction int) string {
	if exp < 0 {
		panic("fsltype.fossilDigits: leading zero form is not used for Julian days")
	}
	buf := make([]byte, 0, exp+fraction+2)
	emitted := 0
	digit := func() byte {
		emitted++
		if emitted > fossilRealDigits {
			return '0'
		}
		d := int(v)
		v = (v - float64(d)) * 10.0
		return byte('0' + d)
	}
	for i := 0; i <= exp; i++ {
		buf = append(buf, digit())
	}
	buf = append(buf, '.')
	for i := 0; i < fraction; i++ {
		buf = append(buf, digit())
	}
	for buf[len(buf)-1] == '0' {
		buf = buf[:len(buf)-1]
	}
	if buf[len(buf)-1] == '.' {
		buf = buf[:len(buf)-1]
	}
	return string(buf)
}
