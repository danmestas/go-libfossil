package fsltype

import (
	"math"
	"math/big"
	"runtime"
)

// Julian day numbers the way fossil stores them (#258).
//
// Fossil computes a time's Julian day with SQLite's julianday(), which counts
// whole milliseconds (iJD) and divides: iJD/86400000.0. Where it binds that
// double to a statement (tag_insert, so tagxref.mtime) the exact value is
// stored. Where it formats it into SQL text with its own printf("%.17g") —
// every event, plink, attachment and forumpost time — the text carries 16
// significant digits, produced digit by digit in long double arithmetic, and
// SQLite reads it back with its own, not correctly rounded, conversion
// (sqliteAtoF). JulianDayFromMillis gives the first,
// TextJulianDayFromMillis the second.
//
// The second depends on the platform fossil was built for, because fossil's
// printf (not SQLite: its julianday() counts integer milliseconds and its
// text-to-double conversion is integer arithmetic) carries the value in
// long double. That is a 64-bit double on macOS arm64 and Windows (MSVC),
// the 80-bit x87 format with a 64-bit significand on x86-64 Linux, macOS
// and the BSDs, and IEEE binary128 with a 113-bit significand on arm64
// Linux. The 16th digit, and so the stored double, differs between them for
// about one time in five. TextJulianDayFromMillis computes in the long
// double of the platform go-libfossil runs on (longDoubleBits), so it is
// bit-identical to the fossil built for that same platform: verified against
// fossil 2.28 on macOS arm64 and linux-x64. The binary128 case follows the
// same arithmetic but no fossil built that way has been compared. A
// repository written on one platform and rebuilt by fossil on another can
// therefore differ in these times by a unit or two in the last place --
// as two fossils on those platforms differ from each other -- never by a
// millisecond.

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

// MillisFromJulianDay returns the time a Julian day names, as milliseconds
// since the Unix epoch, converted exactly as SQLite's date.c does:
// (sqlite3_int64)(r*86400000.0+0.5), less the epoch. Every Julian day is
// positive, so the cast truncates where floor would.
func MillisFromJulianDay(jd float64) int64 {
	return int64(jd*millisPerDay+0.5) - julianEpochMillis
}

// TextJulianDayFromMillis returns the Julian day fossil stores where it writes
// one as SQL text: event, plink, attachment and forumpost times.
func TextJulianDayFromMillis(millis int64) float64 {
	return textJulianDay(millis, longDoubleBits)
}

// textJulianDay is TextJulianDayFromMillis for a fossil whose long double has
// a significand of prec bits.
func textJulianDay(millis int64, prec uint) float64 {
	return sqliteAtoF(fossilPrintf17g(JulianDayFromMillis(millis), prec))
}

// longDoubleBits is the significand width of the C long double fossil uses
// on the platform this program runs on: see the package comment above.
// Platforms it does not name fall back to 53 bits, which is wrong for some:
// ppc64le's long double is IBM double-double and mips64's is binary128, so
// fossil's last digit there may differ from ours.
var longDoubleBits = platformLongDoubleBits(runtime.GOOS, runtime.GOARCH)

func platformLongDoubleBits(goos, goarch string) uint {
	switch {
	case goos == "windows":
		return 53
	case goarch == "amd64" || goarch == "386":
		return 64
	case goos == "darwin" || goos == "ios":
		return 53
	case goarch == "arm64" || goarch == "riscv64" || goarch == "s390x" || goarch == "loong64":
		return 113
	default:
		return 53
	}
}

// fossilPrintf17g is fossil's printf("%.17g", v) (src/printf.c, etGENERIC)
// for a positive v, computed in a long double with a prec-bit significand
// and round-to-nearest-even, as the C arithmetic is. It normalizes v to
// [1, 10) by repeated scaling, adds a rounder (a double, as fossil's is) at
// the 16th digit, and emits digits by truncate-and-multiply; past 16
// significant digits it emits '0'.
func fossilPrintf17g(v0 float64, prec uint) string {
	if !(v0 > 0) || math.IsInf(v0, 0) {
		panic("fsltype.fossilPrintf17g: only finite positive values")
	}
	if prec != 53 && prec != 64 && prec != 113 {
		panic("fsltype.fossilPrintf17g: unknown long double precision")
	}
	const precision = 17 - 1 // etGENERIC counts the leading digit
	rounder := 0.5
	for i := 0; i < precision; i++ {
		rounder *= 0.1
	}
	ld := func(f float64) *big.Float { return new(big.Float).SetPrec(prec).SetFloat64(f) }
	v := ld(v0)
	scale := func(by float64) { v.Mul(v, ld(by)) }
	exp := 0
	for v.Cmp(ld(1e32)) >= 0 && exp <= 350 {
		scale(1e-32)
		exp += 32
	}
	for v.Cmp(ld(1e8)) >= 0 && exp <= 350 {
		scale(1e-8)
		exp += 8
	}
	for v.Cmp(ld(10)) >= 0 && exp <= 350 {
		scale(0.1)
		exp++
	}
	for v.Cmp(ld(1e-8)) < 0 && exp >= -350 {
		scale(1e8)
		exp -= 8
	}
	for v.Cmp(ld(1)) < 0 && exp >= -350 {
		scale(10)
		exp--
	}
	v.Add(v, ld(rounder))
	if v.Cmp(ld(10)) >= 0 {
		scale(0.1)
		exp++
	}
	if exp < -4 || exp > precision {
		panic("fsltype.fossilPrintf17g: exponent form is not used for Julian days")
	}
	return fossilDigits(v, exp, precision-exp)
}

// fossilDigits emits fossil's fixed-point digits for a value normalized to
// [1, 10): exp+1 before the point, fraction after it, trailing zeros removed.
// Each digit is et_getdigit's: truncate, subtract, multiply by ten, in v's
// precision.
func fossilDigits(v *big.Float, exp, fraction int) string {
	if exp < 0 {
		panic("fsltype.fossilDigits: leading zero form is not used for Julian days")
	}
	ten := new(big.Float).SetPrec(v.Prec()).SetInt64(10)
	buf := make([]byte, 0, exp+fraction+2)
	emitted := 0
	digit := func() byte {
		emitted++
		if emitted > fossilRealDigits {
			return '0'
		}
		d, _ := v.Int64()
		v.Sub(v, new(big.Float).SetPrec(v.Prec()).SetInt64(d))
		v.Mul(v, ten)
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
