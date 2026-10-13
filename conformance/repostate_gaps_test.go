package conformance

import "testing"

// repostateGaps marks the repository-state cases go-libfossil is known to
// fail, by case ID, grouped by the code at fault, each with the issue
// tracking its fix.
var repostateGaps = map[string]string{
	// internal/manifest: the leaf table is recomputed per crosslink batch, so
	// a later batch revisits a verdict Section 7.3 keeps.
	"repostate.7.3.recheck-untagged-child": "#426",
}

// repostateDeviations marks the cases where go-libfossil follows fossil(1)
// rather than the draft text, by case ID.
var repostateDeviations = map[string]string{}

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
