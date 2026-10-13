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
	return append([]rsCase{
		mainBranchLeaf(),
		leaves("repostate.7.2.branch-leaves", "RS-7.2-1", merged(),
			[]string{"b1", "m"}, "-leaf: b1"),
		leaves("repostate.7.2.merge-child-leaf", "RS-7.2-1", mergedIntoTrunk(),
			[]string{"b2", "x"}, "+leaf: c2"),
		leaves("repostate.7.3.sole-root", "RS-7.3-1",
			[]rsArt{checkin("c1", 1, nil, rsRoot...)}, []string{"c1"}, "-leaf: c1"),
		leaves("repostate.7.6.retag-branch", "RS-7.6-1", ownTrunk,
			[]string{"c2", "c3", "c4"}, "-leaf: c3"),
		leaves("repostate.7.6.propagated-branch", "RS-7.6-2", retagged,
			[]string{"c2", "c4", "m"}, "-leaf: c4"),
		cancelled,
		pulledChild(), pulledRetag(),
	}, untaggedLeafCases()...)
}

// untaggedLeafCases are Section 7.3's candidates: only a check-in a branch
// tag reaches, and its parents, are ever rechecked, so an untagged check-in
// is a leaf only as the parent of a tagged one.
func untaggedLeafCases() []rsCase {
	// c1 has no tag and no parent: no branch tag reaches it, so it is no
	// candidate, and no leaf although it has no child.
	untagged := leaves("repostate.7.3.parentless-untagged", "RS-7.3-1",
		[]rsArt{checkin("c1", 1, nil)}, nil, "+leaf: c1")
	// c2, untagged, has the untagged c1 as parent and no child: neither is
	// a candidate.
	chainUntagged := leaves("repostate.7.3.untagged-chain", "RS-7.3-1",
		[]rsArt{checkin("c1", 1, nil), checkin("c2", 2, of("c1"))}, nil, "+leaf: c2")
	// fossil rebuild writes artifacts of its own for this history, so the
	// rebuild is left to the cases above and below.
	chainUntagged.rebuild = false
	// c1's singleton trunk tag rechecks c1, and c2, untagged, is on trunk
	// with it: crosslinked in one batch, c1 has its same-branch child and
	// c2 is never rechecked, so neither is a leaf.
	childBatch := leaves("repostate.7.3.untagged-child-one-batch", "RS-7.3-1",
		untaggedChild(), nil, "+leaf: c2")
	childBatch.batch = true
	childBatch.incremental = "fossil's per-artifact load crosslinks c2 in a later " +
		"transaction than c1, which keeps c1 (recheck-untagged-child); fossil rebuild " +
		"crosslinks both in one, as the case does"
	// Per artifact, c1 is rechecked, alone, when its tag lands, and linking
	// c2 later schedules nothing, so c1 keeps its verdict.
	childRecheck := leaves("repostate.7.3.recheck-untagged-child", "RS-7.3-3",
		untaggedChild(), []string{"c1"}, "-leaf: c1", "+leaf: c2")
	childRecheck.each, childRecheck.rebuild = true, false
	// Crosslinking c1, alone, untagged and parentless, schedules nothing.
	lone := leaves("repostate.7.3.recheck-lone-untagged", "RS-7.3-3",
		[]rsArt{checkin("c1", 1, nil)}, nil, "+leaf: c1")
	lone.each, lone.rebuild = true, false
	// b1's feat tag rechecks b1 and its merge parent c2, a leaf until then:
	// c2's only child, b1, is on c2's branch, feat, through the merge. c1,
	// on trunk, has only feat children and stays a leaf.
	mergeParent := leaves("repostate.7.3.recheck-merge-parent", "RS-7.3-3", []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("c2", 2, of("c1"), rsFeat...),
		checkin("b1", 3, of("c1", "c2"), rsFeat...),
	}, []string{"b1", "c1"}, "+leaf: c2")
	mergeParent.each, mergeParent.rebuild = true, false
	return []rsCase{
		untagged, chainUntagged, childBatch, childRecheck, lone, mergeParent,
		// Crosslinking b1 rechecks c1, its parent, which has no tag and no
		// parent: b1 is on feat, so c1, on the default branch, is a leaf.
		leaves("repostate.7.3.recheck-untagged-root", "RS-7.3-3",
			[]rsArt{checkin("c1", 1, nil), checkin("b1", 2, of("c1"), rsFeat...)},
			[]string{"b1", "c1"}, "-leaf: c1"),
	}
}

// pulledChild pulls c2 onto c1, which the puller already holds and has as
// its leaf. c2 arrives as an announced phantom filled the next round, so the
// phantom-fill cascade links it and the session's closing sweep finds
// nothing left: the recheck crosslinking c2 schedules must still be applied
// before the session ends, making c2 the leaf in c1's place, and c1's trunk
// tag must reach c2.
func pulledChild() rsCase {
	return rsCase{
		Case:   Case{ID: "repostate.7.3.pulled-child", Req: "RS-7.3-4"},
		arts:   chain()[:2],
		tables: []string{"leaf", "tagxref"},
		tags:   []string{"branch", "sym-trunk"},
		want: join(rows("leaf", "c2"), rows("tagxref",
			"branch c1 2 c1 c1 'trunk' @1", "branch c2 2 0 c1 'trunk' @1",
			"sym-trunk c1 2 c1 c1 NULL @1", "sym-trunk c2 2 0 c1 NULL @1")),
		ignoring: []string{"-leaf: c2", "+leaf: c1"},
		pull:     1,
	}
}

// pulledRetag pulls a control artifact moving c2 onto feat into a
// repository holding c1, c2 and c3. The cascade links t; the propagation
// to c3 and the leaf recheck of c1, which loses its trunk child, must
// still be applied before the session ends.
func pulledRetag() rsCase {
	return rsCase{
		Case: Case{ID: "repostate.7.6.pulled-retag", Req: "RS-7.6-2"},
		arts: with(chain()[:3],
			control("t", 9, "*branch {c2} feat", "*sym-feat {c2}", "-sym-trunk {c2}")),
		tables: []string{"leaf", "tagxref"},
		tags:   []string{"branch"},
		want: join(rows("leaf", "c1", "c3"), rows("tagxref",
			"branch c1 2 c1 c1 'trunk' @1", "branch c2 2 t c2 'feat' @9",
			"branch c3 2 0 c2 'feat' @9")),
		ignoring: []string{"-leaf: c1", "+tagxref: branch c3 2 0 c1 'trunk' @1",
			"-tagxref: branch c3 2 0 c2 'feat' @9"},
		pull: 3,
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
