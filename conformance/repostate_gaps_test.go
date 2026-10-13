package conformance

import "testing"

// repostateGaps marks the repository-state cases go-libfossil is known to
// fail, by case ID, grouped by the code at fault, each with the issue
// tracking its fix.
var repostateGaps = map[string]string{}

// repostateDeviations marks the cases where go-libfossil follows fossil(1)
// rather than the draft text, by case ID.
var repostateDeviations = map[string]string{
	"repostate.7.3.parentless-untagged": parentlessDeviation,
	// verify.Rebuild now derives leaves through the crosslink sweep, so it shares
	// the same branch-aware leaf rule and the same §7.3 deviation.
	"repostate.7.3.parentless-untagged.rebuild": parentlessDeviation,
	// The same check-in seen from Section 7.3's crosslink trigger: linking it
	// does not recheck it, as fossil's does not.
	"repostate.7.3.recheck-lone-untagged": parentlessDeviation,
	// An untagged child of a tagged check-in: Section 7.3 rechecks it and
	// its parent when it is linked.
	"repostate.7.3.recheck-untagged-child":         untaggedChildDeviation,
	"repostate.7.3.recheck-untagged-child.rebuild": untaggedChildDeviation,
	// ckout.: Section 8.3 retries the whole procedure under a ci type.
	"repostate.8.3.ckout-inclusive-whole-name": ckoutWholeNameDeviation,
}

const ckoutWholeNameDeviation = "follows fossil(1): the ci+ request is rewritten to ci, so " +
	"the trailing-punctuation retry and the recursive forms read ckout as an ordinary " +
	"name, where Section 8.3 retries the whole procedure and makes ckout a keyword under " +
	"ci as well (erratum pending)"

const untaggedChildDeviation = "follows fossil(1): a check-in with a parent but no branch " +
	"tag is never rechecked, so it is no leaf and its parent keeps its verdict, where " +
	"Section 7.3 rechecks every crosslinked check-in and its parents; widening the " +
	"candidates changed the leaf table of a real fossil-authored repository (the " +
	"libfossil corpus)"

const parentlessDeviation = "follows fossil(1): an untagged parentless check-in is " +
	"never a leaf, where Section 7.3 makes every check-in without a same-branch " +
	"child one (the section's informative note records fossil's behavior)"

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
