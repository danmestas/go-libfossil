package conformance

import "testing"

// repostateGaps marks the repository-state cases go-libfossil is known to
// fail, by case ID, grouped by the code at fault, each with the issue
// tracking its fix.
var repostateGaps = map[string]string{
	// verify.Rebuild derives only check-ins and control artifacts: no
	// cluster tag, no technote tags (verify/rebuild_tags.go:47, :65); it
	// applies a control artifact the crosslink would refuse, having no
	// isArtifact gate (:56-70); it skips an absent tag target and an absent
	// parent rather than making a phantom (:124, rebuild_manifest.go:114); it
	// writes no plink baseline (rebuild_manifest.go:131); and its leaf rule
	// is "no child at all", whatever the branch (rebuild_leaves.go:16-21).
	"repostate.4.2.cluster.rebuild":           "#285",
	"repostate.4.2.technote-tags.rebuild":     "#285",
	"repostate.4.4.opaque-control.rebuild":    "#285",
	"repostate.4.2.phantom-target.rebuild":    "#285",
	"repostate.4.2.phantom-parent.rebuild":    "#285",
	"repostate.3.1.link-metadata.rebuild":     "#285",
	"repostate.7.2.branch-leaves.rebuild":     "#285",
	"repostate.7.6.retag-branch.rebuild":      "#285",
	"repostate.7.6.propagated-branch.rebuild": "#285",
}

// repostateDeviations marks the cases where go-libfossil follows fossil(1)
// rather than the draft text, by case ID.
var repostateDeviations = map[string]string{
	"repostate.7.3.parentless-untagged": "follows fossil(1): an untagged parentless " +
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
