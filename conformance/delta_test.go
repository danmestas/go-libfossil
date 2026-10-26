package conformance

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/delta"
)

// The delta suite checks draft-fossil-delta-format-00. Most cases are byte
// strings given to delta.Apply or delta.OutputSize, and every one is also
// given to fossil(1) through its SQL delta functions. Each rejection case
// breaks one rule and keeps the rest of the delta valid, trailer checksum
// included, so a decoder that checks in another order still rejects it for
// the reason under test.

type deltaOp int

const (
	opApply deltaOp = iota // delta.Apply(source, delta)
	opSize                 // delta.OutputSize(delta)
)

// deltaWant is what the draft requires of a decoder given the input.
type deltaWant int

const (
	wantOutput deltaWant = iota // accept, with exactly the case's output
	wantReject                  // reject
	wantEither                  // a MAY: reject, or accept with the case's output
)

// deltaCase is one input to a decoder and what the draft says about it.
type deltaCase struct {
	Case
	op     deltaOp
	source string
	delta  string
	// cut, when positive, gives the decoder only the first cut bytes of
	// delta; the rest stays in the backing array, where a decoder that
	// reads past its buffer would find it.
	cut  int
	want deltaWant
	// output is the applied bytes, or for a size query the decimal size.
	output string
	// sentinel is the error a rejection wraps; ErrInvalidDelta when nil.
	sentinel error
	// reason is a substring of go-libfossil's error that a rejection must
	// carry, so the case fails when another rule rejects the input. Every
	// rejection case sets it, except the MAY cases, whose rejection branch
	// is the decoder's choice. internal/delta has only ErrInvalidDelta and
	// ErrChecksum, so the message is the only way to tell the rules apart.
	reason string
	// fossil is set only when fossil(1) does not give the case's verdict;
	// it says why the case follows the draft and not fossil.
	fossil string
	// draftOnly is the reason the case is not sent to fossil(1).
	draftOnly string
}

func applyOK(id, req, src, d, out string) deltaCase {
	return deltaCase{Case: Case{ID: id, Req: req}, source: src, delta: d, output: out}
}

func applyBad(id, req, src, d, reason string) deltaCase {
	c := deltaCase{Case: Case{ID: id, Req: req}, source: src, delta: d, want: wantReject}
	c.reason = reason
	return c
}

// applyMay is a case the draft leaves to the decoder: reject, or accept
// and produce out.
func applyMay(id, req, src, d, out string) deltaCase {
	c := applyOK(id, req, src, d, out)
	c.want = wantEither
	return c
}

func sizeOK(id, req, d string, size uint64) deltaCase {
	c := applyOK(id, req, "", d, strconv.FormatUint(size, 10))
	c.op = opSize
	return c
}

func sizeBad(id, req, d, reason string) deltaCase {
	c := applyBad(id, req, "", d, reason)
	c.op = opSize
	return c
}

func sizeMay(id, req, d string, size uint64) deltaCase {
	c := sizeOK(id, req, d, size)
	c.want = wantEither
	return c
}

func (c deltaCase) truncated(n int) deltaCase { c.cut = n; return c }

func (c deltaCase) differs(why string) deltaCase { c.fossil = why; return c }

func (c deltaCase) notSent(why string) deltaCase { c.draftOnly = why; return c }

func (c deltaCase) checksumFailure() deltaCase { c.sentinel = delta.ErrChecksum; return c }

// deltaResult is what a decoder did with an input.
type deltaResult struct {
	ok  bool
	out string // applied bytes, or the decimal size
	err error  // go-libfossil's rejection
	// leaked is set when a failed Apply still returned a buffer.
	leaked bool
}

// input returns the buffers the decoders get.
func (c deltaCase) input() (src, d []byte) {
	src = []byte(c.source)
	d = append([]byte(nil), c.delta...)
	if c.cut > 0 {
		d = d[:c.cut]
	}
	return src, d
}

// runGo gives the case to go-libfossil. A panic is reported as the problem
// it is, not as a rejection.
func (c deltaCase) runGo() (res deltaResult, crash error) {
	defer func() {
		if r := recover(); r != nil {
			crash = expectf("panicked: %v", r)
		}
	}()
	src, d := c.input()
	if c.op == opSize {
		n, err := delta.OutputSize(d)
		if err != nil {
			return deltaResult{err: err}, nil
		}
		return deltaResult{ok: true, out: strconv.FormatUint(n, 10)}, nil
	}
	out, err := delta.Apply(src, d)
	if err != nil {
		return deltaResult{err: err, leaked: out != nil}, nil
	}
	return deltaResult{ok: true, out: string(out)}, nil
}

// runFossil gives the case to fossil(1).
func (c deltaCase) runFossil(ref *fossilReference) (deltaResult, error) {
	src, d := c.input()
	if c.op == opSize {
		n, ok, err := ref.deltaSize(d)
		return deltaResult{ok: ok, out: strconv.FormatUint(n, 10)}, err
	}
	out, ok, err := ref.applyDelta(src, d)
	return deltaResult{ok: ok, out: string(out)}, err
}

// satisfies reports whether res is a verdict the draft allows for the case.
func (c deltaCase) satisfies(res deltaResult) bool {
	switch c.want {
	case wantOutput:
		return res.ok && res.out == c.output
	case wantReject:
		return !res.ok
	default:
		return !res.ok || res.out == c.output
	}
}

func (c deltaCase) describe(res deltaResult) string {
	if res.ok {
		return fmt.Sprintf("accepted, giving %q", res.out)
	}
	return "rejected"
}

// conforms runs one case through fossil(1), when present, and go-libfossil.
// fossil goes first: a case whose expectation fossil contradicts without
// saying so is wrong whatever go-libfossil does.
func (c deltaCase) conforms(ref *fossilReference) error {
	if ref != nil && c.draftOnly == "" {
		if err := c.fossilAgrees(ref); err != nil {
			return err
		}
	}
	res, crash := c.runGo()
	if crash != nil {
		return crash
	}
	if !c.satisfies(res) {
		return expectf("go-libfossil %s; the draft wants %s", c.describe(res), c.wanted())
	}
	if res.ok {
		return nil
	}
	if res.leaked {
		return expectf("a rejected delta still returned a buffer")
	}
	sentinel := c.sentinel
	if sentinel == nil {
		sentinel = delta.ErrInvalidDelta
	}
	if !errors.Is(res.err, sentinel) {
		return expectf("rejection %q does not wrap %v", res.err, sentinel)
	}
	if c.want == wantReject && c.sentinel == nil && c.reason == "" {
		panic("conformance: rejection case " + c.ID + " names no reason")
	}
	if !strings.Contains(res.err.Error(), c.reason) {
		return expectf("rejected for another reason: %q does not mention %q", res.err, c.reason)
	}
	return nil
}

func (c deltaCase) wanted() string {
	switch c.want {
	case wantOutput:
		return fmt.Sprintf("acceptance giving %q", c.output)
	case wantReject:
		return "rejection"
	default:
		return fmt.Sprintf("rejection or acceptance giving %q", c.output)
	}
}

// fossilAgrees checks fossil's verdict: allowed by the case, or, when the
// case says why it departs from fossil, not allowed.
func (c deltaCase) fossilAgrees(ref *fossilReference) error {
	res, err := c.runFossil(ref)
	if err != nil {
		return err
	}
	allowed := c.satisfies(res)
	switch {
	case c.fossil != "" && allowed:
		return expectf("fossil(1) now agrees with the case (%s); remove fossil: %q",
			c.describe(res), c.fossil)
	case c.fossil == "" && !allowed:
		return expectf("fossil(1) %s; the case wants %s", c.describe(res), c.wanted())
	}
	return nil
}

// deltaFuncCase is a case that is a procedure, not one input: generated
// vectors, the producer, and the callers of the decoder.
type deltaFuncCase struct {
	Case
	run func(t *testing.T, ref *fossilReference) error
}

// deltaGaps marks the delta cases go-libfossil is known to fail, by case ID.
var deltaGaps = map[string]string{}

func deltaTableCases() []deltaCase {
	var all []deltaCase
	for _, table := range [][]deltaCase{
		formatCases(), integerCases(), segmentCases(), trailerCases(),
		applyCases(), highBitCases(), sizeQueryCases(),
	} {
		all = append(all, table...)
	}
	for i := range all {
		all[i].Gap = deltaGaps[all[i].ID]
	}
	return all
}

func deltaFuncCases() []deltaFuncCase {
	var all []deltaFuncCase
	for _, table := range [][]deltaFuncCase{
		producerCases(), generatedCases(), callerCases(),
	} {
		all = append(all, table...)
	}
	for i := range all {
		all[i].Gap = deltaGaps[all[i].ID]
	}
	return all
}

// deltaSuiteCases is what the coverage test audits for this suite.
func deltaSuiteCases() []Case {
	var all []Case
	for _, c := range deltaTableCases() {
		all = append(all, c.Case)
	}
	for _, c := range deltaFuncCases() {
		all = append(all, c.Case)
	}
	return all
}

func TestDelta(t *testing.T) {
	ref := newFossilReference(t)
	for _, c := range deltaTableCases() {
		check(t, c.Case, func(t *testing.T) error { return c.conforms(ref) })
	}
}

func TestDeltaProcedures(t *testing.T) {
	ref := newFossilReference(t)
	for _, c := range deltaFuncCases() {
		check(t, c.Case, func(t *testing.T) error { return c.run(t, ref) })
	}
}

// TestDeltaGapKeys checks that every marked case exists.
func TestDeltaGapKeys(t *testing.T) {
	ids := map[string]bool{}
	for _, c := range deltaSuiteCases() {
		ids[c.ID] = true
	}
	for id := range deltaGaps {
		if !ids[id] {
			t.Errorf("deltaGaps names %s, which is not a case", id)
		}
	}
}
