package annotate

// A port of the line-diff engine fossil's annotate uses (src/diff.c:
// break_into_lines, diff_all, diff_step, longestCommonSequence, optimalLCS,
// likelyNotIndentChngArtifact, appendTriple), with the line flags annotate
// passes by default (DIFF_STRIP_EOLCR). Annotation depends on which of
// several equally long alignments the diff picks when lines repeat, so this
// follows fossil step for step, including the line hash: the hash orders
// the candidates longestCommonSequence tries. Any change here can change
// which version a repeated line is credited to.

import (
	"bytes"
	"encoding/binary"
)

const (
	lengthBits = 15                  // diff.c LENGTH_MASK_SZ
	lengthMask = 1<<lengthBits - 1   // longest line fossil will diff, in bytes
	hashMul    = 9000000000000000041 // diff.c multiplier
	hashMod    = 281474976710597     // diff.c modulus
)

// The hash keeps a line's length in its low bits, so a longer line cannot
// be represented.
var _ = [1]struct{}{}[lengthMask-32767]

// dline is a line prepared for the diff: diff.c's DLine.
type dline struct {
	text   string // the line without its newline or a trailing CR
	h      uint64 // hash in the high bits, len(text) in the low lengthBits
	iNext  int    // 1 + next line in this line's hash chain; 0 ends it
	iHash  int    // 1 + first line of the chain for bucket i; 0 is empty
	indent int    // leading whitespace, computed with nw on demand
	nw     int    // width without surrounding whitespace; 0 = not computed
}

// isSpace is diff.c's diffIsSpace table: backspace, tab, newline, form
// feed, carriage return and space (not vertical tab).
func isSpace(c byte) bool {
	switch c {
	case '\b', '\t', '\n', '\f', '\r', ' ':
		return true
	default:
		return false
	}
}

// breakIntoLines splits data into lines and hashes them, as fossil's
// break_into_lines does. A final newline ends the last line rather than
// starting an empty one, so "b\n\n" is two lines. ok is false for content
// fossil will not diff: one holding a NUL byte, or a line of more than
// lengthMask bytes.
func breakIntoLines(data []byte) (lines []dline, ok bool) {
	// Fossil's annotate reads each version through blob_to_utf8_no_bom.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	n := 0
	for _, c := range data {
		if c == 0 {
			return nil, false
		}
		if c == '\n' {
			n++
		}
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	lines = make([]dline, n)
	start := 0
	for i := range lines {
		end := start
		for end < len(data) && data[end] != '\n' {
			end++
		}
		if end-start > lengthMask {
			return nil, false
		}
		k := end
		if k > start && data[k-1] == '\r' {
			k--
		}
		lines[i].text = string(data[start:k])
		lines[i].h = lineHash(data[start:k])
		bucket := lines[i].h % uint64(n)
		lines[i].iNext = lines[bucket].iHash
		lines[bucket].iHash = i + 1
		start = end + 1
	}
	return lines, true
}

// lineHash is break_into_lines' hash of one line: eight bytes at a time,
// read little-endian as fossil's memcpy into a u64 does on the platforms it
// runs on.
func lineHash(b []byte) uint64 {
	if len(b) > lengthMask {
		panic("annotate.lineHash: line too long")
	}
	var h uint64
	full := len(b) &^ 7
	for x := 0; x < full; x += 8 {
		h = (h ^ binary.LittleEndian.Uint64(b[x:x+8])) * hashMul
	}
	var tail [8]byte
	copy(tail[:], b[full:])
	h ^= binary.LittleEndian.Uint64(tail[:])
	return (h%hashMod)<<lengthBits | uint64(len(b))
}

// differ reports whether two lines are different: compare_dline.
func differ(a, b *dline) bool {
	return a.h != b.h || a.text != b.text
}

// diffContext is diff.c's DContext for one pair of files.
type diffContext struct {
	from, to []dline
	edit     []int // COPY, DELETE, INSERT triples
}

// diffAll computes the edit script from from to to: diff_all. The result
// is a list of (copy, delete, insert) line counts ending in 0, 0, 0.
func diffAll(from, to []dline) []int {
	p := &diffContext{from: from, to: to}
	e1, e2 := len(from), len(to)
	for e1 > 0 && e2 > 0 && !differ(&from[e1-1], &to[e2-1]) {
		e1--
		e2--
	}
	s := 0
	for s < min(e1, e2) && !differ(&from[s], &to[s]) {
		s++
	}
	if s > 0 {
		p.appendTriple(s, 0, 0)
	}
	p.diffStep(s, e1, s, e2)
	if e1 < len(from) {
		p.appendTriple(len(from)-e1, 0, 0)
	}
	return append(p.edit, 0, 0, 0)
}

// stepWork is one item of diffStep's work list: a range pair to diff, or a
// run of copied lines to append between two of them.
type stepWork struct {
	s1, e1, s2, e2 int
	copyRun        int // > 0: append a copy of this many lines instead
}

// diffStep is diff_step with its recursion replaced by a work list, popped
// in the order the recursion appends: the left range, the common run, then
// the right range.
func (p *diffContext) diffStep(s1, e1, s2, e2 int) {
	work := []stepWork{{s1: s1, e1: e1, s2: s2, e2: e2}}
	for len(work) > 0 {
		w := work[len(work)-1]
		work = work[:len(work)-1]
		if w.copyRun > 0 {
			p.appendTriple(w.copyRun, 0, 0)
			continue
		}
		if w.e1 <= w.s1 {
			if w.e2 > w.s2 {
				p.appendTriple(0, 0, w.e2-w.s2)
			}
			continue
		}
		if w.e2 <= w.s2 {
			p.appendTriple(0, w.e1-w.s1, 0)
			continue
		}
		sx, ex, sy, ey := p.longestCommonSequence(w.s1, w.e1, w.s2, w.e2)
		if ex > sx+5 || (ex > sx && p.likelyNotIndentArtifact(w.s1, sx, ex, w.e1)) {
			// Pushed in reverse: left range first, then the run, then right.
			work = append(work,
				stepWork{s1: ex, e1: w.e1, s2: ey, e2: w.e2},
				stepWork{copyRun: ex - sx},
				stepWork{s1: w.s1, e1: sx, s2: w.s2, e2: sy},
			)
			continue
		}
		p.appendTriple(0, w.e1-w.s1, w.e2-w.s2)
	}
}

// appendTriple appends a COPY/DELETE/INSERT triple, folding it into the
// previous one where fossil's appendTriple does.
func (p *diffContext) appendTriple(nCopy, nDel, nIns int) {
	if nCopy < 0 || nDel < 0 || nIns < 0 {
		panic("annotate.appendTriple: negative count")
	}
	if n := len(p.edit); n >= 3 {
		if p.edit[n-1] == 0 {
			if p.edit[n-2] == 0 {
				p.edit[n-3] += nCopy
				p.edit[n-2] += nDel
				p.edit[n-1] += nIns
				return
			}
			if nCopy == 0 {
				p.edit[n-2] += nDel
				p.edit[n-1] += nIns
				return
			}
		}
		if nCopy == 0 && nDel == 0 {
			p.edit[n-1] += nIns
			return
		}
	}
	p.edit = append(p.edit, nCopy, nDel, nIns)
}

// commonRun is a candidate common run: from[sx:ex] equals to[sy:ey].
type commonRun struct{ sx, ex, sy, ey int }

// longestCommonSequence finds a long run of lines common to from[s1:e1] and
// to[s2:e2], preferring one near the middle: diff.c's hashing heuristic,
// falling back to optimalLCS for small inputs it finds nothing in.
func (p *diffContext) longestCommonSequence(s1, e1, s2, e2 int) (sx, ex, sy, ey int) {
	span := int64(e1-s1) + int64(e2-s2)
	mid := (e1 + s1) / 2
	best := commonRun{s1, s1, s2, s2}
	prev := best
	bestScore := int64(-9223300000) * 1000000000
	cutoff := 4
	for {
		nextCutoff := 0
		for i := s1; i < e1; i++ {
			j, more := p.chainMatch(i, s2, e2, cutoff)
			if more {
				nextCutoff = cutoff * 4
			}
			if j == 0 {
				continue
			}
			if i < best.ex && j >= best.sy && j < best.ey {
				continue
			}
			if i < prev.ex && j >= prev.sy && j < prev.ey {
				continue
			}
			m := p.extendMatch(i, j-1, s1, e1, s2, e2)
			skew := abs((m.sx - s1) - (m.sy - s2))
			dist := abs((m.sx+m.ex)/2 - mid)
			score := int64(m.ex-m.sx)*span - int64(skew+dist)
			if score > bestScore {
				bestScore = score
				best = m
			} else if m.ex > prev.ex {
				prev = m
			}
		}
		if best.sx != best.ex || nextCutoff == 0 {
			break
		}
		cutoff = nextCutoff
		if cutoff > 64 {
			break
		}
	}
	if best.sx == best.ex && int64(e1-s1)*int64(e2-s2) < 2500 {
		return p.optimalLCS(s1, e1, s2, e2)
	}
	return best.sx, best.ex, best.sy, best.ey
}

// chainMatch follows to's hash chain for from[i] and returns 1 + the index
// of the first line in to[s2:e2] equal to it, or 0. more is true when the
// chain was cut off before it ended.
func (p *diffContext) chainMatch(i, s2, e2, cutoff int) (j int, more bool) {
	j = p.to[p.from[i].h%uint64(len(p.to))].iHash
	limit := 0
	for j > 0 && (j-1 < s2 || j >= e2 || differ(&p.from[i], &p.to[j-1])) {
		if limit > cutoff {
			return 0, true
		}
		limit++
		j = p.to[j-1].iNext
	}
	return j, false
}

// extendMatch grows the match of from[i] and to[j] backward and forward
// within the two ranges.
func (p *diffContext) extendMatch(i, j, s1, e1, s2, e2 int) commonRun {
	if i < s1 || j < s2 {
		panic("annotate.extendMatch: match outside its ranges")
	}
	m := commonRun{sx: i, sy: j, ex: i + 1, ey: j + 1}
	back := min(m.sx-s1, m.sy-s2)
	k := 0
	for k < back && !differ(&p.from[m.sx-1-k], &p.to[m.sy-1-k]) {
		k++
	}
	m.sx -= k
	m.sy -= k
	forward := min(e1-m.ex, e2-m.ey)
	k = 0
	for k < forward && !differ(&p.from[m.ex+k], &p.to[m.ey+k]) {
		k++
	}
	m.ex += k
	m.ey += k
	return m
}

// optimalLCS is the exhaustive search fossil uses on small inputs where the
// hashing heuristic found no common run.
func (p *diffContext) optimalLCS(s1, e1, s2, e2 int) (sx, ex, sy, ey int) {
	longest, bx, by := 0, s1, s2
	for i := s1; i < e1-longest; i++ {
		for j := s2; j < e2-longest; j++ {
			if differ(&p.from[i], &p.to[j]) {
				continue
			}
			if longest > 0 && differ(&p.from[i+longest], &p.to[j+longest]) {
				continue
			}
			k := 1
			for i+k < e1 && j+k < e2 && !differ(&p.from[i+k], &p.to[j+k]) {
				k++
			}
			if k > longest {
				bx, by, longest = i, j, k
			}
		}
	}
	return bx, bx + longest, by, by + longest
}

// likelyNotIndentArtifact is likelyNotIndentChngArtifact: whether the
// common run from[sx:ex] within from[s1:e1] is a real match rather than
// lines that only look alike after an indentation change.
func (p *diffContext) likelyNotIndentArtifact(s1, sx, ex, e1 int) bool {
	if (ex-sx)*7 >= e1-s1 {
		return true
	}
	wide := 0
	for i := sx; i < ex; i++ {
		p.from[i].measure()
		if p.from[i].nw > 1 {
			wide++
		}
	}
	if wide == 0 {
		return false
	}
	for i := s1; i < e1; i++ {
		if i == sx {
			i = ex
			if i >= e1 {
				break
			}
		}
		p.from[i].measure()
	}
	for i := sx; i < ex; i++ {
		if p.from[i].nw > 1 && !p.repeatsOutside(i, s1, sx, ex, e1) {
			return true
		}
	}
	return false
}

// repeatsOutside reports whether from[i], ignoring surrounding whitespace,
// also appears in from[s1:sx] or from[ex:e1].
func (p *diffContext) repeatsOutside(i, s1, sx, ex, e1 int) bool {
	core := p.from[i].core()
	for j := s1; j < sx; j++ {
		if p.from[j].nw == p.from[i].nw && p.from[j].core() == core {
			return true
		}
	}
	for j := ex; j < e1; j++ {
		if p.from[j].nw == p.from[i].nw && p.from[j].core() == core {
			return true
		}
	}
	return false
}

// measure computes indent and nw once, as fossil does lazily.
func (d *dline) measure() {
	if d.nw != 0 || d.text == "" {
		return
	}
	n := len(d.text)
	ii := 0
	for ii < n && isSpace(d.text[ii]) {
		ii++
	}
	jj := n - 1
	for jj > ii && isSpace(d.text[jj]) {
		jj--
	}
	d.indent = ii
	d.nw = jj - ii + 1
}

// core is the measured line without its surrounding whitespace.
func (d *dline) core() string {
	if d.nw <= 0 {
		return ""
	}
	return d.text[d.indent : d.indent+d.nw]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
