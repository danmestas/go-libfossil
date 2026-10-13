package conformance

import "testing"

// repostateGaps marks the repository-state cases go-libfossil is known to
// fail, by case ID, grouped by the code at fault, each with the issue
// tracking its fix.

var repostateGaps = map[string]string{
	// Crosslink's event insert reads the check-in's date row whatever its
	// source, so an inherited row from a propagating date tag sets the time
	// of a descendant re-derived after it (internal/manifest/crosslink.go
	// crosslinkCheckinTables). fossil's manifest_crosslink reads it the same
	// way.
	dateRelinkID: "#372",
}

// repostateDeviations marks the cases where go-libfossil follows fossil(1)
// rather than the draft text, by case ID.
var repostateDeviations = map[string]string{
	"repostate.7.3.parentless-untagged": parentlessDeviation,
	// verify.Rebuild now derives leaves through the crosslink sweep, so it shares
	// the same branch-aware leaf rule and the same §7.3 deviation.
	"repostate.7.3.parentless-untagged.rebuild": parentlessDeviation,
	// go-libfossil writes the date tag's value as fossil's tag_insert does, so
	// it shares the arrival-order dependence Section 4.2, step 6 removes.
	"repostate.4.2.date-tag-cancel":             dateCancelDeviation,
	"repostate.4.2.date-tag-cancel.rebuild":     dateCancelDeviation,
	"repostate.4.2.date-tag-unparsable":         dateUnparsableDeviation,
	"repostate.4.2.date-tag-unparsable.rebuild": dateUnparsableDeviation,
	"repostate.4.2.date-tag-zero":               dateZeroDeviation,
	"repostate.4.2.date-tag-zero.rebuild":       dateZeroDeviation,
}

const (
	parentlessDeviation = "follows fossil(1): an untagged parentless check-in is " +
		"never a leaf, where Section 7.3 makes every check-in without a same-branch " +
		"child one (the section's informative note records fossil's behavior)"
	dateCancelDeviation = "follows fossil(1): a cancelled date tag leaves the " +
		"check-in with no time, where Section 4.2, step 6 restores the D-card time"
	dateUnparsableDeviation = "follows fossil(1): an unparsable date tag leaves the " +
		"check-in with no time, where Section 4.2, step 6 keeps the D-card time"
	dateZeroDeviation = "follows fossil(1): a date tag of 0 stores a time of 0, " +
		"where Section 4.2, step 6 ignores a time that is not positive"
)

// TestRepoStateGapKeys checks that every marked case exists.
func TestRepoStateGapKeys(t *testing.T) {
	ids := map[string]bool{}
	for _, c := range repostateSuiteCases() {
		ids[c.ID] = true
	}
	for id := range repostateGaps {
		if !ids[id] {
			t.Errorf("repostateGaps names %s, which is not a case", id)
		}
	}
	for id := range repostateDeviations {
		if !ids[id] {
			t.Errorf("repostateDeviations names %s, which is not a case", id)
		}
	}
}
