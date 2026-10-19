package merge

import "bytes"

// Conflict markers, byte for byte the lines fossil's merge writes (merge3.c),
// so a conflict go-libfossil writes is one fossil recognizes, and the other
// way round. In the merged output the begin, ancestor and merged-in markers
// carry a " (line N)" suffix, and each marker ends its line.
const (
	markerBegin    = "<<<<<<< BEGIN MERGE CONFLICT: local copy shown first <<<<<<<<<<<<"
	markerAncestor = "||||||| COMMON ANCESTOR content follows |||||||||||||||||||||||||"
	markerMergedIn = "======= MERGED IN content follows ==============================="
	markerEnd      = ">>>>>>> END MERGE CONFLICT >>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>"

	// markerSuggested introduces the token-level resolution written between
	// the local copy and the ancestor.
	markerSuggested = "####### SUGGESTED CONFLICT RESOLUTION follows ###################"
)

// markerLen is the length every marker shares; detection compares this many
// bytes at the start of each line, as fossil does.
const markerLen = len(markerBegin)

// Fossil's detector assumes every marker has the same length.
var _ = [1]struct{}{}[len(markerAncestor)-markerLen]
var _ = [1]struct{}{}[len(markerMergedIn)-markerLen]
var _ = [1]struct{}{}[len(markerEnd)-markerLen]
var _ = [1]struct{}{}[len(markerSuggested)-markerLen]

// HasConflictMarkers reports whether data contains a line that starts with
// one of the opening or middle conflict markers. This is fossil's
// contains_merge_marker: a file is in conflict exactly when it holds one, so
// a conflict is resolved by editing the markers out, with nothing to record.
// As in fossil, a line starts after any run of '\n' and '\r' bytes.
func HasConflictMarkers(data []byte) bool {
	markers := [...]string{markerBegin, markerSuggested, markerAncestor, markerMergedIn}
	n := len(data) - markerLen + 1
	for i := 0; i < n; {
		for _, m := range markers {
			if bytes.HasPrefix(data[i:], []byte(m)) {
				return true
			}
		}
		for i < n && data[i] != '\n' {
			i++
		}
		for i < n && (data[i] == '\n' || data[i] == '\r') {
			i++
		}
	}
	return false
}
