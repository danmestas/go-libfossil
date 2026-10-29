//go:build !wasm

package fsltype

import "math/big"

// printf17gLong is fossilPrintf17g in a long double with a prec-bit
// significand, emulated with big.Float at that precision and
// round-to-nearest-even.
func printf17gLong(v0 float64, prec uint) string {
	if !(v0 > 0) || prec < 53 {
		panic("fsltype.printf17gLong: v0 must be positive and prec at least 53")
	}
	const precision = printf17gPrecision
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
	v.Add(v, ld(printf17gRounder()))
	if v.Cmp(ld(10)) >= 0 {
		scale(0.1)
		exp++
	}
	if exp < -4 || exp > precision {
		panic("fsltype.printf17gLong: exponent form is not used for Julian days")
	}
	ten := ld(10)
	return fossilDigits(exp, precision-exp, func() int64 {
		d, _ := v.Int64()
		v.Sub(v, new(big.Float).SetPrec(prec).SetInt64(d))
		v.Mul(v, ten)
		return d
	})
}
