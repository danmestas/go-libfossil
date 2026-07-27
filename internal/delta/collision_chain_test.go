package delta

import (
	"bytes"
	"fmt"
	"testing"
)

// repetitiveInputs returns source/target pairs whose content is
// pathologically low-entropy: every nHash-byte window in the source hashes
// into a handful of buckets, so the encoder's collision chains are as long
// as the hash table can make them. This is ordinary content — repeated
// tokens, runs of whitespace, boilerplate SQL — not a synthetic adversary.
func repetitiveInputs() []struct {
	name           string
	source, target []byte
} {
	const n = 128 * 1024

	// A single repeated token, edited in the middle: the shape from the
	// issue's reproduction.
	repeated := bytes.Repeat([]byte("abcdefgh"), n/8)
	repeatedEdited := append([]byte(nil), repeated...)
	copy(repeatedEdited[n/2:], []byte("XXXXXXXXXX"))

	// A run of identical bytes has every window hashing to one bucket.
	zeros := make([]byte, n)
	zerosGrown := append(append([]byte(nil), zeros...), []byte("tail bytes")...)

	// Boilerplate lines: low entropy from indentation and repeated keywords
	// rather than from a single repeating byte pattern.
	var boiler []byte
	for len(boiler) < n {
		boiler = append(boiler, "    INSERT INTO blob(rid,uuid,size,content) VALUES(?,?,?,?);\n"...)
	}
	boilerEdited := append([]byte(nil), boiler...)
	copy(boilerEdited[len(boilerEdited)/3:], "    DELETE FROM blob WHERE rid=?;\n")

	// Source and target both repetitive but with different periods, so no
	// long run matches and the encoder walks chains without ever finding a
	// match worth emitting.
	period7 := bytes.Repeat([]byte("abcdefg"), n/7)

	return []struct {
		name           string
		source, target []byte
	}{
		{"repeated-token-mid-edit", repeated, repeatedEdited},
		{"repeated-token-truncated", repeated, repeated[:n/3]},
		{"all-zeros-grown", zeros, zerosGrown},
		{"boilerplate-lines", boiler, boilerEdited},
		{"mismatched-periods", repeated, period7},
		{"identical", repeated, append([]byte(nil), repeated...)},
	}
}

// TestCreate_RepetitiveInput_RoundTripsByteForByte pins the invariant that
// bounding the collision-chain walk could plausibly threaten: capping the
// search may select a shorter match than an exhaustive walk would, but the
// delta must still reconstruct the target exactly.
func TestCreate_RepetitiveInput_RoundTripsByteForByte(t *testing.T) {
	for _, tc := range repetitiveInputs() {
		t.Run(tc.name, func(t *testing.T) {
			d := Create(tc.source, tc.target)

			got, err := Apply(tc.source, d)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(got) != len(tc.target) {
				t.Fatalf("length mismatch: got %d, want %d", len(got), len(tc.target))
			}
			if !bytes.Equal(got, tc.target) {
				for i := range got {
					if got[i] != tc.target[i] {
						t.Fatalf("byte mismatch at %d: got %#x, want %#x", i, got[i], tc.target[i])
					}
				}
				t.Fatal("byte mismatch")
			}
		})
	}
}

// TestBestMatch_BoundsCollisionChainWalk is the perf regression guard. It
// asserts structurally — on a candidate count, not on wall-clock time — so
// it cannot flake on loaded or shared hardware: the encoder must never
// inspect more than maxChainCandidates chain entries for one target
// position, and on input whose chains are far longer than the cap it must
// actually reach the cap (otherwise the assertion would pass vacuously).
func TestBestMatch_BoundsCollisionChainWalk(t *testing.T) {
	source := bytes.Repeat([]byte("abcdefgh"), 128*1024/8)
	target := append([]byte(nil), source...)
	copy(target[len(target)/2:], []byte("XXXXXXXXXX"))

	heads, entries, mask := buildHashTable(source)

	if longest := longestChain(heads, entries); longest <= maxChainCandidates {
		t.Fatalf("test input is not pathological: longest collision chain is %d, "+
			"which does not exceed the cap of %d", longest, maxChainCandidates)
	}

	// Walk the target the way emitMatches does, so the candidate counts are
	// the ones the encoder actually pays for.
	maxExamined := 0
	atCap := 0
	for tPos := 0; tPos < len(target); {
		if tPos+nHash > len(target) {
			break
		}
		_, bestLen, examined := bestMatch(source, target, tPos, heads, entries, mask)
		if examined > maxChainCandidates {
			t.Fatalf("tPos %d: examined %d chain entries, cap is %d",
				tPos, examined, maxChainCandidates)
		}
		if examined > maxExamined {
			maxExamined = examined
		}
		if examined == maxChainCandidates {
			atCap++
		}
		if bestLen >= nHash {
			tPos += bestLen
		} else {
			tPos++
		}
	}

	if maxExamined != maxChainCandidates {
		t.Fatalf("cap never bit: most entries examined at any position was %d, want %d",
			maxExamined, maxChainCandidates)
	}
	// Assert the cap bit at more than one position. A single position hitting
	// it is also what a global budget consumed across the whole encode would
	// look like -- that refactor would silently degrade matching as the file
	// progressed while still satisfying the check above. Requiring several
	// positions to reach the cap pins the per-position reset.
	if atCap < 2 {
		t.Fatalf("only %d position(s) reached the cap of %d; the limit must reset "+
			"per target position, not be a single budget spent across the encode",
			atCap, maxChainCandidates)
	}
}

// longestChain reports the length of the longest hash-bucket collision
// chain, so a test can prove its input really does stress the walk.
func longestChain(heads []int, entries []hashEntry) int {
	longest := 0
	for _, head := range heads {
		n := 0
		for ei := head; ei > 0; ei = entries[ei-1].next + 1 {
			n++
		}
		if n > longest {
			longest = n
		}
	}
	return longest
}

// TestCreate_LargeRepetitiveInput_RoundTrips is the issue's reproduction at
// the size that made it intolerable. Without the collision-chain bound this
// does not finish in minutes; with it, it completes in well under a second.
// It carries no timing assertion — a regression shows up as the package
// test timeout, which is a signal a reviewer can read without it being able
// to flake on a slow machine.
func TestCreate_LargeRepetitiveInput_RoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1 MiB delta creation in short mode")
	}

	const n = 1 << 20
	source := bytes.Repeat([]byte("abcdefgh"), n/8)
	target := append([]byte(nil), source...)
	copy(target[n/2:], []byte("XXXXXXXXXX"))

	d := Create(source, target)
	got, err := Apply(source, d)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(target))
	}
}

// BenchmarkCreate_Repetitive documents the cost of the pathological case at
// several sizes. Read the throughput column: before the collision-chain
// bound cost grew ~3.9x per doubling (quadratic, so MB/s halved each step);
// with the bound it grows 2.0x per doubling and throughput holds flat at
// ~21 MB/s.
//
//	32 KiB   1.58 ms  20.73 MB/s
//	64 KiB   3.14 ms  20.85 MB/s
//	128 KiB  6.22 ms  21.07 MB/s
func BenchmarkCreate_Repetitive(b *testing.B) {
	for _, kib := range []int{32, 64, 128} {
		b.Run(fmt.Sprintf("%dKiB", kib), func(b *testing.B) {
			n := kib * 1024
			source := bytes.Repeat([]byte("abcdefgh"), n/8)
			target := append([]byte(nil), source...)
			copy(target[n/2:], []byte("XXXXXXXXXX"))

			b.SetBytes(int64(n))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				Create(source, target)
			}
		})
	}
}
