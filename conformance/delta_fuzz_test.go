package conformance

import (
	"bytes"
	"testing"

	"github.com/danmestas/go-libfossil/internal/delta"
)

// nonNil copies b, as an empty non-nil slice when b is empty: Apply treats a
// nil source as a caller bug.
func nonNil(b []byte) []byte { return append([]byte{}, b...) }

// FuzzDeltaApply checks Apply against the model decoder for any source and
// delta: no panic; success gives exactly the declared size, never more; the
// output is what the model gives, which reads only inside the source;
// failure returns nothing; and only a delta in a form Section 3.1 leaves to
// the decoder may be rejected when the model accepts it.
func FuzzDeltaApply(f *testing.F) {
	for _, c := range deltaTableCases() {
		f.Add([]byte(c.source), []byte(c.delta))
	}
	f.Add([]byte(b1Source), []byte(b1Delta))
	f.Fuzz(func(t *testing.T, src, d []byte) {
		src, d = nonNil(src), nonNil(d)
		out, err := delta.Apply(src, d)
		want, ok, optional := modelApply(src, d)
		if err != nil {
			if out != nil {
				t.Fatalf("failed Apply returned %q", out)
			}
			if ok && !optional {
				t.Fatalf("Apply rejects %q on %q, which Section 4 accepts: %v", d, src, err)
			}
			return
		}
		if !ok || !bytes.Equal(out, want) {
			t.Fatalf("Apply(%q, %q) = %q; the model says %q (accepted=%v)", src, d, out, want, ok)
		}
		if size, sizeErr := delta.OutputSize(d); sizeErr != nil || size != uint64(len(out)) {
			t.Fatalf("Apply gave %d bytes; OutputSize says %d (%v)", len(out), size, sizeErr)
		}
	})
}

// FuzzDeltaRoundTrip checks Section 5 for any source and target: Create's
// delta passes the producer oracle, and Apply gives the target back.
func FuzzDeltaRoundTrip(f *testing.F) {
	for _, p := range boundaryPairs() {
		f.Add(p.src, p.tgt)
	}
	f.Fuzz(func(t *testing.T, src, tgt []byte) {
		d := delta.Create(src, tgt)
		if finding := producerFinding(src, tgt, d); finding != nil {
			t.Fatalf("Create(%q, %q) = %q: %s", src, tgt, d, finding.msg)
		}
		got, err := delta.Apply(src, d)
		if err != nil || !bytes.Equal(got, tgt) {
			t.Fatalf("Apply(%q, Create(%q, %q)) = %q, %v", src, src, tgt, got, err)
		}
	})
}
