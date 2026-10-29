//go:build wasm

package fsltype

// printf17gLong is never reached in a wasm build: longDoubleBits is 53
// there, which fossilPrintf17g computes in float64. Leaving out the
// big.Float emulation keeps math/big out of the binary.
func printf17gLong(v0 float64, prec uint) string {
	panic("fsltype.printf17gLong: a wasm build has no extended long double")
}
