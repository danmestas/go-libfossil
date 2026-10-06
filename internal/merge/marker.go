package merge

import "bytes"

// Conflict markers, byte for byte the lines fossil's merge writes (merge3.c),
// so a conflict go-libfossil writes is one fossil recognizes, and the other
// way round. Each marker is followed by a newline in the merged output.
const (
	markerBegin    = "<<<<<<< BEGIN MERGE CONFLICT: local copy shown first <<<<<<<<<<<<"
	markerAncestor = "||||||| COMMON ANCESTOR content follows |||||||||||||||||||||||||"
	markerMergedIn = "======= MERGED IN content follows ==============================="
	markerEnd      = ">>>>>>> END MERGE CONFLICT >>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>"

	// markerSuggested is a section fossil can write between the local copy
	// and the ancestor. go-libfossil never writes it, but a file fossil wrote
	// may contain it.
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
func HasConflictMarkers(data []byte) bool {
	markers := [...]string{markerBegin, markerSuggested, markerAncestor, markerMergedIn}
	for len(data) > 0 {
		for _, m := range markers {
			if bytes.HasPrefix(data, []byte(m)) {
				return true
			}
		}
		next := bytes.IndexByte(data, '\n')
		if next < 0 {
			return false
		}
		data = data[next+1:]
	}
	return false
}
