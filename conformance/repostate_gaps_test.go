package conformance

import "testing"

// repostateGaps marks the repository-state cases go-libfossil is known to
// fail, by case ID, grouped by the code at fault, each with the issue
// tracking its fix. "#0" stands for an issue not yet filed.
var repostateGaps = map[string]string{
	// verify.Rebuild derives only check-ins and control artifacts: no
	// cluster tag, no technote tags (verify/rebuild_tags.go:47, :65); it
	// applies a control artifact the crosslink would refuse, having no
	// isArtifact gate (:56-70); it skips an absent tag target and an absent
	// parent rather than making a phantom (:124, rebuild_manifest.go:114); it
	// writes no plink baseline (rebuild_manifest.go:131); and its leaf rule
	// is "no child at all", whatever the branch (rebuild_leaves.go:16-21).
	"repostate.4.2.cluster.rebuild":             "#0",
	"repostate.4.2.technote-tags.rebuild":       "#0",
	"repostate.4.4.opaque-control.rebuild":      "#0",
	"repostate.4.2.phantom-target.rebuild":      "#0",
	"repostate.4.2.phantom-parent.rebuild":      "#0",
	"repostate.3.1.link-metadata.rebuild":       "#0",
	"repostate.7.2.branch-leaves.rebuild":       "#0",
	"repostate.7.6.retag-branch.rebuild":        "#0",
	"repostate.7.6.propagated-branch.rebuild":   "#0",
	"repostate.7.3.parentless-untagged.rebuild": "#0",
	// No parent (reparent) tag handling in the crosslink, tag or rebuild
	// code: plink keeps the old parent.
	"repostate.4.2.reparent":         "#0",
	"repostate.4.2.reparent.rebuild": "#0",
	// blob.Store returns a phantom's rid as if the content were stored: its
	// existingName takes any row blob.Exists finds, phantoms included
	// (internal/blob/blob.go:23), so an artifact that arrives after being
	// referenced is never stored or crosslinked.
	"repostate.4.2.phantom-late-target": "#0",
	// isArtifact accepts a technote with a T-card naming another artifact
	// (internal/manifest/crosslink.go:583), and crosslinkEvent applies it
	// (:1510).
	"repostate.12.technote-non-self": "#0",
	// The cancel walk deletes an inherited row whatever its mtime: the query
	// is COALESCE(srcid=0, 0), without AND tagxref.mtime < ? (tag/propagate.go:67).
	"repostate.5.4.cancel-skips-newer-inherited": "#0",
	// manifest.Checkin writes the first check-in's sym-trunk as a singleton,
	// +sym-trunk, where fossil(1) and Section 6.1 write *sym-trunk
	// (internal/manifest/manifest.go:226).
	"repostate.6.1.initial-sym": "#0",
	// ResolveVersion (repo_history.go:267-350) orders by tagxref.mtime, not
	// event.mtime (:291, :310); falls back to tip when no check-in carries
	// sym-trunk (:299); tries a name before a hash prefix (:300-320); and has
	// no rid:, tag:, root:, start:, merge-in: or NAME:TIMESTAMP form and no
	// trailing-punctuation retry.
	"repostate.8.2.event-mtime":      "#0",
	"repostate.8.2.trunk-chain":      "#0",
	"repostate.8.2.not-leaf":         "#0",
	"repostate.8.2.no-trunk-sym":     "#0",
	"repostate.8.3.hash-before-name": "#0",
	"repostate.8.3.rid":              "#0",
	"repostate.8.3.tag-prefix":       "#0",
	"repostate.8.3.root":             "#0",
	"repostate.8.3.start":            "#0",
	"repostate.8.3.merge-in":         "#0",
	"repostate.8.3.name-timestamp":   "#0",
	"repostate.8.3.trailing-punct":   "#0",
	// checkout.BranchLeaves counts closed leaves (internal/checkout/fork.go:19-26),
	// and WouldFork, built on it, also skips the leaf pre-check (fork.go:45-68).
	"repostate.7.4.closed-leaf":           "#0",
	"repostate.8.5.resolution-not-leaves": "#0",
	"repostate.9.branch-not-open":         "#0",
	"repostate.7.5.ambiguity-non-leaf":    "#0",
	"repostate.7.5.ambiguity-closed":      "#0",
	// branch.Create does not refuse the name of an open branch
	// (internal/branch/branch.go:31-120), and branch.List reports a branch
	// closed when any check-in on it is closed (branch.go:131-138).
	"repostate.6.2.new-branch-duplicate":  "#0",
	"repostate.6.3.branch-listing-closed": "#0",
	// Repo.Tag stores the name as given: no sym- prefix, no refusal of the
	// reserved wiki-, tkt- and event- prefixes (repo_extras.go:39-46,
	// internal/tag/tag.go:45).
	"repostate.2.sym-prefix":      "#0",
	"repostate.2.reserved-prefix": "#0",
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
}
