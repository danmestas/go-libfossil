// Package diff produces unified diff output from two byte slices
// using the Myers diff algorithm.
package diff

import (
	"bytes"
	"fmt"
	"strings"
)

// Options configures diff output.
type Options struct {
	ContextLines int    // lines of context around changes; 0 means no context
	SrcName      string // source file label for header (e.g. "a/file.txt")
	DstName      string // destination file label for header
}

// DiffStat summarizes the magnitude of changes.
type DiffStat struct {
	Insertions int  // lines present only in b
	Deletions  int  // lines present only in a
	Binary     bool // true if either input contains null bytes
}

// splitLines splits data into lines, stripping \r and trailing empty line.
// The second return value is true if data ends with a newline (or is empty).
func splitLines(data []byte) ([]string, bool) {
	if len(data) == 0 {
		return nil, true
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	endsWithNewline := strings.HasSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, true
	}
	return lines, endsWithNewline
}

// isBinary returns true if data contains a null byte.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

type opKind int

const (
	opEqual opKind = iota
	opInsert
	opDelete
)

type editOp struct {
	kind opKind
	text string
}

// myers computes the minimal edit script between src and dst
// using Myers' O(nd) algorithm.
func myers(src, dst []string) []editOp {
	n := len(src)
	m := len(dst)

	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 {
		ops := make([]editOp, m)
		for i, line := range dst {
			ops[i] = editOp{opInsert, line}
		}
		return ops
	}
	if m == 0 {
		ops := make([]editOp, n)
		for i, line := range src {
			ops[i] = editOp{opDelete, line}
		}
		return ops
	}

	max := n + m
	// V array indexed by diagonal k in [-max, max].
	// We use offset = max so V[k+max] = furthest x on diagonal k.
	size := 2*max + 1

	// Store V snapshots for backtracking.
	var trace [][]int

	v := make([]int, size)
	for i := range v {
		v[i] = 0
	}
	v[1+max] = 0 // standard Myers: V[1] = 0

	for d := 0; d <= max; d++ {
		// Snapshot V before mutations.
		snap := make([]int, size)
		copy(snap, v)
		trace = append(trace, snap)

		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1+max] < v[k+1+max]) {
				x = v[k+1+max] // move down (insert)
			} else {
				x = v[k-1+max] + 1 // move right (delete)
			}
			y := x - k

			// Follow diagonal (equal lines).
			for x < n && y < m && src[x] == dst[y] {
				x++
				y++
			}

			v[k+max] = x

			if x >= n && y >= m {
				// Reached the end. Backtrack to build edit script.
				return backtrack(trace, src, dst)
			}
		}
	}

	// Should not reach here for valid inputs.
	panic("diff.myers: failed to find edit path")
}

// backtrack reconstructs the edit script from Myers trace snapshots.
func backtrack(trace [][]int, src, dst []string) []editOp {
	n := len(src)
	m := len(dst)
	max := n + m

	x, y := n, m
	var ops []editOp

	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		k := x - y

		var prevK int
		if k == -d || (k != d && v[k-1+max] < v[k+1+max]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}

		prevX := v[prevK+max]
		prevY := prevX - prevK

		// Diagonal moves (equal lines) -- walk backward.
		for x > prevX && y > prevY {
			x--
			y--
			ops = append(ops, editOp{opEqual, src[x]})
		}

		if d > 0 {
			if x == prevX {
				// Vertical move: insert
				y--
				ops = append(ops, editOp{opInsert, dst[y]})
			} else {
				// Horizontal move: delete
				x--
				ops = append(ops, editOp{opDelete, src[x]})
			}
		}
	}

	// Reverse since we built it backward.
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}

	// Postcondition: edit script applied to src must produce dst.
	var ri int
	for _, op := range ops {
		if op.kind == opEqual || op.kind == opInsert {
			if ri >= len(dst) || op.text != dst[ri] {
				panic("diff.backtrack: postcondition violated: edit script does not reconstruct dst")
			}
			ri++
		}
	}
	if ri != len(dst) {
		panic("diff.backtrack: postcondition violated: edit script length mismatch")
	}

	return ops
}

// hunk represents a group of changes with surrounding context.
type hunk struct {
	srcStart, srcCount int
	dstStart, dstCount int
	ops                []editOp
}

type changeRange struct{ start, end int }

// findChanges returns contiguous ranges of non-equal ops.
func findChanges(ops []editOp) []changeRange {
	var changes []changeRange
	for i, op := range ops {
		if op.kind != opEqual {
			if len(changes) == 0 || i > changes[len(changes)-1].end {
				changes = append(changes, changeRange{i, i + 1})
			} else {
				changes[len(changes)-1].end = i + 1
			}
		}
	}
	return changes
}

// mergeAdjacentChanges combines change ranges separated by fewer than
// 2*contextLines equal ops into single ranges.
func mergeAdjacentChanges(changes []changeRange, contextLines int) []changeRange {
	if len(changes) == 0 {
		return nil
	}
	merged := []changeRange{changes[0]}
	for _, c := range changes[1:] {
		prev := &merged[len(merged)-1]
		if c.start-prev.end <= 2*contextLines {
			prev.end = c.end
		} else {
			merged = append(merged, c)
		}
	}
	return merged
}

// buildHunks groups edit ops into hunks with context lines.
// Adjacent hunks within 2*contextLines of each other are merged.
func buildHunks(ops []editOp, contextLines int) []hunk {
	changes := findChanges(ops)
	if len(changes) == 0 {
		return nil
	}
	merged := mergeAdjacentChanges(changes, contextLines)

	// Track src/dst line positions through ops.
	srcPos := make([]int, len(ops))
	dstPos := make([]int, len(ops))
	si, di := 0, 0
	for i, op := range ops {
		srcPos[i] = si
		dstPos[i] = di
		switch op.kind {
		case opEqual:
			si++
			di++
		case opDelete:
			si++
		case opInsert:
			di++
		}
	}

	var hunks []hunk
	for _, cr := range merged {
		start := cr.start - contextLines
		if start < 0 {
			start = 0
		}
		end := cr.end + contextLines
		if end > len(ops) {
			end = len(ops)
		}

		h := hunk{
			srcStart: srcPos[start] + 1, // 1-indexed
			dstStart: dstPos[start] + 1,
			ops:      ops[start:end],
		}
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				h.srcCount++
				h.dstCount++
			case opDelete:
				h.srcCount++
			case opInsert:
				h.dstCount++
			}
		}
		hunks = append(hunks, h)
	}
	return hunks
}

// noNewlineMarker is the POSIX / Fossil marker for files lacking a trailing newline.
const noNewlineMarker = "\\ No newline at end of file\n"

// formatUnified formats hunks as a unified diff string.
// srcNoNL and dstNoNL indicate whether the respective sides lack a trailing newline.
// When a side lacks a trailing newline, the marker is emitted after the last
// line of the final hunk that came from that side.
func formatUnified(hunks []hunk, srcName, dstName string, srcNoNL, dstNoNL bool, srcTotal, dstTotal int) string {
	if len(hunks) == 0 {
		return ""
	}

	var buf strings.Builder
	if srcName == "" {
		srcName = "a"
	}
	if dstName == "" {
		dstName = "b"
	}
	fmt.Fprintf(&buf, "--- %s\n", srcName)
	fmt.Fprintf(&buf, "+++ %s\n", dstName)

	nHunks := len(hunks)

	for i := range hunks {
		h := &hunks[i]
		fmt.Fprintf(&buf, "@@ -%d,%d +%d,%d @@\n",
			h.srcStart, h.srcCount, h.dstStart, h.dstCount)

		isLast := i == nHunks-1

		// Pre-compute the index of the last op that contributes to each side,
		// used only for the final hunk (where marker emission is possible).
		lastSrcOpIdx := -1
		lastDstOpIdx := -1
		if isLast {
			hunkSrcEnd := h.srcStart + h.srcCount - 1
			hunkDstEnd := h.dstStart + h.dstCount - 1
			if srcNoNL && hunkSrcEnd == srcTotal {
				for k := len(h.ops) - 1; k >= 0; k-- {
					if h.ops[k].kind == opDelete || h.ops[k].kind == opEqual {
						lastSrcOpIdx = k
						break
					}
				}
			}
			if dstNoNL && hunkDstEnd == dstTotal {
				for k := len(h.ops) - 1; k >= 0; k-- {
					if h.ops[k].kind == opInsert || h.ops[k].kind == opEqual {
						lastDstOpIdx = k
						break
					}
				}
			}
		}

		for j, op := range h.ops {
			switch op.kind {
			case opEqual:
				fmt.Fprintf(&buf, " %s\n", op.text)
				if j == lastSrcOpIdx {
					buf.WriteString(noNewlineMarker)
				}
				if j == lastDstOpIdx {
					buf.WriteString(noNewlineMarker)
				}
			case opDelete:
				fmt.Fprintf(&buf, "-%s\n", op.text)
				if j == lastSrcOpIdx {
					buf.WriteString(noNewlineMarker)
				}
			case opInsert:
				fmt.Fprintf(&buf, "+%s\n", op.text)
				if j == lastDstOpIdx {
					buf.WriteString(noNewlineMarker)
				}
			}
		}
	}

	return buf.String()
}

// Unified returns a unified diff string between a and b.
// Returns "" if a and b are identical. For binary inputs, returns
// a message matching Fossil's behavior.
func Unified(a, b []byte, opts Options) string {
	if opts.ContextLines < 0 {
		panic("diff.Unified: ContextLines must not be negative")
	}
	if isBinary(a) || isBinary(b) {
		srcName, dstName := opts.SrcName, opts.DstName
		if srcName == "" {
			srcName = "a"
		}
		if dstName == "" {
			dstName = "b"
		}
		return fmt.Sprintf("--- %s\n+++ %s\ncannot compute difference between binary files\n", srcName, dstName)
	}
	srcLines, srcHasNL := splitLines(a)
	dstLines, dstHasNL := splitLines(b)
	ops := myers(srcLines, dstLines)

	allEqual := true
	for _, op := range ops {
		if op.kind != opEqual {
			allEqual = false
			break
		}
	}
	if allEqual {
		return ""
	}

	hunks := buildHunks(ops, opts.ContextLines)
	return formatUnified(hunks, opts.SrcName, opts.DstName, !srcHasNL, !dstHasNL, len(srcLines), len(dstLines))
}

// Stat returns insertion/deletion counts between a and b.
func Stat(a, b []byte) DiffStat {
	if isBinary(a) || isBinary(b) {
		return DiffStat{Binary: true}
	}
	srcLines, _ := splitLines(a)
	dstLines, _ := splitLines(b)
	ops := myers(srcLines, dstLines)

	var stat DiffStat
	for _, op := range ops {
		switch op.kind {
		case opInsert:
			stat.Insertions++
		case opDelete:
			stat.Deletions++
		}
	}
	return stat
}
