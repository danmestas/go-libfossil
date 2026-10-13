package conformance

import (
	"fmt"
	"regexp"
	"strings"
)

// The Section 9 operations that name one check-in: the current trunk and a
// branch's open leaf.

func trunkAPICases() []rsAPICase {
	// b3, on feat, is the newest check-in; c2 is the newest on trunk.
	feat3 := with(branched(), checkin("b3", 6, of("b2")))
	// feat has three leaves under b1: bz, the newest, closes itself; of the
	// open two, bx is newer than by but is listed, and stored, first.
	forkedFeat := []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("b1", 2, of("c1"), rsFeat...),
		checkin("bx", 7, of("b1")),
		checkin("by", 5, of("b1")),
		checkin("bz", 9, of("b1"), "+closed *"),
	}
	return []rsAPICase{
		currentTrunk("repostate.9.current-trunk", "RS-9-1", feat3, "trunk", "c2"),
		// A technote tagged sym-trunk, newer than trunk's tip, is no check-in.
		currentTrunk("repostate.9.current-trunk-technote", "RS-9-1",
			with(feat3, technote("e", 9, "+sym-trunk *")), "trunk", "c2"),
		// The same artifacts with main-branch set to feat: the setting is
		// the only input that changed.
		mainBranch("repostate.9.current-trunk-setting", "RS-9-1", "feat",
			currentTrunk("", "", feat3, "feat", "b3")),
		branchOpenLeaf("repostate.9.open-leaf-newest", "RS-9-4", forkedFeat, "feat", "bx"),
		branchOpenLeaf("repostate.9.open-leaf-none", "RS-9-3",
			with(branched(), control("t", 9, "+closed {b2}")), "feat", ""),
	}
}

// currentTrunk checks that Repo.CurrentTrunk gives the check-in labelled
// want. fossil(1) has no command that prints the current trunk; where it
// needs one (update outside a checkout, src/update.c, and open with no
// version, src/db.c) it resolves the main-branch setting's value as a
// check-in by symbolic_name_to_rid, so the case resolves that value,
// setting, with test-name-to-id --type ci.
func currentTrunk(id, req string, arts []rsArt, setting, want string) rsAPICase {
	return rsAPICase{Case: Case{ID: id, Req: req}, arts: arts, run: func(e *rsEnv) error {
		if e.ref != nil {
			got, err := e.fossilResolveCheckin(setting)
			if err != nil {
				return err
			}
			if got != want {
				return expectf("fossil(1) resolves main-branch %q to %q; the case expects %q",
					setting, got, want)
			}
		}
		r, err := e.open()
		if err != nil {
			return err
		}
		rid, err := r.CurrentTrunk()
		if err != nil {
			return err
		}
		if got := e.labelOfRid(r.Inner(), rid); got != want {
			return expectf("CurrentTrunk() = %q, want %q", got, want)
		}
		return nil
	}}
}

// fossilResolveCheckin is the label test-name-to-id --type ci gives name,
// or "" when it names no check-in.
func (e *rsEnv) fossilResolveCheckin(name string) (string, error) {
	out, err := e.fossilRun("test-name-to-id", "--type", "ci", name)
	if err != nil {
		return "", err
	}
	_, result, ok := strings.Cut(strings.TrimSpace(out), " -> ")
	if !ok {
		return "", fmt.Errorf("test-name-to-id: unrecognized output %q", out)
	}
	if strings.HasPrefix(result, "ERROR") {
		return "", nil
	}
	return labelOf(result, e.script.labels), nil
}

// branchInfo is `fossil branch info` naming an open branch's leaf.
var branchInfo = regexp.MustCompile(`: open as of .* on ([0-9a-f]{16})`)

// branchOpenLeaf checks that Repo.BranchOpenLeaf names the open leaf of
// branch labelled want, or reports the branch not open when want is empty.
// fossil(1)'s `branch info` (branch_is_open, src/branch.c) takes whichever
// open leaf its query yields first, with no ORDER BY, so fossil is held only
// to the open-or-not answer and to naming one of the open leaves.
func branchOpenLeaf(id, req string, arts []rsArt, branch, want string) rsAPICase {
	return rsAPICase{Case: Case{ID: id, Req: req}, arts: arts, run: func(e *rsEnv) error {
		if e.ref != nil {
			if err := e.fossilBranchInfo(branch, want); err != nil {
				return err
			}
		}
		r, err := e.open()
		if err != nil {
			return err
		}
		rid, open, err := r.BranchOpenLeaf(branch)
		if err != nil {
			return err
		}
		got := ""
		if open {
			got = e.labelOfRid(r.Inner(), rid)
		}
		if got != want {
			return expectf("BranchOpenLeaf(%q) = %q, want %q (\"\" for not open)",
				branch, got, want)
		}
		return nil
	}}
}

// fossilBranchInfo checks fossil's `branch info` agrees that branch is open
// exactly when want is set, and that the leaf it names is an open leaf.
func (e *rsEnv) fossilBranchInfo(branch, want string) error {
	out, err := e.fossilRun("branch", "info", branch)
	if err != nil {
		return err
	}
	m := branchInfo.FindStringSubmatch(out)
	if want == "" {
		if m != nil || !strings.Contains(out, "not an open branch") {
			return expectf("fossil branch info %s: %q; the case expects it not open",
				branch, strings.TrimSpace(out))
		}
		return nil
	}
	if m == nil {
		return expectf("fossil branch info %s: %q; the case expects it open",
			branch, strings.TrimSpace(out))
	}
	leaves, err := e.fossilLeaves(branch)
	if err != nil {
		return err
	}
	named := e.labelOfPrefix(m[1])
	for _, leaf := range leaves {
		if leaf == named {
			return nil
		}
	}
	return expectf("fossil branch info %s names %s, not one of its open leaves %v",
		branch, named, leaves)
}
