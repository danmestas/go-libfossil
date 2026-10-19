package merge

import "bytes"

// lcsMatch is a common block: from[iSX:iEX] equals to[iSY:iEY].
type lcsMatch struct {
	iSX, iEX, iSY, iEY int
}

// longestCommonSequence is fossil's: an O(N) hash-chain heuristic for a
// long common block near the middle, falling back to optimalLCS for small
// inputs where the heuristic finds nothing.
func (c *dcontext) longestCommonSequence(iS1, iE1, iS2, iE2 int) (iSX, iEX, iSY, iEY int) {
	var best lcsMatch
	cutoff := 4
	for {
		var nextCutoff int
		best, nextCutoff = c.lcsPass(iS1, iE1, iS2, iE2, cutoff)
		if best.iSX != best.iEX || nextCutoff == 0 {
			break
		}
		if cutoff = nextCutoff; cutoff > 64 {
			break
		}
	}
	if best.iSX == best.iEX && int64(iE1-iS1)*int64(iE2-iS2) < 2500 {
		return c.optimalLCS(iS1, iE1, iS2, iE2)
	}
	return best.iSX, best.iEX, best.iSY, best.iEY
}

// lcsPass is one pass of the do-loop in fossil's longestCommonSequence,
// following at most cutoff hash-chain entries per line. Fossil keeps its
// locals across passes, but it repeats a pass only when the last one found
// nothing, which leaves them as initialized, so each pass starts fresh.
func (c *dcontext) lcsPass(iS1, iE1, iS2, iE2, cutoff int) (lcsMatch, int) {
	span := int64((iE1 - iS1) + (iE2 - iS2))
	bestScore := int64(-9223300000 * 1000000000)
	best := lcsMatch{iS1, iS1, iS2, iS2}
	prev := best
	mid := (iE1 + iS1) / 2
	nextCutoff := 0
	for i := iS1; i < iE1; i++ {
		j, exceeded := c.chainMatch(i, iS2, iE2, cutoff)
		if exceeded {
			nextCutoff = cutoff * 4
		}
		if j == 0 {
			continue
		}
		if i < best.iEX && j >= best.iSY && j < best.iEY {
			continue
		}
		if i < prev.iEX && j >= prev.iSY && j < prev.iEY {
			continue
		}
		m := c.growMatch(i, j-1, iS1, iE1, iS2, iE2)
		skew := abs((m.iSX - iS1) - (m.iSY - iS2))
		dist := abs((m.iSX+m.iEX)/2 - mid)
		score := int64(m.iEX-m.iSX)*span - int64(skew+dist)
		if score > bestScore {
			bestScore = score
			best = m
		} else if m.iEX > prev.iEX {
			prev = m
		}
	}
	return best, nextCutoff
}

// chainMatch walks the hash chain for from[i] and returns 1+ the index of
// the first equal line in to[iS2:iE2], or 0. exceeded reports that the walk
// gave up after cutoff entries.
func (c *dcontext) chainMatch(i, iS2, iE2, cutoff int) (j int, exceeded bool) {
	j = int(c.to[c.from[i].h%uint64(c.nTo)].iHash)
	limit := 0
	for j > 0 && (j-1 < iS2 || j >= iE2 || differ(&c.from[i], &c.to[j-1])) {
		if limit > cutoff {
			return 0, true
		}
		limit++
		j = int(c.to[j-1].iNext)
	}
	return j, false
}

// growMatch extends the single-line match from[i] == to[j] backwards and
// forwards within the ranges.
func (c *dcontext) growMatch(i, j, iS1, iE1, iS2, iE2 int) lcsMatch {
	m := lcsMatch{iSX: i, iSY: j, iEX: i + 1, iEY: j + 1}
	n := min(m.iSX-iS1, m.iSY-iS2)
	k := 0
	for k < n && !differ(&c.from[m.iSX-1-k], &c.to[m.iSY-1-k]) {
		k++
	}
	m.iSX -= k
	m.iSY -= k
	n = min(iE1-m.iEX, iE2-m.iEY)
	k = 0
	for k < n && !differ(&c.from[m.iEX+k], &c.to[m.iEY+k]) {
		k++
	}
	m.iEX += k
	m.iEY += k
	return m
}

// optimalLCS is fossil's exhaustive O(N*N) longest common block.
func (c *dcontext) optimalLCS(iS1, iE1, iS2, iE2 int) (iSX, iEX, iSY, iEY int) {
	mxLength := 0
	iSXb, iSYb := iS1, iS2
	for i := iS1; i < iE1-mxLength; i++ {
		for j := iS2; j < iE2-mxLength; j++ {
			if differ(&c.from[i], &c.to[j]) {
				continue
			}
			if mxLength > 0 && differ(&c.from[i+mxLength], &c.to[j+mxLength]) {
				continue
			}
			k := 1
			for i+k < iE1 && j+k < iE2 && !differ(&c.from[i+k], &c.to[j+k]) {
				k++
			}
			if k > mxLength {
				iSXb, iSYb, mxLength = i, j, k
			}
		}
	}
	return iSXb, iSXb + mxLength, iSYb, iSYb + mxLength
}

// likelyNotIndentChngArtifact is fossil's heuristic for rejecting a short
// common block that is probably an artifact of an indentation change.
func (c *dcontext) likelyNotIndentChngArtifact(iS1, iSX, iEX, iE1 int) bool {
	if (iEX-iSX)*7 >= iE1-iS1 {
		return true
	}
	n := 0
	for i := iSX; i < iEX; i++ {
		computeIndent(&c.from[i])
		if c.from[i].nw > 1 {
			n++
		}
	}
	if n == 0 {
		return false
	}
	for i := iS1; i < iE1; i++ {
		if i == iSX {
			if i = iEX; i >= iE1 {
				break
			}
		}
		computeIndent(&c.from[i])
	}
	i := iSX
	for ; i < iEX; i++ {
		if c.from[i].nw <= 1 {
			continue
		}
		if c.hasTrimmedTwin(i, iS1, iSX) {
			continue
		}
		if !c.hasTrimmedTwin(i, iEX, iE1) {
			break
		}
	}
	return i < iEX
}

// hasTrimmedTwin reports whether some line in from[lo:hi] equals from[i]
// with leading and trailing space removed.
func (c *dcontext) hasTrimmedTwin(i, lo, hi int) bool {
	a := &c.from[i]
	z := a.z[a.indent : int(a.indent)+int(a.nw)]
	for j := lo; j < hi; j++ {
		b := &c.from[j]
		if b.nw == a.nw && bytes.Equal(b.z[b.indent:int(b.indent)+int(b.nw)], z) {
			return true
		}
	}
	return false
}

// computeIndent fills in a line's indent and nw the first time they are
// needed, as fossil does (nw==0 means not yet computed, or all space).
func computeIndent(a *dline) {
	if a.nw != 0 || a.n == 0 {
		return
	}
	nn := int(a.n)
	ii := 0
	for ii < nn && isDiffSpace(a.z[ii]) {
		ii++
	}
	a.indent = uint16(ii)
	jj := nn - 1
	for jj > ii && isDiffSpace(a.z[jj]) {
		jj--
	}
	a.nw = uint16(jj - ii + 1)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
