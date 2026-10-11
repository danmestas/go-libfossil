package conformance

import (
	"bytes"
	"strings"
)

// Two decoders written from the draft alone, independent of internal/delta:
// producerFinding checks what a producer emitted, strictly, and modelApply
// is the reference the fuzz test compares Apply with.

const deltaDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz~"

// oracleChecksum is Section 3.4: the sum, modulo 2^32, of the data's
// big-endian 32-bit words, a short tail padded with zeros.
func oracleChecksum(data []byte) uint32 {
	var sum uint32
	for i, b := range data {
		sum += uint32(b) << (24 - 8*(i%4))
	}
	return sum
}

// Kinds of finding, each tied to the requirement it breaks.
const (
	findInteger  = "integer"   // DF-3.1-1: empty, or a leading zero
	findRange    = "range"     // DF-3.1-6: 2^32 or more
	findZeroCopy = "zero-copy" // DF-3.3.1-4
	findTrailer  = "trailer"   // DF-3.4-1
	findGrammar  = "grammar"   // DF-3-1, or the segments do not rebuild the target
)

type finding struct {
	kind string
	msg  string
}

// oracleReader walks a delta byte by byte.
type oracleReader struct {
	d   []byte
	pos int
}

// integer reads one integer in the form a producer must write.
func (r *oracleReader) integer() (uint64, *finding) {
	start := r.pos
	var v uint64
	for r.pos < len(r.d) {
		digit := strings.IndexByte(deltaDigits, r.d[r.pos])
		if digit < 0 {
			break
		}
		v = v*64 + uint64(digit)
		if v >= 1<<32 {
			return 0, &finding{findRange, "an integer of 2^32 or more"}
		}
		r.pos++
	}
	switch {
	case r.pos == start:
		return 0, &finding{findInteger, "an empty integer"}
	case r.pos-start > 1 && r.d[start] == '0':
		return 0, &finding{findInteger, "an integer with a leading zero"}
	}
	return v, nil
}

// next returns the byte at the cursor and moves past it; 0, false at the end.
func (r *oracleReader) next() (byte, bool) {
	if r.pos >= len(r.d) {
		return 0, false
	}
	r.pos++
	return r.d[r.pos-1], true
}

// producerFinding returns what is wrong with d as a delta from src to tgt,
// or nil. It reports the first problem only.
func producerFinding(src, tgt, d []byte) *finding {
	r := &oracleReader{d: d}
	size, f := r.integer()
	if f != nil {
		return f
	}
	if lf, _ := r.next(); lf != '\n' || size != uint64(len(tgt)) {
		return &finding{findGrammar, "the header is not the target size and LF"}
	}
	out := 0
	for r.pos < len(r.d) {
		done, f := r.segment(src, tgt, &out)
		if f != nil || done {
			return f
		}
	}
	return &finding{findTrailer, "no trailer"}
}

// segment checks the next segment, advancing *out by what it contributes.
// done is true once the trailer has been checked.
func (r *oracleReader) segment(src, tgt []byte, out *int) (done bool, f *finding) {
	count, f := r.integer()
	if f != nil {
		return false, f
	}
	op, ok := r.next()
	if !ok {
		return false, &finding{findGrammar, "an integer with no opcode"}
	}
	switch op {
	case ';':
		return true, r.trailer(tgt, count, *out)
	case '@':
		return false, r.copySegment(src, tgt, count, out)
	case ':':
		return false, r.literal(tgt, count, out)
	}
	return false, &finding{findGrammar, "an unknown opcode"}
}

func (r *oracleReader) trailer(tgt []byte, sum uint64, out int) *finding {
	switch {
	case r.pos != len(r.d):
		return &finding{findTrailer, "bytes after the trailer"}
	case out != len(tgt):
		return &finding{findGrammar, "the segments do not fill the target"}
	case sum != uint64(oracleChecksum(tgt)):
		return &finding{findTrailer, "the trailer is not the target's checksum"}
	}
	return nil
}

func (r *oracleReader) copySegment(src, tgt []byte, count uint64, out *int) *finding {
	if count == 0 {
		return &finding{findZeroCopy, "a copy segment with count zero"}
	}
	offset, f := r.integer()
	if f != nil {
		return f
	}
	if comma, _ := r.next(); comma != ',' {
		return &finding{findGrammar, "a copy offset without a comma"}
	}
	if offset+count > uint64(len(src)) || *out+int(count) > len(tgt) {
		return &finding{findGrammar, "a copy outside the source or the target"}
	}
	if !bytes.Equal(src[offset:offset+count], tgt[*out:*out+int(count)]) {
		return &finding{findGrammar, "a copy that does not match the target"}
	}
	*out += int(count)
	return nil
}

func (r *oracleReader) literal(tgt []byte, count uint64, out *int) *finding {
	if count > uint64(len(r.d)-r.pos) || *out+int(count) > len(tgt) {
		return &finding{findGrammar, "a literal outside the delta or the target"}
	}
	payload := r.d[r.pos : r.pos+int(count)]
	if !bytes.Equal(payload, tgt[*out:*out+int(count)]) {
		return &finding{findGrammar, "a literal that does not match the target"}
	}
	r.pos += int(count)
	*out += int(count)
	return nil
}

// lenientReader reads integers the way Section 3.1 lets a decoder: empty
// fields as zero, leading zeros by their value, values of 2^32 or more as
// they stand. optional is set when it used one of those forms.
type lenientReader struct {
	d        []byte
	pos      int
	optional bool
}

func (r *lenientReader) integer() uint64 {
	start := r.pos
	var v uint64
	for r.pos < len(r.d) {
		digit := strings.IndexByte(deltaDigits, r.d[r.pos])
		if digit < 0 {
			break
		}
		if v < 1<<56 { // saturate: far above any length a test can hold
			v = v*64 + uint64(digit)
		}
		r.pos++
	}
	if r.pos == start || (r.pos-start > 1 && r.d[start] == '0') || v >= 1<<32 {
		r.optional = true
	}
	return v
}

// modelApply is Section 4 written out for a verifying decoder. optional
// reports that the delta used a form the draft leaves to the decoder, so a
// decoder may reject what the model accepts.
func modelApply(src, d []byte) (out []byte, ok, optional bool) {
	r := &lenientReader{d: d}
	size := r.integer()
	if r.pos >= len(d) || d[r.pos] != '\n' {
		return nil, false, r.optional
	}
	r.pos++
	for r.pos < len(d) {
		count := r.integer()
		if r.pos >= len(d) {
			return nil, false, r.optional
		}
		op := d[r.pos]
		r.pos++
		var done bool
		out, done, ok = r.modelSegment(src, out, op, count, size)
		if !ok {
			return nil, false, r.optional
		}
		if done {
			return out, true, r.optional
		}
	}
	return nil, false, r.optional
}

// modelSegment applies one segment to out. done is true after the trailer.
func (r *lenientReader) modelSegment(src, out []byte, op byte, count, size uint64,
) (_ []byte, done, ok bool) {
	switch op {
	case ';':
		good := uint64(len(out)) == size && count == uint64(oracleChecksum(out))
		return out, good, good
	case '@':
		offset := r.integer()
		if r.pos >= len(r.d) || r.d[r.pos] != ',' {
			return out, false, false
		}
		r.pos++
		inSource := count <= uint64(len(src)) && offset <= uint64(len(src))-count
		if !inSource || count > size-uint64(len(out)) || uint64(len(out)) > size {
			return out, false, false
		}
		return append(out, src[offset:offset+count]...), false, true
	case ':':
		if count > uint64(len(r.d)-r.pos) || uint64(len(out)) > size ||
			count > size-uint64(len(out)) {
			return out, false, false
		}
		out = append(out, r.d[r.pos:r.pos+int(count)]...)
		r.pos += int(count)
		return out, false, true
	}
	return out, false, false
}
