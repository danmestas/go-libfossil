package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// deltaPair is a source and a target for the producer cases.
type deltaPair struct{ src, tgt []byte }

// deltaBytes is n seeded bytes drawn from alphabet, or from all 256 values
// when alphabet is empty. A two-letter alphabet makes long matches likely.
func deltaBytes(r *rand.Rand, n int, alphabet string) []byte {
	b := make([]byte, n)
	for i := range b {
		if alphabet == "" {
			b[i] = byte(r.Intn(256))
		} else {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
	}
	return b
}

// deltaMutate returns a copy of src with up to four flips, deletions or
// insertions.
func deltaMutate(r *rand.Rand, src []byte) []byte {
	out := append([]byte{}, src...)
	for edits := r.Intn(4); edits >= 0; edits-- {
		if len(out) == 0 {
			out = append(out, byte(r.Intn(256)))
			continue
		}
		at := r.Intn(len(out))
		switch r.Intn(3) {
		case 0:
			out[at] ^= byte(1 + r.Intn(255))
		case 1:
			out = append(out[:at], out[at+r.Intn(len(out)-at):]...)
		default:
			insert := deltaBytes(r, r.Intn(40), "")
			out = append(out[:at], append(insert, out[at:]...)...)
		}
	}
	return out
}

// The sizes straddle the producer's 16-byte match window and its hash table.
var boundarySizes = []int{0, 1, 15, 16, 17, 31, 32, 33, 64, 100, 1000}

// boundaryPairs: every pair of sizes, the target either unrelated to the
// source or a mutation of it.
var boundaryPairs = sync.OnceValue(func() []deltaPair {
	r := rand.New(rand.NewSource(0x5eed))
	var pairs []deltaPair
	for _, a := range boundarySizes {
		for _, b := range boundarySizes {
			src := deltaBytes(r, a, "ab")
			pairs = append(pairs, deltaPair{src, deltaBytes(r, b, "ab")})
			pairs = append(pairs, deltaPair{src, deltaMutate(r, src)})
		}
	}
	return pairs
})

// propertyPairs: 400 random sources up to 3000 bytes, each with a mutation.
var propertyPairs = sync.OnceValue(func() []deltaPair {
	r := rand.New(rand.NewSource(0x7e57))
	var pairs []deltaPair
	for i := 0; i < 400; i++ {
		src := deltaBytes(r, r.Intn(3000), "")
		pairs = append(pairs, deltaPair{src, deltaMutate(r, src)})
	}
	return pairs
})

func allPairs() []deltaPair {
	return append(slices.Clone(boundaryPairs()), propertyPairs()...)
}

// interopPairs are the pairs given to fossil(1): non-empty on both sides,
// because fossil's encoder is not specified for an empty input.
func interopPairs() []deltaPair {
	var pairs []deltaPair
	for _, p := range propertyPairs() {
		if len(p.src) > 0 && len(p.tgt) > 0 && len(pairs) < 40 {
			pairs = append(pairs, p)
		}
	}
	return pairs
}

func producerCases() []deltaFuncCase {
	findings := func(id, req string, kinds ...string) deltaFuncCase {
		return deltaFuncCase{Case{ID: id, Req: req}, func(*testing.T, *fossilReference) error {
			return noFindings(allPairs(), kinds...)
		}}
	}
	return []deltaFuncCase{
		{Case{ID: "delta.5.boundaries", Req: "DF-3-2"}, roundTrip(boundaryPairs)},
		{Case{ID: "delta.5.properties", Req: "DF-3-2"}, roundTrip(propertyPairs)},
		{Case{ID: "delta.5.interop-fossil-apply", Req: "DF-3-2"}, fossilAppliesCreate},
		{Case{ID: "delta.5.interop-go-apply", Req: "DF-3-2"}, goAppliesFossilCreate},
		findings("delta.3.1.create-canonical", "DF-3.1-1", findInteger, findRange),
		findings("delta.3.3.1.create-no-zero-copy", "DF-3.3.1-4", findZeroCopy),
		findings("delta.3.4.create-trailer", "DF-3.4-1", findTrailer),
		findings("delta.3.create-grammar", "DF-3-1", findGrammar),
		{Case{ID: "delta.B4.generated-empty", Req: "DF-3.4-1"}, createEmptyTarget},
	}
}

// noFindings runs Create over the pairs and reports the first finding of
// one of the given kinds. DF-3.1-6 (no integer of 2^32 or more) has no case
// of its own: only inputs of 4 GiB can break it, and the range check here is
// reachable only through DF-3.1-1.
func noFindings(pairs []deltaPair, kinds ...string) error {
	for _, p := range pairs {
		d, err := delta.Create(p.src, p.tgt)
		if err != nil {
			return err
		}
		f := producerFinding(p.src, p.tgt, d)
		if f != nil && slices.Contains(kinds, f.kind) {
			return expectf("Create(%d bytes, %d bytes) = %q: %s", len(p.src), len(p.tgt), d, f.msg)
		}
	}
	return nil
}

// roundTrip is Section 5: applying what Create wrote to the source gives
// the target.
func roundTrip(pairs func() []deltaPair) func(*testing.T, *fossilReference) error {
	return func(*testing.T, *fossilReference) error {
		for _, p := range pairs() {
			created, err := delta.Create(p.src, p.tgt)
			if err != nil {
				return err
			}
			got, err := delta.Apply(p.src, created)
			if err != nil {
				return expectf("Apply(Create(%d bytes, %d bytes)): %v", len(p.src), len(p.tgt), err)
			}
			if !bytes.Equal(got, p.tgt) {
				return expectf("Apply(Create(%d bytes, %d bytes)) is not the target",
					len(p.src), len(p.tgt))
			}
		}
		return nil
	}
}

// fossilAppliesCreate: fossil applies what go-libfossil's encoder wrote.
// The bytes need not match fossil's own encoder's.
func fossilAppliesCreate(_ *testing.T, ref *fossilReference) error {
	if ref == nil {
		return nil
	}
	for _, p := range interopPairs() {
		d, err := delta.Create(p.src, p.tgt)
		if err != nil {
			return err
		}
		got, ok, err := ref.applyDelta(p.src, d)
		if err != nil {
			return err
		}
		if !ok || !bytes.Equal(got, p.tgt) {
			return expectf("fossil delta_apply of Create(%d bytes, %d bytes): ok=%v, "+
				"target reproduced=%v", len(p.src), len(p.tgt), ok, bytes.Equal(got, p.tgt))
		}
	}
	return nil
}

// goAppliesFossilCreate: go-libfossil applies what fossil's encoder wrote,
// and that is a delta the strict oracle accepts too.
func goAppliesFossilCreate(_ *testing.T, ref *fossilReference) error {
	if ref == nil {
		return nil
	}
	for _, p := range interopPairs() {
		d, err := ref.createDelta(p.src, p.tgt)
		if err != nil {
			return err
		}
		got, err := delta.Apply(p.src, d)
		if err != nil || !bytes.Equal(got, p.tgt) {
			return expectf("Apply of fossil's delta_create(%d bytes, %d bytes): %v",
				len(p.src), len(p.tgt), err)
		}
	}
	return nil
}

// createEmptyTarget: Section 3.4 has a trailer even for an empty target.
// go-libfossil writes "0\n0;"; fossil writes "0\n0:0;", a zero-length
// literal the draft allows, so the case asserts the grammar, not the bytes.
func createEmptyTarget(*testing.T, *fossilReference) error {
	for _, src := range []string{"", "abc"} {
		d, err := delta.Create([]byte(src), []byte{})
		if err != nil {
			return err
		}
		if f := producerFinding([]byte(src), []byte{}, d); f != nil {
			return expectf("Create(%q, empty) = %q: %s", src, d, f.msg)
		}
	}
	return nil
}

func generatedCases() []deltaFuncCase {
	return []deltaFuncCase{
		{Case{ID: "delta.3.1.values", Req: "DF-3.1-2"}, integerValues},
		{Case{ID: "delta.3.1.terminators", Req: "DF-3.1-3"}, integerTerminators},
		{Case{ID: "delta.3.4.checksum-tails", Req: "DF-3.4-2"}, checksumTails},
		{Case{ID: "delta.3.4.B3", Req: "DF-3.4-2"}, checksumExample},
		{Case{ID: "delta.3.1.max-target-apply", Req: "DF-6-5"}, hugeHeaderIsCheap},
		{Case{ID: "delta.6.high-bit-header", Req: "DF-6-4"}, highBitHeaders},
		{Case{ID: "delta.6.no-false-success", Req: "DF-6-2"}, noFalseSuccess},
		{Case{ID: "delta.6.applyinto-discard", Req: "DF-6-3"}, applyIntoDiscards},
	}
}

// integerValues: each of the 64 digits alone, the examples of Appendix B.2,
// and multi-digit values, read most significant digit first.
func integerValues(_ *testing.T, ref *fossilReference) error {
	vectors := map[string]uint64{"10": 64, "~~": 4095, "2N": 151}
	for i := 0; i < len(deltaDigits); i++ {
		vectors[deltaDigits[i:i+1]] = uint64(i)
	}
	for text, value := range vectors {
		c := sizeOK("delta.3.1.values", "DF-3.1-2", text+"\n", value)
		if err := c.conforms(ref); err != nil {
			return expectf("%q: %v", text, err)
		}
	}
	return nil
}

// integerTerminators: the first byte that is not a digit ends an integer. In
// a header only LF can follow, so every other byte below 0x80 must fail the
// query. 0x80-0xFF are in highBitHeaders.
func integerTerminators(_ *testing.T, ref *fossilReference) error {
	for b := 0; b < 0x80; b++ {
		var c deltaCase
		switch {
		case b == '\n':
			c = sizeOK("delta.3.1.terminators", "DF-3.1-3", "1\n\n", 1)
		case strings.IndexByte(deltaDigits, byte(b)) >= 0:
			continue
		default:
			c = sizeBad("delta.3.1.terminators", "DF-3.1-3", "1"+string([]byte{byte(b)})+"\n",
				"expected newline")
		}
		if err := c.conforms(ref); err != nil {
			return expectf("byte %#02x after the digit: %v", b, err)
		}
	}
	return nil
}

// highBitHeaders: the bytes 0x80-0xFF end an integer; they are never read as
// digits. fossil masks them, so delta_output_size("1\xb0\n") is 64 there;
// the check pins that disagreement for the one byte whose mask is a digit.
func highBitHeaders(_ *testing.T, ref *fossilReference) error {
	for b := 0x80; b <= 0xff; b++ {
		d := []byte{'1', byte(b), '\n'}
		n, err := delta.OutputSize(d)
		if err == nil || !strings.Contains(err.Error(), "expected newline") {
			return expectf("OutputSize(%q) = %d, %v; want failure at the header's LF", d, n, err)
		}
		out, err := delta.Apply([]byte{}, append(d, '0', ';'))
		if out != nil || err == nil || !strings.Contains(err.Error(), "expected newline") {
			return expectf("Apply with header %q returned (%q, %v); want failure at the LF",
				d, out, err)
		}
	}
	if ref == nil {
		return nil
	}
	n, ok, err := ref.deltaSize([]byte("1\xb0\n"))
	if err != nil {
		return err
	}
	if !ok || n != 64 {
		return expectf("fossil no longer masks the high bit (size %d, ok=%v); "+
			"the high-bit cases need re-reading", n, ok)
	}
	return nil
}

// checksumTails: Section 3.4 for every length mod 4, with seeded content.
func checksumTails(*testing.T, *fossilReference) error {
	r := rand.New(rand.NewSource(0xc5))
	for n := 0; n < 260; n++ {
		data := deltaBytes(r, n, "")
		if got, want := delta.Checksum(data), oracleChecksum(data); got != want {
			return expectf("Checksum of %d bytes = %#x, want %#x", n, got, want)
		}
	}
	return nil
}

// checksumExample: Appendix B.3, the checksum of the 151-byte B.1 target.
func checksumExample(*testing.T, *fossilReference) error {
	return want("Checksum(B.1 target)", delta.Checksum([]byte(b1Target)), uint32(0x99136DA1))
}

// hugeHeaderIsCheap: a header that declares 2^32-1 bytes, over a delta with
// none, is rejected without allocating in proportion to the header (DF-6-5,
// a SHOULD of Section 6). The least of five runs is compared with a margin
// of 1 MiB, against the 4 GiB a decoder trusting the header would take, so a
// busy machine's other allocations cannot trip it.
func hugeHeaderIsCheap(_ *testing.T, ref *fossilReference) error {
	d := []byte("3~~~~~\n0;")
	least := uint64(1 << 62)
	for run := 0; run < 5; run++ {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		out, err := delta.Apply([]byte{}, d)
		runtime.ReadMemStats(&after)
		if err == nil || out != nil || !strings.Contains(err.Error(), "!= target size") {
			return expectf("Apply(%q) = (%q, %v), want the length mismatch", d, out, err)
		}
		least = min(least, after.TotalAlloc-before.TotalAlloc)
	}
	if least > 1<<20 {
		return expectf("rejecting a delta of %d bytes allocated %d bytes", len(d), least)
	}
	if ref == nil {
		return nil
	}
	if _, ok, err := ref.applyDelta([]byte{}, d); err != nil || ok {
		return expectf("fossil(1) applies %q (ok=%v, err=%v)", d, ok, err)
	}
	return nil
}

// noFalseSuccess: seeded damage to 400 valid deltas. An Apply that succeeds
// gives the declared size and the bytes the model gives; one that fails
// returns nothing, and was entitled to fail.
func noFalseSuccess(*testing.T, *fossilReference) error {
	r := rand.New(rand.NewSource(0xbad))
	for _, p := range propertyPairs() {
		created, err := delta.Create(p.src, p.tgt)
		if err != nil {
			return err
		}
		d := damage(r, created)
		out, err := delta.Apply(p.src, d)
		want, ok, optional := modelApply(p.src, d)
		switch {
		case err == nil && !ok:
			return expectf("Apply accepted %q, which Section 4 rejects", d)
		case err == nil && !bytes.Equal(out, want):
			return expectf("Apply(%q) gave %q, want %q", d, out, want)
		case err == nil:
			if n, sizeErr := delta.OutputSize(d); sizeErr != nil || n != uint64(len(out)) {
				return expectf("Apply(%q) gave %d bytes, OutputSize says %d (%v)",
					d, len(out), n, sizeErr)
			}
		case out != nil:
			return expectf("Apply(%q) failed yet returned %q", d, out)
		case ok && !optional:
			return expectf("Apply rejected %q, which Section 4 accepts: %v", d, err)
		}
	}
	return nil
}

// damage flips, inserts, deletes or truncates one place in d.
func damage(r *rand.Rand, d []byte) []byte {
	out := append([]byte(nil), d...)
	at := r.Intn(len(out))
	switch r.Intn(4) {
	case 0:
		out[at] ^= byte(1 + r.Intn(255))
	case 1:
		out = append(out[:at], append([]byte{byte(r.Intn(256))}, out[at:]...)...)
	case 2:
		out = append(out[:at], out[at+1:]...)
	default:
		out = out[:at]
	}
	return out
}

// applyIntoDiscards: a failed ApplyInto hands back no buffer, though the
// caller gave it room; a successful one may share that room.
func applyIntoDiscards(*testing.T, *fossilReference) error {
	room := make([]byte, 0, 64)
	out, err := delta.ApplyInto(room, []byte("abc"), []byte("3\n3@0,0;"))
	if out != nil || !errors.Is(err, delta.ErrChecksum) {
		return expectf("ApplyInto with a bad checksum = (%q, %v), want ErrChecksum", out, err)
	}
	out, err = delta.ApplyInto(room, []byte("abc"), []byte("3\n3@0,"+ckabc+";"))
	if err != nil || string(out) != "abc" {
		return expectf("ApplyInto of a good delta = (%q, %v)", out, err)
	}
	return nil
}

func callerCases() []deltaFuncCase {
	return []deltaFuncCase{
		{Case{ID: "delta.4.2.absent-source", Req: "DF-4.2-1"}, absentSource},
		{Case{ID: "delta.6.content-discards", Req: "DF-6-3"}, contentDiscards},
		{Case{ID: "delta.4.1.store-refuses", Req: "DF-4.1-3"}, storeRefusesBadHeaders},
	}
}

// absentSource: a stored delta whose base is only a phantom, a name with no
// content, is not applied. The decoder is never reached; the loader refuses
// the phantom first.
func absentSource(t *testing.T, _ *fossilReference) error {
	r := newPolicyRepo(t, "2")
	base := []byte("original content, long enough to compress well enough")
	target := append(append([]byte(nil), base...), " and then changed"...)
	baseRid, err := blob.StorePhantom(r.DB(), hash.SHA1(base))
	if err != nil {
		return fmt.Errorf("StorePhantom: %v", err)
	}
	// Built against an empty source, the delta is all literals: it needs
	// nothing from the base, so only the phantom can make Expand fail.
	stored, err := delta.Create([]byte{}, target)
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	rid, err := blob.StoreDeltaRaw(r.DB(), hash.SHA1(target), stored, baseRid, nil)
	if err != nil {
		return fmt.Errorf("StoreDeltaRaw: %v", err)
	}
	out, err := content.Expand(r.DB(), rid)
	if err == nil || out != nil || !strings.Contains(err.Error(), "phantom") {
		return expectf("Expand of a delta on a phantom = (%q, %v), want a phantom error", out, err)
	}
	return nil
}

// contentDiscards: Expand over a stored delta whose trailer checksum is
// wrong returns nothing but the checksum error.
func contentDiscards(t *testing.T, _ *fossilReference) error {
	r := newPolicyRepo(t, "2")
	base := []byte("fresh original content, long enough to compress well")
	target := append(append([]byte(nil), base...), " and modified"...)
	baseRid, _, err := blob.Store(r.DB(), base)
	if err != nil {
		return fmt.Errorf("Store: %v", err)
	}
	good, err := delta.Create(base, target)
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	bad := append([]byte(nil), good...)
	last := strings.IndexByte(deltaDigits, bad[len(bad)-2])
	bad[len(bad)-2] = deltaDigits[(last+1)%64]
	rid, err := blob.StoreDeltaRaw(r.DB(), hash.SHA1(target), bad, baseRid, nil)
	if err != nil {
		return fmt.Errorf("StoreDeltaRaw: %v", err)
	}
	out, err := content.Expand(r.DB(), rid)
	if out != nil || !errors.Is(err, delta.ErrChecksum) {
		return expectf("Expand of a delta with a wrong checksum = (%q, %v), want ErrChecksum",
			out, err)
	}
	return nil
}

// storeRefusesBadHeaders: the store reads each delta's size query before
// keeping it, so a header that does not end in LF, or an empty delta, never
// reaches the decoder.
func storeRefusesBadHeaders(t *testing.T, _ *fossilReference) error {
	r := newPolicyRepo(t, "2")
	baseRid, _, err := blob.Store(r.DB(), []byte("a base"))
	if err != nil {
		return fmt.Errorf("Store: %v", err)
	}
	name := hash.SHA1([]byte("x"))
	refusals := map[string]string{"1 \n1:x0;": "expected newline", "": "empty delta"}
	for refused, reason := range refusals {
		_, err := blob.StoreDeltaRaw(r.DB(), name, []byte(refused), baseRid, nil)
		if !errors.Is(err, delta.ErrInvalidDelta) || !strings.Contains(err.Error(), reason) {
			return expectf("StoreDeltaRaw of %q = %v, want a %q refusal", refused, err, reason)
		}
	}
	return nil
}
