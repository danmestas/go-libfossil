package conformance

// Derived-state cases for branch membership (Section 6) and the leaf table
// (Section 7).

func branchCases() []rsCase {
	return []rsCase{{
		Case:   Case{ID: "repostate.6.1.branch-membership", Req: "RS-6.1-1"},
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
	untagged := leaves("repostate.7.3.parentless-untagged", "RS-7.3-1",
		[]rsArt{checkin("c1", 1, nil)}, nil, "+leaf: c1")
	untagged.draft = "Section 7.3 makes every parentless check-in without a same-branch " +
		"child a leaf; fossil 2.28 never makes an untagged one a leaf (erratum candidate)"
	each := leaves("repostate.7.3.recheck-parents", "RS-7.3-3",
		mergedIntoTrunk(), []string{"b2", "x"}, "+leaf: c2")
	each.each = true
	incremental := leaves("repostate.7.3.incremental", "RS-7.3-2", chain(), []string{"c4"},
		"+leaf: c3")
	incremental.each = true
	// c4 gains feat by propagation from c3 and so stops being the merge
	// parent of a same-branch check-in: m stays on trunk.
	retagged := with(chain(), checkin("m", 5, of("c1", "c4")),
		control("t", 9, "*branch {c3} feat", "*sym-feat {c3}", "-sym-trunk {c3}"))
	return []rsCase{
		leaves("repostate.7.2.branch-leaves", "RS-7.2-1", merged(),
			[]string{"b1", "m"}, "-leaf: b1"),
		leaves("repostate.7.2.merge-child-leaf", "RS-7.2-1", mergedIntoTrunk(),
			[]string{"b2", "x"}, "+leaf: c2"),
		untagged,
		leaves("repostate.7.3.sole-root", "RS-7.3-1",
			[]rsArt{checkin("c1", 1, nil, rsRoot...)}, []string{"c1"}, "-leaf: c1"),
		incremental,
		each,
		leaves("repostate.7.3.batch-applied", "RS-7.3-4", mergedIntoTrunk(),
			[]string{"b2", "x"}, "+leaf: c2"),
		leaves("repostate.7.6.retag-branch", "RS-7.6-1",
			with(chain(), control("t", 9, "*branch {c3} feat", "*sym-feat {c3}",
				"-sym-trunk {c3}")),
			[]string{"c2", "c4"}, "-leaf: c2"),
		leaves("repostate.7.6.propagated-branch", "RS-7.6-2", retagged,
			[]string{"c2", "c4", "m"}, "-leaf: c4"),
	}
}
