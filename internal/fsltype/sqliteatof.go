package fsltype

import (
	"math"
	"math/bits"
)

// sqliteAtoF reads a decimal Julian day the way SQLite 3.52.0, the SQLite
// fossil 2.28 bundles, reads a real literal in SQL text: sqlite3AtoF and
// sqlite3Fp10Convert2 in its src/util.c. That conversion is not correctly
// rounded -- it multiplies the decimal significand by a 64-bit
// approximation of the power of ten -- so for some 16-digit Julian days it
// returns a double one unit in the last place away from the one
// strconv.ParseFloat returns, and fossil stores SQLite's.
//
// s is the text fossilPrintf17g writes: digits, a point, digits.
func sqliteAtoF(s string) float64 {
	const largestInt64 = math.MaxInt64
	var sig uint64 // significand
	exp := 0       // value is sig * 10^exp
	i := 0
	for ; i < len(s) && isDigit(s[i]); i++ {
		sig = sig*10 + uint64(s[i]-'0')
		if sig >= (largestInt64-9)/10 {
			for i+1 < len(s) && isDigit(s[i+1]) {
				i++
				exp++
			}
		}
	}
	if i < len(s) && s[i] == '.' {
		for i++; i < len(s) && isDigit(s[i]); i++ {
			if sig < (largestInt64-9)/10 {
				sig = sig*10 + uint64(s[i]-'0')
				exp--
			}
		}
	}
	if i != len(s) || sig == 0 {
		panic("fsltype.sqliteAtoF: not a positive decimal: " + s)
	}
	return sqliteFp10Convert2(sig, exp)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// sqliteFp10Convert2 is SQLite 3.52.0's sqlite3Fp10Convert2: the double
// nearest, by its arithmetic, to d * 10^p.
func sqliteFp10Convert2(d uint64, p int) float64 {
	if d&(1<<63) != 0 || d == 0 {
		panic("fsltype.sqliteFp10Convert2: significand out of range")
	}
	if p < powersOf10First {
		return 0
	}
	if p > powersOf10Last {
		return math.Inf(1)
	}
	lz := bits.LeadingZeros64(d)
	lp := pwr10to2(p)
	e1 := lz - (lp + 11)
	if e1 > 1074 {
		if e1 >= 1130 {
			return 0
		}
		e1 = 1074
	}
	h, _ := bits.Mul64(d<<lz, powerOfTen(p))
	x := lz - (e1 + lp + 3)
	if x < 0 || x > 63 {
		panic("fsltype.sqliteFp10Convert2: shift out of range")
	}
	out := h >> x
	if out >= 1<<55-2 {
		out >>= 1
		e1--
	}
	if e1 <= -972 {
		return math.Inf(1)
	}
	out = (out + 2) >> 2
	if out&(1<<52) != 0 {
		out = out&^(1<<52) | uint64(1075-e1)<<52
	}
	return math.Float64frombits(out)
}

const (
	powersOf10First = -348
	powersOf10Last  = 347
)

// pwr10to2 is SQLite's floor(log2(10^p)) approximation.
func pwr10to2(p int) int { return (p * 108853) >> 15 }

// powerOfTen is SQLite 3.52.0's powerOfTen: the most significant 64 bits
// of 10^p, for p in [-348, 347].
func powerOfTen(p int) uint64 {
	if p < powersOf10First || p > powersOf10Last {
		panic("fsltype.powerOfTen: p out of range")
	}
	var g, n int
	switch {
	case p < 0:
		g, n = p/27, p%27
		if n != 0 {
			g--
			n += 27
		}
	case p < 27:
		return pow10Base[p]
	default:
		g, n = p/27, p%27
	}
	y := pow10Scale[g+13]
	if n == 0 {
		return y
	}
	x, _ := bits.Mul64(pow10Base[n], y)
	if x&(1<<63) == 0 {
		x = x<<1 | 1
	}
	return x
}

// pow10Base holds 10^0 through 10^26, each shifted to fill 64 bits.
var pow10Base = [27]uint64{
	0x8000000000000000, 0xa000000000000000, 0xc800000000000000, 0xfa00000000000000,
	0x9c40000000000000, 0xc350000000000000, 0xf424000000000000, 0x9896800000000000,
	0xbebc200000000000, 0xee6b280000000000, 0x9502f90000000000, 0xba43b74000000000,
	0xe8d4a51000000000, 0x9184e72a00000000, 0xb5e620f480000000, 0xe35fa931a0000000,
	0x8e1bc9bf04000000, 0xb1a2bc2ec5000000, 0xde0b6b3a76400000, 0x8ac7230489e80000,
	0xad78ebc5ac620000, 0xd8d726b7177a8000, 0x878678326eac9000, 0xa968163f0a57b400,
	0xd3c21bcecceda100, 0x84595161401484a0, 0xa56fa5b99019a5c8,
}

// pow10Scale holds 10^(27*(i-13)) for i in 0..25, each shifted to fill 64
// bits.
var pow10Scale = [26]uint64{
	0x8049a4ac0c5811ae, 0xcf42894a5dce35ea, 0xa76c582338ed2622, 0x873e4f75e2224e68,
	0xda7f5bf590966849, 0xb080392cc4349ded, 0x8e938662882af53e, 0xe65829b3046b0afa,
	0xba121a4650e4ddec, 0x964e858c91ba2655, 0xf2d56790ab41c2a3, 0xc428d05aa4751e4d,
	0x9e74d1b791e07e48, 0x8000000000000000, 0xcecb8f27f4200f3a, 0xa70c3c40a64e6c52,
	0x86f0ac99b4e8dafd, 0xda01ee641a708dea, 0xb01ae745b101e9e4, 0x8e41ade9fbebc27d,
	0xe5d3ef282a242e82, 0xb9a74a0637ce2ee1, 0x95f83d0a1fb69cd9, 0xf24a01a73cf2dcd0,
	0xc3b8358109e84f07, 0x9e19db92b4e31ba9,
}
