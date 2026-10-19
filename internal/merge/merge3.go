package merge

import (
	"bytes"
	"strconv"
)

// This file ports fossil's merge3.c: merge_three_blobs with the text
// MergeBuilder (conflict markers) and the token MergeBuilder (the suggested
// resolution inside a conflict). Fossil's walk calls the builder as it goes;
// here walkEdits plans the steps and each builder applies them, so the text
// builder can run a token merge without the walk re-entering itself.

// opKind names the MergeBuilder callback a step stands for.
type opKind int

const (
	opSame     opKind = iota // xSame: nPivot lines unchanged on both sides
	opChngV1                 // xChngV1: nPivot pivot lines become nV1 of V1
	opChngV2                 // xChngV2: nPivot pivot lines become nV2 of V2
	opChngBoth               // xChngBoth: both sides made the same change
	opConflict               // xConflict: overlapping different changes
)

// mergeOp is one MergeBuilder call, as the number of pivot, V1 and V2
// lines it passes over (bytes, by token).
type mergeOp struct {
	kind             opKind
	nPivot, nV1, nV2 int
}

// cursorBlob is a fossil Blob read through its cursor.
type cursorBlob struct {
	data []byte
	cur  int
}

// copyLines is fossil's blob_copy_lines: append n lines from the cursor to
// out (nil skips them) and advance past them. The last line may lack '\n'.
func (b *cursorBlob) copyLines(out *[]byte, n int) {
	if n == 0 {
		return
	}
	i, cnt := b.cur, 0
	for i < len(b.data) {
		if b.data[i] == '\n' {
			cnt++
			if cnt == n {
				i++
				break
			}
		}
		i++
	}
	if out != nil {
		*out = append(*out, b.data[b.cur:i]...)
	}
	b.cur = i
}

// extractLines is fossil's blob_extract_lines: the next n lines from the
// cursor, leaving the cursor where it is.
func (b *cursorBlob) extractLines(n int) []byte {
	i := b.cur
	for ; n > 0; n-- {
		for i < len(b.data) && b.data[i] != '\n' {
			i++
		}
		if i >= len(b.data) {
			break
		}
		i++
	}
	return b.data[b.cur:i]
}

// lineCount is fossil's blob_linecount: an unterminated last line counts.
func lineCount(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// lineStarts maps a line number to the byte offset where it starts, as
// blob_copy_lines would leave a cursor: past the end, the end.
func lineStarts(data []byte) func(int) int {
	starts := []int{0}
	for i, c := range data {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return func(line int) int {
		if line < len(starts) {
			return starts[line]
		}
		return len(data)
	}
}

// textMerger is fossil's MergeBuilderText.
type textMerger struct {
	pivot, v1, v2 cursorBlob
	out           []byte
	useCRLF       bool
	lnPivot       int
	lnV1          int
	lnV2          int
	conflicts     []Conflict
}

// mergeText is fossil's merge_three_blobs with the text builder, as
// merge_3way runs it: it returns the merged text, its conflicts, and the
// conflict count, which is negative when fossil cannot diff the inputs
// (binary content).
func mergeText(pivot, v1, v2 []byte) (out []byte, conflicts []Conflict, n int) {
	aC1, ok1 := textDiff(&pivot, &v1, false)
	aC2, ok2 := textDiff(&pivot, &v2, false)
	if !ok1 || !ok2 {
		return nil, nil, -1
	}
	ops, n := walkEdits(aC1, aC2, v1, v2, lineStarts(v1), lineStarts(v2))
	m := &textMerger{pivot: cursorBlob{data: pivot}, v1: cursorBlob{data: v1}, v2: cursorBlob{data: v2}}
	m.start()
	for _, op := range ops {
		m.apply(op)
	}
	if m.out == nil {
		m.out = []byte{}
	}
	return m.out, m.conflicts, n
}

// mergeTokens is fossil's merge_try_to_resolve_conflict after it has cut out
// the conflicting lines: merge them token by token, taking the merged-in
// text where tokens still conflict, and end the result with a newline.
func mergeTokens(pivot, v1, v2 []byte) []byte {
	aC1, ok1 := textDiff(&pivot, &v1, true)
	aC2, ok2 := textDiff(&pivot, &v2, true)
	if !ok1 || !ok2 {
		return nil
	}
	identity := func(i int) int { return i }
	ops, _ := walkEdits(aC1, aC2, v1, v2, identity, identity)
	var out []byte
	iPivot, iV1, iV2 := 0, 0, 0
	for _, op := range ops {
		switch op.kind {
		case opSame:
			out = append(out, pivot[iPivot:iPivot+op.nPivot]...)
		case opChngV1:
			out = append(out, v1[iV1:iV1+op.nV1]...)
		default:
			out = append(out, v2[iV2:iV2+op.nV2]...)
		}
		iPivot, iV1, iV2 = iPivot+op.nPivot, iV1+op.nV1, iV2+op.nV2
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out
}

// start is fossil's txtStart: keep a BOM both versions still start with
// (text_diff has already stripped one), and use CRLF markers when both
// versions' first line ends in CRLF.
func (m *textMerger) start() {
	if bytes.HasPrefix(m.v1.data, utf8BOM) && bytes.HasPrefix(m.v2.data, utf8BOM) {
		m.out = append(m.out, utf8BOM...)
	}
	m.useCRLF = containsCRLF(m.v1.data) && containsCRLF(m.v2.data)
}

// containsCRLF is fossil's contains_crlf. Its loop stops advancing at the
// first '\n' it finds from index 1, so in effect it asks whether that
// newline follows a '\r'.
func containsCRLF(z []byte) bool {
	if len(z) < 2 {
		return false
	}
	nl := bytes.IndexByte(z[1:], '\n')
	return nl >= 0 && z[nl] == '\r'
}

// apply is the text builder's callbacks: txtSame, txtChngV1, txtChngV2,
// txtChngBoth and txtConflict.
func (m *textMerger) apply(op mergeOp) {
	switch op.kind {
	case opSame:
		m.pivot.copyLines(&m.out, op.nPivot)
		m.v1.copyLines(nil, op.nV1)
		m.v2.copyLines(nil, op.nV2)
	case opChngV1:
		m.pivot.copyLines(nil, op.nPivot)
		m.v2.copyLines(nil, op.nV2)
		m.v1.copyLines(&m.out, op.nV1)
	case opChngV2, opChngBoth:
		m.pivot.copyLines(nil, op.nPivot)
		m.v1.copyLines(nil, op.nV1)
		m.v2.copyLines(&m.out, op.nV2)
	case opConflict:
		m.conflict(op.nPivot, op.nV1, op.nV2)
	}
	m.lnPivot += op.nPivot
	m.lnV1 += op.nV1
	m.lnV2 += op.nV2
}

// conflict is fossil's txtConflict, before its line counters advance: the
// local copy, the suggested resolution, the ancestor and the merged-in text
// between line-numbered markers.
func (m *textMerger) conflict(nPivot, nV1, nV2 int) {
	local := m.v1.extractLines(nV1)
	base := m.pivot.extractLines(nPivot)
	remote := m.v2.extractLines(nV2)
	m.conflicts = append(m.conflicts, Conflict{
		StartLine: m.lnPivot + 1,
		EndLine:   m.lnPivot + nPivot,
		Local:     bytes.Clone(local),
		Remote:    bytes.Clone(remote),
		Base:      bytes.Clone(base),
	})
	res := mergeTokens(base, local, remote)
	m.appendMark(markerBegin, m.lnV1+1)
	m.v1.copyLines(&m.out, nV1)
	if nRes := lineCount(res); nRes > 0 {
		m.appendMark(markerSuggested, 0)
		resBlob := cursorBlob{data: res}
		resBlob.copyLines(&m.out, nRes)
	}
	m.appendMark(markerAncestor, m.lnPivot+1)
	m.pivot.copyLines(&m.out, nPivot)
	m.appendMark(markerMergedIn, m.lnV2+1)
	m.v2.copyLines(&m.out, nV2)
	m.appendMark(markerEnd, 0)
}

// appendMark is fossil's append_merge_mark: end the current line, write the
// marker with its line number when ln > 0, and end that line.
func (m *textMerger) appendMark(marker string, ln int) {
	m.ensureLineEnd()
	m.out = append(m.out, marker...)
	if ln > 0 {
		m.out = append(m.out, " (line "...)
		m.out = strconv.AppendInt(m.out, int64(ln), 10)
		m.out = append(m.out, ')')
	}
	m.ensureLineEnd()
}

// ensureLineEnd is fossil's ensure_line_end.
func (m *textMerger) ensureLineEnd() {
	if len(m.out) == 0 || m.out[len(m.out)-1] == '\n' {
		return
	}
	if m.useCRLF {
		m.out = append(m.out, '\r')
	}
	m.out = append(m.out, '\n')
}

// walkEdits is the main loop of fossil's merge_three_blobs. It consumes the
// edit vectors aC1 and aC2 in place and returns the builder calls fossil
// would make, and the number of conflicts. at1 and at2 map a count of V1 or
// V2 units passed to the byte offset fossil's cursor would be at, which
// sameEdit compares from.
func walkEdits(aC1, aC2 []int, v1, v2 []byte, at1, at2 func(int) int) ([]mergeOp, int) {
	limit1, limit2 := editsEnd(aC1), editsEnd(aC2)
	var ops []mergeOp
	i1, i2, u1, u2, nConflict := 0, 0, 0, 0, 0
	emit := func(op mergeOp) {
		ops = append(ops, op)
		u1 += op.nV1
		u2 += op.nV2
	}
	for i1 < limit1 && i2 < limit2 {
		switch {
		case aC1[i1] > 0 && aC2[i2] > 0:
			n := min(aC1[i1], aC2[i2])
			emit(mergeOp{opSame, n, n, n})
			aC1[i1] -= n
			aC2[i2] -= n
		case aC1[i1] >= aC2[i2+1] && aC1[i1] > 0 && aC2[i2+1]+aC2[i2+2] > 0:
			emit(mergeOp{opChngV2, aC2[i2+1], aC2[i2+1], aC2[i2+2]})
			aC1[i1] -= aC2[i2+1]
			i2 += 3
		case aC2[i2] >= aC1[i1+1] && aC2[i2] > 0 && aC1[i1+1]+aC1[i1+2] > 0:
			emit(mergeOp{opChngV1, aC1[i1+1], aC1[i1+2], aC1[i1+1]})
			aC2[i2] -= aC1[i1+1]
			i1 += 3
		case sameEdit(aC1[i1:], aC2[i2:], v1[at1(u1):], v2[at2(u2):]):
			if aC1[i1] != 0 {
				panic("merge.walkEdits: identical edits after a copy")
			}
			emit(mergeOp{opChngBoth, aC1[i1+1], aC1[i1+2], aC1[i1+2]})
			i1 += 3
			i2 += 3
		default:
			nConflict++
			sz := 1
			for !endsWithCopy(aC1[i1:], sz) || !endsWithCopy(aC2[i2:], sz) {
				sz++
			}
			var nV1, nV2 int
			i1, nV1 = skipConflict(aC1, i1, sz)
			i2, nV2 = skipConflict(aC2, i2, sz)
			emit(mergeOp{opConflict, sz, nV1, nV2})
		}
		if i1 < limit1 && aC1[i1] == 0 && aC1[i1+1] == 0 && aC1[i1+2] == 0 {
			i1 += 3
		}
		if i2 < limit2 && aC2[i2] == 0 && aC2[i2+1] == 0 && aC2[i2+2] == 0 {
			i2 += 3
		}
	}
	if i1 < limit1 && aC1[i1+2] > 0 {
		emit(mergeOp{opChngV1, 0, aC1[i1+2], 0})
	} else if i2 < limit2 && aC2[i2+2] > 0 {
		emit(mergeOp{opChngV2, 0, 0, aC2[i2+2]})
	}
	return ops, nConflict
}

// editsEnd is the index of the 0,0,0 triple that ends an edit vector.
func editsEnd(aC []int) int {
	i := 0
	for aC[i] != 0 || aC[i+1] != 0 || aC[i+2] != 0 {
		i += 3
	}
	return i
}

// sameEdit is fossil's sameEdit: the next triples match and so do the
// inserted lines, compared from each version's cursor. Fossil counts lines
// here even in a token merge, where the insert count is bytes; that is kept.
func sameEdit(aC1, aC2 []int, z1, z2 []byte) bool {
	if aC1[0] != aC2[0] || aC1[1] != aC2[1] || aC1[2] != aC2[2] {
		return false
	}
	return sameLines(z1, z2, aC1[2])
}

// sameLines is fossil's sameLines: whether the first n lines of z1 and z2
// match. Past its end each reads as the NUL fossil's blobs end with.
func sameLines(z1, z2 []byte, n int) bool {
	if n == 0 {
		return true
	}
	at := func(z []byte, i int) byte {
		if i < len(z) {
			return z[i]
		}
		return 0
	}
	for i := 0; ; i++ {
		c := at(z1, i)
		if c != at(z2, i) {
			return false
		}
		if c == '\n' || c == 0 {
			n--
			if n == 0 || c == 0 {
				return true
			}
		}
	}
}

// endsWithCopy is fossil's ends_with_copy: whether advancing sz pivot lines
// through aC lands on a copy rather than inside a delete.
func endsWithCopy(aC []int, sz int) bool {
	for sz > 0 && (aC[0] > 0 || aC[1] > 0 || aC[2] > 0) {
		if aC[0] >= sz {
			return true
		}
		sz -= aC[0]
		if aC[1] > sz {
			return false
		}
		sz -= aC[1]
		aC = aC[3:]
	}
	return true
}

// skipConflict is fossil's skip_conflict: advance aC from triple i over sz
// pivot lines, returning the new index and the lines of the version passed.
func skipConflict(aC []int, i, sz int) (next, ln int) {
	for sz > 0 {
		if aC[i] == 0 && aC[i+1] == 0 && aC[i+2] == 0 {
			break
		}
		if aC[i] >= sz {
			aC[i] -= sz
			ln += sz
			break
		}
		ln += aC[i] + aC[i+2]
		sz -= aC[i] + aC[i+1]
		i += 3
	}
	return i, ln
}
