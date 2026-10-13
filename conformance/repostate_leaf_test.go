package conformance

// Derived-state cases for branch membership (Section 6) and the leaf table
// (Section 7).

func branchCases() []rsCase {
	return []rsCase{{
		// b2 inherits feat, and c2 trunk, each from its branch's root.
		Case:   Case{ID: "repostate.5.4.branch-inherited", Req: "RS-5.4-5"},
		arts:   branched(),
		tables: []string{"tagxref"},
		tags:   []string{"branch", "sym-feat", "sym-trunk"},
		want: rows("tagxref",
			"branch c1 2 c1 c1 'trunk' @1", "branch b1 2 b1 b1 'feat' @2",
			"branch b2 2 0 b1 'feat' @2", "branch c2 2 0 c1 'trunk' @1",
			"sym-trunk c1 2 c1 c1 NULL @1", "sym-trunk b1 0 b1 b1 NULL @2",
			"sym-trunk c2 2 0 c1 NULL @1",
			"sym-feat b1 2 b1 b1 NULL @2", "sym-feat b2 2 0 b1 NULL @2"),
		ignoring: []string{"-tagxref: branch b2 2 0 b1 'feat' @2",
			"+tagxref: branch b2 2 0 c1 'trunk' @1"},
		rebuild: true,
	}}
}

// leaves is a case comparing only the leaf table.
func leaves(id, req string, arts []rsArt, want []string, ignoring ...string) rsCase {
	return rsCase{
		Case:     Case{ID: id, Req: req},
		arts:     arts,
		tables:   []string{"leaf"},
		want:     rows("leaf", want...),
		ignoring: ignoring,
		rebuild:  true,
	}
}

// mergedIntoTrunk is c1 with b1 and b2 on feat and c2 on trunk; x, moved
// back to trunk, has primary parent b1 and merge parent c2. c2's only
// child is x, on its own branch through a merge link, so c2 is no leaf.
func mergedIntoTrunk() []rsArt {
	return []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("b1", 2, of("c1"), rsFeat...),
		checkin("c2", 3, of("c1")),
		checkin("b2", 4, of("b1")),
		checkin("x", 5, of("b1", "c2"), "*branch * trunk", "*sym-trunk *", "-sym-feat *"),
	}
}

func leafCases() []rsCase {
	// Section 7.3 makes a parentless check-in with no same-branch child a
	// leaf; fossil(1) never makes an untagged one a leaf. The case follows
	// the draft, and go-libfossil's crosslink, following fossil, is marked
	// as a deviation.
	untagged := leaves("repostate.7.3.parentless-untagged", "RS-7.3-1",
		[]rsArt{checkin("c1", 1, nil)}, []string{"c1"}, "-leaf: c1")
	untagged.fossil = "fossil 2.28 never makes an untagged parentless check-in a leaf; " +
		"erratum candidate against Section 7.3"
	// c4 carries its own trunk tag, so retagging c3 feat leaves c3 with no
	// same-branch child: re-evaluating c3 itself makes it a leaf.
	ownTrunk := with(chain()[:3], checkin("c4", 4, of("c3"), "*branch * trunk"),
		control("t", 9, "*branch {c3} feat", "*sym-feat {c3}", "-sym-trunk {c3}"))
	// c4 gains feat by propagation from c3 and so stops being the merge
	// parent of a same-branch check-in: m stays on trunk.
	retagged := with(chain(), checkin("m", 5, of("c1", "c4")),
		control("t", 9, "*branch {c3} feat", "*sym-feat {c3}", "-sym-trunk {c3}"))
	// c1's cancelled feat row is not active, so c1 is on trunk, the
	// default branch, with c2: no leaf.
	cancelled := leaves("repostate.6.4.cancelled-branch-leaf", "RS-6.4-1", cancelledBranch(),
		[]string{"c2"}, "+leaf: c1")
	cancelled.fossil = "fossil 2.28's leaf_check reads the branch value without tagtype > 0 " +
		"(src/leaf.c), so c1's cancelled feat row keeps it a leaf; fossil-bug candidate " +
		"against Section 6.4"
	// Crosslinking c2 rechecks c2, a leaf, and c1, which c2 joins on trunk.
	childRecheck := leaves("repostate.7.3.recheck-untagged-child", "RS-7.3-3",
		untaggedChild(), []string{"c2"}, "-leaf: c2", "+leaf: c1")
	childRecheck.fossil = "fossil 2.28 rechecks leaves only when a branch tag lands, so c2, " +
		"which carries none, is never checked: per-artifact keeps c1, rebuild neither; the " +
		"informative note of Section 7.3 records this"
	// Crosslinking c1, alone, untagged and parentless, rechecks it: the
	// trigger half of the RS-7.3-1 deviation.
	lone := leaves("repostate.7.3.recheck-lone-untagged", "RS-7.3-3",
		[]rsArt{checkin("c1", 1, nil)}, []string{"c1"}, "-leaf: c1")
	lone.each, lone.rebuild, lone.fossil = true, false, untagged.fossil
	return []rsCase{
		mainBranchLeaf(),
		leaves("repostate.7.2.branch-leaves", "RS-7.2-1", merged(),
			[]string{"b1", "m"}, "-leaf: b1"),
		leaves("repostate.7.2.merge-child-leaf", "RS-7.2-1", mergedIntoTrunk(),
			[]string{"b2", "x"}, "+leaf: c2"),
		untagged,
		leaves("repostate.7.3.sole-root", "RS-7.3-1",
			[]rsArt{checkin("c1", 1, nil, rsRoot...)}, []string{"c1"}, "-leaf: c1"),
		leaves("repostate.7.6.retag-branch", "RS-7.6-1", ownTrunk,
			[]string{"c2", "c3", "c4"}, "-leaf: c3"),
		leaves("repostate.7.6.propagated-branch", "RS-7.6-2", retagged,
			[]string{"c2", "c4", "m"}, "-leaf: c4"),
		cancelled, childRecheck,
		// Crosslinking b1 rechecks c1, its parent, which has no tag and no
		// parent: b1 is on feat, so c1, on the default branch, is a leaf.
		leaves("repostate.7.3.recheck-untagged-root", "RS-7.3-3",
			[]rsArt{checkin("c1", 1, nil), checkin("b1", 2, of("c1"), rsFeat...)},
			[]string{"b1", "c1"}, "-leaf: c1"),
		lone,
	}
}

// mainBranchLeaf sets main-branch to main before anything loads. c1 carries
// no branch tag, so Section 6.1 puts it on main, the default branch, where
// b1 starts: b1 is a same-branch child, and c1 is no leaf. Read as the
// literal trunk, c1 would have no same-branch child and stay one. The
// setting is local, not an artifact, so the case is doctored in and loads
// per artifact only.
func mainBranchLeaf() rsCase {
	c := leaves("repostate.6.1.main-branch-leaf", "RS-6.1-4", []rsArt{
		checkin("c1", 1, nil),
		checkin("b1", 2, of("c1"), "*branch * main", "*sym-main *"),
	}, []string{"b1"}, "+leaf: c1")
	c.rebuild = false
	c.doctor = &rsDoctor{at: 0, sql: "REPLACE INTO config(name, value, mtime) " +
		"VALUES('main-branch', 'main', 0)"}
	c.fossil = "fossil 2.28's leaf_check reads an untagged check-in as the literal trunk " +
		"(src/leaf.c), whatever main-branch says, so c1 stays a leaf; fossil-bug candidate " +
		"against Section 6.1"
	return c
}
