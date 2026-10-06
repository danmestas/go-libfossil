package merge

import (
	"bytes"
	"fmt"
	"strings"
)

func init() { Register(&ThreeWayText{}) }

// ThreeWayText implements line-level 3-way merge.
type ThreeWayText struct{}

func (t *ThreeWayText) Name() string { return "three-way" }

func (t *ThreeWayText) Merge(base, local, remote []byte) (*Result, error) {
	if base == nil {
		panic("merge.ThreeWayText.Merge: base must not be nil")
	}
	if local == nil {
		panic("merge.ThreeWayText.Merge: local must not be nil")
	}
	if remote == nil {
		panic("merge.ThreeWayText.Merge: remote must not be nil")
	}
	// Fast paths.
	if bytes.Equal(local, remote) {
		return &Result{Content: local, Clean: true}, nil
	}
	if bytes.Equal(base, local) {
		return &Result{Content: remote, Clean: true}, nil
	}
	if bytes.Equal(base, remote) {
		return &Result{Content: local, Clean: true}, nil
	}

	baseLines := splitLines(base)
	localLines := splitLines(local)
	remoteLines := splitLines(remote)

	// Compute LCS-based diff hunks for base→local and base→remote.
	localDiff := diffLines(baseLines, localLines)
	remoteDiff := diffLines(baseLines, remoteLines)

	// Merge the two diffs against the base.
	merged, conflicts := merge3(baseLines, localLines, remoteLines, localDiff, remoteDiff)

	var buf bytes.Buffer
	for _, line := range merged {
		buf.WriteString(line)
	}

	return &Result{
		Content:   buf.Bytes(),
		Conflicts: conflicts,
		Clean:     len(conflicts) == 0,
	}, nil
}

// hunk represents a change region: replace base[baseStart:baseEnd] with lines.
type hunk struct {
	baseStart int // first base line affected (0-indexed)
	baseEnd   int // one past last base line affected
	lines     []string
}

// diffLines computes a list of hunks that transform base into modified.
func diffLines(base, modified []string) []hunk {
	lcs := longestCommonSubseq(base, modified)

	var hunks []hunk
	bi, mi := 0, 0

	for _, match := range lcs {
		if bi < match.baseIdx || mi < match.modIdx {
			hunks = append(hunks, hunk{
				baseStart: bi,
				baseEnd:   match.baseIdx,
				lines:     modified[mi:match.modIdx],
			})
		}
		bi = match.baseIdx + 1
		mi = match.modIdx + 1
	}
	// Trailing changes after last match.
	if bi < len(base) || mi < len(modified) {
		hunks = append(hunks, hunk{
			baseStart: bi,
			baseEnd:   len(base),
			lines:     modified[mi:],
		})
	}
	return hunks
}

type lcsMatch struct {
	baseIdx int
	modIdx  int
}

// longestCommonSubseq finds matching lines between base and modified.
func longestCommonSubseq(a, b []string) []lcsMatch {
	m, n := len(a), len(b)
	// DP table.
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	// Backtrack to find matches.
	var matches []lcsMatch
	i, j := 0, 0
	for i < m && j < n {
		if a[i] == b[j] {
			matches = append(matches, lcsMatch{i, j})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			i++
		} else {
			j++
		}
	}
	return matches
}

// handleOverlappingHunks processes a pair of overlapping hunks.
// If they have the same content, returns the lines with no conflict.
// Otherwise, returns conflict markers with both versions.
func handleOverlappingHunks(base []string, lh, rh *hunk, bi int) (lines []string, conflict *Conflict) {
	if sameHunkContent(lh, rh) {
		return lh.lines, nil
	}
	// Conflict.
	c := Conflict{
		StartLine: bi + 1,
		Local:     []byte(strings.Join(lh.lines, "")),
		Remote:    []byte(strings.Join(rh.lines, "")),
	}
	// Find the base region covered by both hunks.
	start := min(lh.baseStart, rh.baseStart)
	end := max(lh.baseEnd, rh.baseEnd)
	if start < end {
		c.Base = []byte(strings.Join(base[start:end], ""))
	}
	c.EndLine = bi + max(lh.baseEnd, rh.baseEnd) - bi

	// Fossil's layout: local copy, common ancestor, merged-in content.
	var result []string
	result = appendConflictSection(result, markerBegin, lh.lines)
	result = appendConflictSection(result, markerAncestor, base[start:end])
	result = appendConflictSection(result, markerMergedIn, rh.lines)
	result = append(result, markerEnd+"\n")
	return result, &c
}

// appendConflictSection appends a marker line and the section's lines, ending
// the last line with a newline so the next marker starts a line.
func appendConflictSection(result []string, marker string, lines []string) []string {
	if marker == "" {
		panic("merge.appendConflictSection: empty marker")
	}
	result = append(result, marker+"\n")
	result = append(result, lines...)
	if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		result = append(result, "\n")
	}
	return result
}

// merge3 combines two sets of hunks against a common base.
func merge3(base, local, remote []string, localHunks, remoteHunks []hunk) ([]string, []Conflict) {
	var result []string
	var conflicts []Conflict

	li, ri := 0, 0 // hunk indices
	bi := 0         // base line index

	for bi <= len(base) {
		var lh, rh *hunk
		if li < len(localHunks) {
			lh = &localHunks[li]
		}
		if ri < len(remoteHunks) {
			rh = &remoteHunks[ri]
		}

		// No more hunks — copy remaining base.
		if lh == nil && rh == nil {
			if bi < len(base) {
				result = append(result, base[bi:]...)
			}
			break
		}

		// Find the next hunk start.
		nextStart := len(base)
		if lh != nil && lh.baseStart < nextStart {
			nextStart = lh.baseStart
		}
		if rh != nil && rh.baseStart < nextStart {
			nextStart = rh.baseStart
		}

		// Copy base lines before the next hunk.
		if bi < nextStart {
			result = append(result, base[bi:nextStart]...)
			bi = nextStart
		}

		// Determine if hunks overlap.
		if lh != nil && rh != nil && hunksOverlap(lh, rh) {
			lines, c := handleOverlappingHunks(base, lh, rh, bi)
			result = append(result, lines...)
			if c != nil {
				conflicts = append(conflicts, *c)
			}
			bi = max(lh.baseEnd, rh.baseEnd)
			li++
			ri++
			continue
		}

		// Non-overlapping: apply whichever hunk starts first.
		if lh != nil && (rh == nil || lh.baseStart <= rh.baseStart) {
			result = append(result, lh.lines...)
			bi = lh.baseEnd
			li++
		} else if rh != nil {
			result = append(result, rh.lines...)
			bi = rh.baseEnd
			ri++
		}
	}

	return result, conflicts
}

func hunksOverlap(a, b *hunk) bool {
	return a.baseStart < b.baseEnd && b.baseStart < a.baseEnd
}

func sameHunkContent(a, b *hunk) bool {
	if len(a.lines) != len(b.lines) {
		return false
	}
	for i := range a.lines {
		if a.lines[i] != b.lines[i] {
			return false
		}
	}
	return true
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Ensure fmt is available for error formatting if needed.
var _ = fmt.Sprintf
