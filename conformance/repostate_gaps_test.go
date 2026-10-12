package conformance

import "testing"

// repostateGaps marks the repository-state cases go-libfossil is known to
// fail, by case ID, grouped by the code at fault, each with the issue
// tracking its fix.
var repostateGaps = map[string]string{}

// repostateDeviations marks the cases where go-libfossil follows fossil(1)
// rather than the draft text, by case ID.
var repostateDeviations = map[string]string{
	"repostate.7.3.parentless-untagged": "follows fossil(1): an untagged parentless " +
		"check-in is never a leaf, where Section 7.3 makes every check-in without a " +
		"same-branch child one (erratum candidate)",
	// verify.Rebuild now derives leaves through the crosslink sweep, so it shares
	// the same branch-aware leaf rule and the same §7.3 deviation.
	"repostate.7.3.parentless-untagged.rebuild": "follows fossil(1): an untagged parentless " +
		"check-in is never a leaf, where Section 7.3 makes every check-in without a " +
		"same-branch child one (erratum candidate)",
}

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
