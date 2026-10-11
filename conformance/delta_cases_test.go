package conformance

import "strings"

// Checksums of the targets below, in the draft's base-64 digits, followed by
// ";" in the deltas that use them. Each was computed by hand from Section
// 3.4 and is checked by fossil(1) and go-libfossil on every run.
const (
	ckA      = "110000"
	ckAB     = "11GW00"
	ckBA     = "12GG00"
	ckab     = "1XOW00"
	ckabc    = "1XObC0"
	ckababab = "32m65Y"
	ckSep    = "wG3dA"  // ";@:\n"
	ckFF80   = "3~W000" // 0xFF 0x80
	ckFF8    = "3~~~~z" // eight 0xFF bytes: the sum wraps modulo 2^32
	ckx64    = "27XtU0" // 64 times "x"
	ckx65    = "3~XtU0" // 65 times "x"
)

// The worked example of Appendix B.1.
const (
	b1Source = "The quick brown fox jumps over the lazy dog. A Fossil delta " +
		"encodes byte-level differences compactly.\n"
	b1Target = "The quick brown fox jumps over the sleeping fox. A Fossil delta " +
		"encodes byte-level differences compactly.\n An example delta target " +
		"with appended tail.\n"
	b1Delta = "2N\nZ@0,C:sleeping foxw@g,i: An example delta target with " +
		"appended tail.\n2P4rrX;"
)

func formatCases() []deltaCase {
	return []deltaCase{
		applyOK("delta.B1.worked", "DF-3-1", b1Source, b1Delta, b1Target),
		applyOK("delta.B4.empty", "DF-3-1", "", "0\n0;", ""),
		applyOK("delta.B4.empty-any-source", "DF-3-1", "abc", "0\n0;", ""),
		applyBad("delta.4.reject-01", "DF-3.2-1", "", "1:a;", "expected newline"),
		applyBad("delta.3.2.space", "DF-3.2-1", "", "1 \n1:A"+ckA+";", "expected newline"),
		applyBad("delta.3.2.crlf", "DF-3.2-1", "", "1\r\n1:A"+ckA+";", "expected newline"),
	}
}

// integerCases are the compatibility forms of Section 3.1: an empty field
// and leading zeros are read, or rejected, by choice of the decoder.
// go-libfossil rejects an empty field ("expected integer") and accepts
// leading zeros; fossil(1) accepts both.
func integerCases() []deltaCase {
	return []deltaCase{
		applyMay("delta.3.1.empty-header", "DF-3.1-4", "", "\n0;", ""),
		applyMay("delta.3.1.empty-count", "DF-3.1-4", "", "1\n:1:A"+ckA+";", "A"),
		applyMay("delta.3.1.empty-offset", "DF-3.1-4", "abc", "3\n3@,"+ckabc+";", "abc"),
		applyMay("delta.3.1.empty-checksum", "DF-3.1-4", "", "0\n;", ""),
		sizeMay("delta.4.1.empty-integer", "DF-3.1-4", "\n", 0),
		applyMay("delta.3.1.leading-zero-header", "DF-3.1-5", "", "01\n1:A"+ckA+";", "A"),
		applyMay("delta.3.1.leading-zero-count", "DF-3.1-5", "", "1\n01:A"+ckA+";", "A"),
		applyMay("delta.3.1.leading-zero-checksum", "DF-3.1-5", "", "1\n1:A0"+ckA+";", "A"),
		sizeMay("delta.4.1.leading-zero", "DF-3.1-5", "0002N\n", 151),
		sizeOK("delta.3.1.max-value", "DF-3.1-2", "3~~~~~\n", 1<<32-1).differs(
			"fossil keeps the size in a signed int, so delta_output_size reports every " +
				"header of 2^31 or more as corrupt; the draft allows values below 2^32"),
	}
}

func segmentCases() []deltaCase {
	return []deltaCase{
		applyOK("delta.3.3.1.copy-tail", "DF-3.3.1-1", "xyzAB", "2\n2@3,"+ckAB+";", "AB"),
		applyOK("delta.4.reordered-copies", "DF-3.3.1-1", "AB", "2\n1@1,1@0,"+ckBA+";", "BA"),
		applyOK("delta.4.repeated-copy", "DF-3.3.1-1", "ab", "6\n2@0,2@0,2@0,"+ckababab+";",
			"ababab"),
		applyBad("delta.4.reject-03", "DF-3.3.1-2", "abcdef", "3\n3@0;", "expected comma"),
		applyBad("delta.4.copy-truncated", "DF-4-2", "abc", "3\n3@0", "unexpected end"),
		applyOK("delta.3.3.1.zero-copy", "DF-3.3.1-3", "abc", "0\n0@3,0;", ""),
		// Appendix A: a zero count copies nothing. A decoder that read it as
		// "the rest of the source" would write 3 bytes into a 0-byte target.
		applyOK("delta.A.zero-copy-not-rest", "DF-3.3.1-3", "abc", "0\n0@0,0;", ""),
		applyOK("delta.4.single-literal", "DF-3.3.2-1", "AB", "2\n2:AB"+ckAB+";", "AB"),
		applyOK("delta.3.3.2.zero-literal", "DF-3.3.2-1", "", "0\n0:0;", ""),
		applyOK("delta.3.3.2.payload-separators", "DF-3.3.2-2", "", "4\n4:;@:\n"+ckSep+";",
			";@:\n"),
		applyOK("delta.3.3.2.payload-nul", "DF-3.3.2-2", "", "1\n1:\x00"+"0;", "\x00"),
		applyOK("delta.3.3.2.payload-high-bit", "DF-3.3.2-2", "", "2\n2:\xff\x80"+ckFF80+";",
			"\xff\x80"),
	}
}

func trailerCases() []deltaCase {
	return []deltaCase{
		applyOK("delta.3.4.wrap", "DF-3.4-2", "", "8\n8:"+strings.Repeat("\xff", 8)+ckFF8+";",
			strings.Repeat("\xff", 8)),
		// go-libfossil verifies the checksum, which the draft allows but
		// does not require; fossil(1) does not, and accepts "A".
		applyBad("delta.4.reject-08", "DF-3.4-3", "", "1\n1:A0;", "").checksumFailure().differs(
			"fossil never verifies the checksum, which Section 3.4 permits; " +
				"go-libfossil verifies, so the verifying branch applies"),
		applyOK("delta.3.4.trailing", "DF-3.4-4", "", "0\n0;ignored", ""),
		applyOK("delta.3.4.trailing-segment", "DF-3.4-4", "", "1\n1:A"+ckA+";1:B", "A"),
	}
}

func applyCases() []deltaCase {
	var all []deltaCase
	for _, table := range [][]deltaCase{
		rejectBoundsCases(), rejectStructureCases(), wrapCases(), freedomCases(),
		bufferEndCases(),
	} {
		all = append(all, table...)
	}
	return all
}

func rejectBoundsCases() []deltaCase {
	return []deltaCase{
		applyBad("delta.4.empty-input", "DF-4-1", "abc", "", "empty delta").notSent(
			"fossil's delta_apply crashes (SIGSEGV) on a zero-length delta"),
		applyBad("delta.4.reject-02", "DF-4-2", "", "5\n9", "unexpected end"),
		applyBad("delta.4.reject-02-header", "DF-4-2", "", "2N", "unexpected end"),
		applyBad("delta.4.reject-04", "DF-4-3", "abcdef", "2\n3@0,"+ckabc+";", "during copy"),
		applyBad("delta.4.reject-05", "DF-4-4", "abc", "5\n5@0,0;", "copy offset/count overflow"),
		applyBad("delta.4.copy-offset", "DF-4-4", "abc", "1\n1@3,0;", "source bounds"),
		applyBad("delta.4.copy-offset-zero-past", "DF-4-4", "abc", "0\n0@4,0;",
			"copy offset/count overflow"),
		// 2^32-1 + 2 wraps to 1 in 32-bit arithmetic, inside the source.
		applyBad("delta.4.copy-overflow-32", "DF-4-5", "abc", "2\n2@3~~~~~,"+ckab+";",
			"copy offset/count overflow"),
		// 2^66-1 does not fit in 64 bits: a decoder that rejects the integer
		// itself is applying its checks to a value of 2^32 or more.
		applyBad("delta.4.copy-overflow-64", "DF-4-11", "abc", "2\n2@~~~~~~~~~~~,"+ckab+";",
			"integer overflow"),
		applyBad("delta.4.reject-06", "DF-4-6", "", "2\n3:abc"+ckab+";", "during insert"),
		applyBad("delta.4.reject-07", "DF-4-7", "", "9\n5:ab", "insert exceeds delta"),
		applyBad("delta.4.reject-09", "DF-4-8", "", "5\n1:A"+ckA+";", "!= target size"),
	}
}

func rejectStructureCases() []deltaCase {
	return []deltaCase{
		applyBad("delta.4.reject-10", "DF-4-9", "abc", "3\n3!", "unknown command"),
		applyBad("delta.4.reject-10-comma", "DF-4-9", "abc", "3\n3,", "unknown command"),
		// Only the header's LF is structural; a LF between segments is an
		// unknown opcode.
		applyBad("delta.4.reject-10-lf", "DF-4-9", "", "1\n1\nA"+ckA+";", "unknown command"),
		// A non-empty count, so the NUL is judged as an opcode. fossil stops
		// at a NUL and reports an unterminated delta; same verdict, other road.
		applyBad("delta.4.reject-10-nul", "DF-4-9", "", "1\n1\x00A"+ckA+";", "unknown command"),
		applyBad("delta.4.reject-11", "DF-4-10", "abc", "3\n3@0,", "missing terminator"),
		applyBad("delta.4.reject-11-literal", "DF-4-10", "", "1\n1:A", "missing terminator"),
	}
}

// wrapCases give a decoder that accepts a value of 2^32 or more, or an
// aliasing checksum, a delta it must still check. go-libfossil uses the full
// value and so rejects all three; fossil(1) wraps to 32 bits and accepts.
func wrapCases() []deltaCase {
	return []deltaCase{
		applyMay("delta.3.1.header-2^32", "DF-4-11", "", "400000\n0;", ""),
		applyMay("delta.3.1.header-2^32+1", "DF-4-11", "", "400001\n1:A"+ckA+";", "A"),
		// ck("A") + 2^32: the same value modulo 2^32.
		applyMay("delta.3.1.checksum-2^32-alias", "DF-4-11", "", "1\n1:A510000;",
			"A").checksumFailure(),
	}
}

// freedomCases: Section 4 does not tie a decoder to one segmentation.
func freedomCases() []deltaCase {
	return []deltaCase{
		applyOK("delta.4.copy", "DF-4-12", "AB", "2\n2@0,"+ckAB+";", "AB"),
		applyOK("delta.4.split-literal", "DF-4-12", "AB", "2\n1:A1:B"+ckAB+";", "AB"),
		applyOK("delta.4.zero-segments", "DF-4-12", "AB", "2\n0:1:A0@2,1:B"+ckAB+";", "AB"),
	}
}

// highBitCases: bytes 0x80-0xFF in integer position end the integer. fossil
// masks them to 7 bits and reads them as digits, which Section 6 forbids.
func highBitCases() []deltaCase {
	const masks = "fossil masks the high bit and reads 0xB0 as '0'; Section 6 forbids that"
	return []deltaCase{
		applyBad("delta.6.high-bit-no-mask", "DF-6-4", "",
			"1\xb0\n1\xb0:"+strings.Repeat("x", 64)+ckx64+";", "expected newline").differs(masks),
		// 0xB0 ends the integer 11 and is then an unknown opcode.
		applyBad("delta.6.high-bit-checksum", "DF-6-4", "", "1\n1:A11\xb0"+"000;",
			"unknown command").differs(masks),
		// The count 1 is followed by 0xB1, which masked is '1': fossil reads
		// the count 11 (65), copies 65 bytes and accepts. A conforming decoder
		// ends the integer at 0xB1, an unknown opcode. libfossil's and zit's
		// vector for this ("3\n\xb0@0,") also fails to end in a trailer under
		// masking, so it cannot tell the two apart.
		applyBad("delta.6.high-bit-count", "DF-6-4", strings.Repeat("x", 65),
			"11\n1\xb1@0,"+ckx65+";", "unknown command").differs(masks),
	}
}

func sizeQueryCases() []deltaCase {
	return []deltaCase{
		sizeOK("delta.4.1.header-only", "DF-4.1-1", "2N\n", 151),
		sizeOK("delta.4.1.zero", "DF-4.1-1", "0\n", 0),
		sizeOK("delta.4.1.ignore-tail", "DF-4.1-1", "2N\n!!!", 151),
		sizeBad("delta.4.1.empty", "DF-4.1-2", "", "empty delta").notSent(
			"fossil's delta_output_size crashes (SIGSEGV) on a zero-length delta"),
		sizeBad("delta.4.1.space", "DF-4.1-3", "5 \n", "expected newline"),
		// The LF is in the backing array but outside the buffer.
		sizeBad("delta.4.1.no-lf", "DF-4.1-3", "2N\n", "unexpected end").truncated(2),
	}
}

// bufferEndCases give the decoder a prefix of a valid delta whose remaining
// bytes are still in the backing array: a decoder that reads past the end of
// its buffer finds what completes the delta, and wrongly accepts it.
func bufferEndCases() []deltaCase {
	return []deltaCase{
		applyBad("delta.6.trailer-boundary", "DF-6-1", "", "0\n0;", "unexpected end").truncated(3),
		applyBad("delta.6.literal-boundary", "DF-6-1", "", "1\n1:A"+ckA+";",
			"insert exceeds delta").truncated(4),
		applyBad("delta.6.integer-boundary", "DF-6-1", "", "1\n1:A"+ckA+";",
			"unexpected end").truncated(9),
	}
}
