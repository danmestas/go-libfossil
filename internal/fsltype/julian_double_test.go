//go:build !wasm

package fsltype

import (
	"go/build"
	"math/rand"
	"slices"
	"testing"
)

// On a platform whose long double is a double, fossil's %.17g is computed in
// float64 (printf17gDouble), with no math/big, so a wasm build links none.
// It must give exactly what the extended-precision path gives at 53 bits.
func TestPrintf17gDoubleMatchesLongDouble53(t *testing.T) {
	check := func(v float64) {
		t.Helper()
		if got, want := printf17gDouble(v), printf17gLong(v, 53); got != want {
			t.Fatalf("printf17gDouble(%v) = %q, printf17gLong(_, 53) = %q", v, got, want)
		}
	}
	// Every millisecond of a day, a day either side of a leap second and the
	// epoch, then random times across five millennia.
	for _, day := range []int64{0, 1483228800000 - 86400000, 1483228800000} {
		for ms := int64(0); ms < 86400000; ms += 997 {
			check(JulianDayFromMillis(day + ms))
		}
	}
	rng := rand.New(rand.NewSource(258))
	for i := 0; i < 50000; i++ {
		check(JulianDayFromMillis(rng.Int63n(2*95617584000000) - 95617584000000))
	}
	for _, v := range []float64{1, 9.999999999999999, 10, 123456.789, 99999999.99999999, 2.4605e6} {
		check(v)
	}
}

// The wasm build of this package must not import math/big: big.Float is
// about 5KB of a gzipped TinyGo binary, for arithmetic a double does exactly.
func TestWasmBuildDoesNotImportMathBig(t *testing.T) {
	for _, goos := range []string{"js", "wasip1"} {
		ctx := build.Default
		ctx.GOOS, ctx.GOARCH = goos, "wasm"
		pkg, err := ctx.ImportDir(".", 0)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(pkg.Imports, "math/big") {
			t.Fatalf("%s/wasm build of fsltype imports math/big", goos)
		}
	}
}
