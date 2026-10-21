package merge

import (
	"bytes"
	"encoding/binary"
)

// This file ports the parts of fossil's diff.c that merge3.c uses: text_diff
// without output, by lines or by tokens, producing copy/delete/insert
// triples. The merge must align its inputs exactly as fossil does, or its
// conflict regions, and so its output, differ from fossil's. Quirks of the C
// code (the hash that skips a token's first byte, the character classes that
// make '9' punctuation) are kept on purpose.

// lengthMaskSz and lengthMask: the low bits of a dline hash hold its length.
// A line longer than lengthMask makes the file binary.
const (
	lengthMaskSz = 15
	lengthMask   = 1<<lengthMaskSz - 1
)

// dline is one line (or token) of a file being diffed. The slice also serves
// as a hash table: iHash is 1+ the first entry of the chain whose bucket is
// this index, iNext 1+ the next entry with the same bucket.
type dline struct {
	z      []byte // the line's bytes, without its '\n'
	h      uint64 // hash, with the length in the low lengthMaskSz bits
	indent uint16 // index of the first non-space, computed lazily
	n      uint16 // number of bytes (truncated as fossil's unsigned short)
	nw     uint16 // bytes without leading/trailing space, computed lazily
	iNext  uint32
	iHash  uint32
}

// dcontext is fossil's DContext: the two files and the edit triples.
type dcontext struct {
	edit  []int
	from  []dline // len nFrom+1: a zero sentinel follows the lines
	to    []dline
	nFrom int
	nTo   int
}

// isDiffSpace is fossil's diffIsSpace table: backspace counts, vertical tab
// does not.
func isDiffSpace(c byte) bool {
	switch c {
	case '\b', '\t', '\n', '\f', '\r', ' ':
		return true
	}
	return false
}

// tokenClass is fossil's aTCharClass: 1 alphanumeric, 2 whitespace,
// 3 punctuation.
var tokenClass = [256]byte{
	2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	2, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 3, 3, 3, 3, 3, 3, 3,
	3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 3, 3, 3, 3, 3,
	3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 3, 3, 3, 3, 3,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
}

// textDiff is fossil's text_diff with no output and flags 0 (or
// DIFF_BY_TOKEN): it converts *a and *b to UTF-8 without a BOM, in place as
// fossil does, and returns the copy/delete/insert triples ending in 0,0,0.
// By token, the counts are bytes. ok is false when either file is binary.
func textDiff(a, b *[]byte, byToken bool) (edit []int, ok bool) {
	if a == nil || b == nil {
		panic("merge.textDiff: nil input")
	}
	*a = toUTF8NoBOM(*a)
	*b = toUTF8NoBOM(*b)
	var c dcontext
	if byToken {
		c.from, c.nFrom = breakIntoTokens(*a)
		c.to, c.nTo = breakIntoTokens(*b)
	} else {
		var okA, okB bool
		c.from, c.nFrom, okA = breakIntoLines(*a)
		c.to, c.nTo, okB = breakIntoLines(*b)
		if !okA || !okB {
			return nil, false
		}
	}
	c.diffAll()
	c.optimize()
	if byToken {
		c.tokensToBytes()
	}
	return c.edit, true
}

// lineHash is break_into_lines' hash: eight bytes at a time, little-endian,
// as fossil computes it on the little-endian machines it ships for.
func lineHash(z []byte) uint64 {
	k := len(z)
	k2 := k &^ 7
	var h uint64
	for x := 0; x < k2; x += 8 {
		h = (h ^ binary.LittleEndian.Uint64(z[x:])) * 9000000000000000041
	}
	var m uint64
	for i := k - 1; i >= k2; i-- {
		m = m<<8 | uint64(z[i])
	}
	h ^= m
	return (h%281474976710597)<<lengthMaskSz | uint64(k)
}

// breakIntoLines is fossil's break_into_lines. ok is false for binary input:
// a NUL byte or a line longer than lengthMask.
func breakIntoLines(z []byte) (lines []dline, n int, ok bool) {
	if bytes.IndexByte(z, 0) >= 0 {
		return nil, 0, false
	}
	n = bytes.Count(z, []byte{'\n'})
	if len(z) > 0 && z[len(z)-1] != '\n' {
		n++
	}
	lines = make([]dline, n+1)
	start := 0
	for i := 0; i < n; i++ {
		end := len(z)
		if nl := bytes.IndexByte(z[start:], '\n'); nl >= 0 {
			end = start + nl
		}
		if end-start > lengthMask {
			return nil, 0, false
		}
		lines[i].z = z[start:end]
		lines[i].n = uint16(end - start)
		lines[i].h = lineHash(lines[i].z)
		bucket := lines[i].h % uint64(n)
		lines[i].iNext = lines[bucket].iHash
		lines[bucket].iHash = uint32(i + 1)
		start = end + 1
	}
	return lines, n, true
}

// breakIntoTokens is fossil's break_into_tokens: runs of one character
// class. Its hash leaves out each token's first byte, as fossil's does.
func breakIntoTokens(z []byte) (tokens []dline, n int) {
	var prev byte
	for _, c := range z {
		if tokenClass[c] != prev {
			prev = tokenClass[c]
			n++
		}
	}
	tokens = make([]dline, n+1)
	for i, start := 0, 0; start < len(z); i++ {
		class := tokenClass[z[start]]
		h := uint64(0xcbf29ce484222325)
		k := 1
		for ; start+k < len(z) && tokenClass[z[start+k]] == class; k++ {
			h ^= uint64(z[start+k])
			h *= 0x100000001b3
		}
		tokens[i].z = z[start : start+k]
		tokens[i].n = uint16(k)
		tokens[i].h = (h%281474976710597)<<lengthMaskSz | uint64(k)
		bucket := tokens[i].h % uint64(n)
		tokens[i].iNext = tokens[bucket].iHash
		tokens[bucket].iHash = uint32(i + 1)
		start += k
	}
	return tokens, n
}

// differ is fossil's compare_dline: true when the two lines differ.
func differ(a, b *dline) bool {
	if a.h != b.h {
		return true
	}
	m := int(a.h & lengthMask)
	return !bytes.Equal(a.z[:m], b.z[:m])
}

// tokensToBytes converts token counts in the edit triples to byte counts.
func (c *dcontext) tokensToBytes() {
	iA, iB := 0, 0
	for i := 0; c.edit[i] != 0 || c.edit[i+1] != 0 || c.edit[i+2] != 0; i += 3 {
		sum := 0
		for k := 0; k < c.edit[i]; k++ {
			sum += int(c.from[iA].n)
			iA++
		}
		iB += c.edit[i]
		c.edit[i] = sum
		sum = 0
		for k := 0; k < c.edit[i+1]; k++ {
			sum += int(c.from[iA].n)
			iA++
		}
		c.edit[i+1] = sum
		sum = 0
		for k := 0; k < c.edit[i+2]; k++ {
			sum += int(c.to[iB].n)
			iB++
		}
		c.edit[i+2] = sum
	}
}

// appendTriple is fossil's appendTriple, which folds a triple into the last
// one where it can.
func (c *dcontext) appendTriple(nCopy, nDel, nIns int) {
	if n := len(c.edit); n >= 3 {
		if c.edit[n-1] == 0 {
			if c.edit[n-2] == 0 {
				c.edit[n-3] += nCopy
				c.edit[n-2] += nDel
				c.edit[n-1] += nIns
				return
			}
			if nCopy == 0 {
				c.edit[n-2] += nDel
				c.edit[n-1] += nIns
				return
			}
		}
		if nCopy == 0 && nDel == 0 {
			c.edit[n-1] += nIns
			return
		}
	}
	c.edit = append(c.edit, nCopy, nDel, nIns)
}

// diffAll is fossil's diff_all: carve off the common head and tail, diff the
// middle, and terminate the triples with 0,0,0.
func (c *dcontext) diffAll() {
	iE1, iE2 := c.nFrom, c.nTo
	for iE1 > 0 && iE2 > 0 && !differ(&c.from[iE1-1], &c.to[iE2-1]) {
		iE1--
		iE2--
	}
	mnE := min(iE1, iE2)
	iS := 0
	for iS < mnE && !differ(&c.from[iS], &c.to[iS]) {
		iS++
	}
	if iS > 0 {
		c.appendTriple(iS, 0, 0)
	}
	c.diffSteps(iS, iE1, iS, iE2)
	if iE1 < c.nFrom {
		c.appendTriple(c.nFrom-iE1, 0, 0)
	}
	c.edit = append(c.edit, 0, 0, 0)
}

// diffTask is one pending piece of fossil's recursive diff_step: diff a
// range, or emit a copy of the common block between two ranges.
type diffTask struct {
	copyN              int
	iS1, iE1, iS2, iE2 int
}

// diffSteps is fossil's diff_step with the recursion replaced by a stack:
// the left range, the common block, then the right range, in that order.
func (c *dcontext) diffSteps(iS1, iE1, iS2, iE2 int) {
	stack := []diffTask{{iS1: iS1, iE1: iE1, iS2: iS2, iE2: iE2}}
	for len(stack) > 0 {
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if t.copyN > 0 {
			c.appendTriple(t.copyN, 0, 0)
			continue
		}
		if t.iE1 <= t.iS1 {
			if t.iE2 > t.iS2 {
				c.appendTriple(0, 0, t.iE2-t.iS2)
			}
			continue
		}
		if t.iE2 <= t.iS2 {
			c.appendTriple(0, t.iE1-t.iS1, 0)
			continue
		}
		iSX, iEX, iSY, iEY := c.longestCommonSequence(t.iS1, t.iE1, t.iS2, t.iE2)
		if iEX > iSX+5 || (iEX > iSX && c.likelyNotIndentChngArtifact(t.iS1, iSX, iEX, t.iE1)) {
			stack = append(stack, diffTask{iS1: iEX, iE1: t.iE1, iS2: iEY, iE2: t.iE2})
			if iEX > iSX {
				stack = append(stack, diffTask{copyN: iEX - iSX})
			}
			stack = append(stack, diffTask{iS1: t.iS1, iE1: iSX, iS2: t.iS2, iE2: iSY})
		} else {
			c.appendTriple(0, t.iE1-t.iS1, t.iE2-t.iS2)
		}
	}
}

// optimize is fossil's diff_optimize: slide each pure insertion or deletion
// up or down while that keeps the same diff and moves its edges toward
// shorter lines. The asymmetric length test in the last loop is fossil's.
// The r+3 bounds stand in for reads fossil never makes on valid triples.
func (c *dcontext) optimize() {
	e := c.edit
	lnFrom, lnTo := 0, 0
	for r := 0; r < len(e); r += 3 {
		cpy, del, ins := e[r], e[r+1], e[r+2]
		lnFrom += cpy
		lnTo += cpy
		for cpy > 0 && del == 0 && ins > 0 && r+3 < len(e) {
			top, btm := &c.from[lnFrom-1], &c.to[lnTo+ins-1]
			if differ(top, btm) || int(c.from[lnFrom].n)+int(btm.n) <= int(top.n)+int(c.to[lnTo+ins-2].n) {
				break
			}
			lnFrom, lnTo, cpy = lnFrom-1, lnTo-1, cpy-1
			e[r]--
			e[r+3]++
		}
		for r+3 < len(e) && e[r+3] > 0 && del == 0 && ins > 0 {
			top, btm := &c.to[lnTo], &c.to[lnTo+ins]
			if differ(top, btm) || int(top.n)+int(c.to[lnTo+ins-1].n) <= int(c.to[lnTo+1].n)+int(btm.n) {
				break
			}
			lnFrom, lnTo, cpy = lnFrom+1, lnTo+1, cpy+1
			e[r]++
			e[r+3]--
		}
		for cpy > 0 && del > 0 && ins == 0 && r+3 < len(e) {
			top, btm := &c.from[lnFrom-1], &c.from[lnFrom+del-1]
			if differ(top, btm) || int(c.from[lnFrom].n)+int(btm.n) <= int(top.n)+int(c.from[lnFrom+del-2].n) {
				break
			}
			lnFrom, lnTo, cpy = lnFrom-1, lnTo-1, cpy-1
			e[r]--
			e[r+3]++
		}
		for r+3 < len(e) && e[r+3] > 0 && del > 0 && ins == 0 {
			top, btm := &c.from[lnFrom], &c.from[lnFrom+del]
			if differ(top, btm) || int(top.n)+int(c.from[lnFrom+del-1].n) <= int(top.n)+int(btm.n) {
				break
			}
			lnFrom, lnTo, cpy = lnFrom+1, lnTo+1, cpy+1
			e[r]++
			e[r+3]--
		}
		lnFrom += del
		lnTo += ins
	}
}
