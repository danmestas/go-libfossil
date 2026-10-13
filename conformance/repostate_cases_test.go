package conformance

// The derived-state cases, by draft section. Rows read:
//
//	plink    parent>child isprim @mtime baseline
//	tagxref  tag rid tagtype srcid origid value @mtime
//	leaf     rid
//	phantom  rid
//	event    objid type @mtime
//
// with every artifact named by its label and srcid 0 written 0.

func repostateCases() []rsCase {
	var all []rsCase
	for _, table := range [][]rsCase{
		dagCases(), crosslinkCases(), rejectionCases(), tagTypeCases(),
		propagationCases(), blockingCases(), branchCases(), leafCases(), dateTagCases(),
	} {
		all = append(all, table...)
	}
	return all
}

func dagCases() []rsCase {
	return []rsCase{{
		Case:     Case{ID: "repostate.3.2.parents-order", Req: "RS-3.2-1"},
		arts:     merged(),
		tables:   []string{"plink"},
		want:     rows("plink", "c1>c2 1 @2 -", "c1>b1 1 @3 -", "c2>m 1 @4 -", "b1>m 0 @4 -"),
		ignoring: []string{"-plink: b1>m 0 @4 -", "+plink: b1>m 1 @4 -"},
		rebuild:  true,
	}, {
		// plink's unique key on (pid, cid) makes a second row for c1 an
		// error, so a crosslink that ignored the rule would lose the link.
		Case: Case{ID: "repostate.3.1.duplicate-parent", Req: "RS-3.1-2"},
		arts: with(chain()[:2],
			checkin("m", 3, of("c2", "c1", "c1")), checkin("n", 4, of("m", "m"))),
		tables:   []string{"plink"},
		want:     rows("plink", "c1>c2 1 @2 -", "c2>m 1 @3 -", "c1>m 0 @3 -", "m>n 1 @4 -"),
		ignoring: []string{"-plink: c1>m 0 @3 -"},
		rebuild:  true,
	}, {
		// n names m twice; the second occurrence must not clear the flag
		// the first set.
		Case: Case{ID: "repostate.3.2.duplicate-primary", Req: "RS-3.2-2"},
		arts: with(chain()[:2],
			checkin("m", 3, of("c2", "c1", "c1")), checkin("n", 4, of("m", "m"))),
		tables:   []string{"plink"},
		want:     rows("plink", "c1>c2 1 @2 -", "c2>m 1 @3 -", "c1>m 0 @3 -", "m>n 1 @4 -"),
		ignoring: []string{"-plink: m>n 1 @4 -", "+plink: m>n 0 @4 -"},
		rebuild:  true,
	}, {
		// c2 is a delta manifest: its link carries c1 as the baseline and
		// c2's own time. The files are loaded too: go-libfossil defers a
		// check-in whose files are missing.
		Case: Case{ID: "repostate.3.1.link-metadata", Req: "RS-3.1-1"},
		arts: []rsArt{
			file("fa", "a\n"), file("fb", "b\n"),
			literal("c1", "C c1\nD "+rsTime(1)+"\nF a.txt {fa}\n"+
				"T *branch * trunk\nT *sym-trunk *\nU u\n"),
			literal("c2", "B {c1}\nC c2\nD "+rsTime(2)+"\nF b.txt {fb}\nP {c1}\nU u\n"),
		},
		tables:   []string{"plink"},
		want:     rows("plink", "c1>c2 1 @2 c1"),
		ignoring: []string{"-plink: c1>c2 1 @2 c1", "+plink: c1>c2 1 @2 -"},
		rebuild:  true,
	}}
}

func crosslinkCases() []rsCase {
	return []rsCase{{
		Case:          Case{ID: "repostate.4.2.phantom-parent", Req: "RS-4.2-2"},
		arts:          []rsArt{checkin("c2", 2, of("c1"))},
		tables:        []string{"phantom", "plink"},
		want:          join(rows("phantom", "c1"), rows("plink", "c1>c2 1 @2 -")),
		ignoring:      []string{"-phantom: c1", "-plink: c1>c2 1 @2 -"},
		rebuild:       true,
		rebuildWrites: "the phantom root has no branch tag, so fossil rebuild tags it trunk",
	}, {
		// The shape of a foreign check-in imported without its history (#398):
		// neither its parent nor its file is stored. The absent parent still
		// yields the linkage row; with no parent manifest there is no file
		// transition to record, so the absent file blocks nothing.
		Case:   Case{ID: "repostate.4.2.phantom-parent-absent-file", Req: "RS-4.2-2"},
		arts:   []rsArt{literal("c2", "C c2\nD "+rsTime(2)+"\nF a.txt {fa}\nP {c1}\nU u\n")},
		tables: []string{"event", "mlink", "phantom", "plink"},
		want: join(rows("event", "c2 ci @2"), rows("phantom", "c1"),
			rows("plink", "c1>c2 1 @2 -")),
		ignoring:      []string{"-event: c2 ci @2", "-phantom: c1", "-plink: c1>c2 1 @2 -"},
		rebuild:       true,
		rebuildWrites: "the phantom root has no branch tag, so fossil rebuild tags it trunk",
	}, {
		// A root check-in whose file is absent: fossil links it against a
		// phantom file; go-libfossil still defers it (#406).
		Case:     Case{ID: "repostate.4.2.root-absent-file", Req: "RS-4.2-2"},
		arts:     []rsArt{literal("c1", "C c1\nD "+rsTime(1)+"\nF a.txt {fa}\nU u\n")},
		tables:   []string{"event"},
		want:     rows("event", "c1 ci @1"),
		ignoring: []string{"-event: c1 ci @1"},
	}, {
		Case:     Case{ID: "repostate.4.2.phantom-target", Req: "RS-4.2-1"},
		arts:     with(chain()[:2], control("t", 9, "*foo {c3} v")),
		tables:   []string{"phantom", "tagxref"},
		tags:     []string{"foo"},
		want:     join(rows("phantom", "c3"), rows("tagxref", "foo c3 2 t c3 'v' @9")),
		ignoring: []string{"-phantom: c3", "-tagxref: foo c3 2 t c3 'v' @9"},
		rebuild:  true,
	}, {
		// The tag waits on phantom c3, then reaches c4 once both arrive.
		Case:   Case{ID: "repostate.4.2.phantom-late-target", Req: "RS-4.2-1"},
		arts:   with(chain()[:2], control("t", 9, "*foo {c3} v"), chain()[2], chain()[3]),
		tables: []string{"phantom", "plink", "tagxref"},
		tags:   []string{"foo"},
		want: join(chainPlink,
			rows("tagxref", "foo c3 2 t c3 'v' @9", "foo c4 2 0 c3 'v' @9")),
		ignoring: []string{"-tagxref: foo c4 2 0 c3 'v' @9", "+phantom: c3"},
		rebuild:  true,
	}, {
		Case:     Case{ID: "repostate.4.2.technote-tags", Req: "RS-4.2-3"},
		arts:     with(chain()[:2], technote("e", 6, "+bgcolor * red")),
		tables:   []string{"tagxref"},
		tags:     []string{"bgcolor"},
		want:     rows("tagxref", "bgcolor e 1 e e 'red' @6"),
		ignoring: []string{"-tagxref: bgcolor e 1 e e 'red' @6"},
		rebuild:  true,
	}, {
		// fossil(1) dates the cluster tag with the wall clock, so its
		// mtime is not compared.
		Case:     Case{ID: "repostate.4.2.cluster", Req: "RS-4.2-4"},
		arts:     with(chain()[:2], literal("k", "M {c1}\nM {c2}\n")),
		tables:   []string{"tagxref"},
		tags:     []string{"cluster"},
		want:     rows("tagxref", "cluster k 1 k k NULL *"),
		ignoring: []string{"-tagxref: cluster k 1 k k NULL *"},
		rebuild:  true,
	}, {
		// c3 and c4 arrive after the tag on c1 and inherit it.
		Case:   Case{ID: "repostate.4.2.late-child", Req: "RS-4.2-5"},
		arts:   with(chain()[:2], control("t", 9, "*foo {c1} v"), chain()[2], chain()[3]),
		tables: []string{"tagxref"},
		tags:   []string{"foo"},
		want: rows("tagxref", "foo c1 2 t c1 'v' @9", "foo c2 2 0 c1 'v' @9",
			"foo c3 2 0 c1 'v' @9", "foo c4 2 0 c1 'v' @9"),
		ignoring: []string{"-tagxref: foo c3 2 0 c1 'v' @9", "-tagxref: foo c4 2 0 c1 'v' @9"},
		rebuild:  true,
	}, {
		Case:   Case{ID: "repostate.4.2.propagate-on-tag", Req: "RS-4.2-6"},
		arts:   with(chain(), control("t", 9, "*foo {c2} v")),
		tables: []string{"tagxref"},
		tags:   []string{"foo"},
		want: rows("tagxref", "foo c2 2 t c2 'v' @9", "foo c3 2 0 c2 'v' @9",
			"foo c4 2 0 c2 'v' @9"),
		ignoring: []string{"-tagxref: foo c3 2 0 c2 'v' @9"},
		rebuild:  true,
	}, {
		// A singleton walks as a cancel: c4 loses the foo it inherited.
		Case:   Case{ID: "repostate.4.2.singleton-walks-as-cancel", Req: "RS-4.2-6"},
		arts:   with(chain(), control("t1", 8, "*foo {c1} v"), control("t2", 9, "+foo {c3} s")),
		tables: []string{"tagxref"},
		tags:   []string{"foo"},
		want: rows("tagxref", "foo c1 2 t1 c1 'v' @8", "foo c2 2 0 c1 'v' @8",
			"foo c3 1 t2 c3 's' @9"),
		ignoring: []string{"+tagxref: foo c4 2 0 c1 'v' @8"},
		rebuild:  true,
	}, reparentCase(), reparentFirstCase()}
}

// reparentCase moves c3 from c2 to c1 with a parent tag, which leaves c2
// without a child, so a leaf.
func reparentCase() rsCase {
	return rsCase{
		Case:   Case{ID: "repostate.4.2.reparent", Req: "RS-4.2-8"},
		arts:   with(chain(), control("t", 9, "+parent {c3} {c1}")),
		tables: []string{"leaf", "plink", "tagxref"},
		tags:   []string{"parent"},
		want: join(rows("plink", "c1>c2 1 @2 -", "c1>c3 1 @3 -", "c3>c4 1 @4 -"),
			rows("leaf", "c2", "c4"), rows("tagxref", "parent c3 1 t c3 '{c1}' @9")),
		ignoring: []string{"-plink: c1>c3 1 @3 -", "+plink: c2>c3 1 @3 -", "-leaf: c2"},
		rebuild:  true,
		// fossil's per-artifact crosslink rewrites plink but does not
		// recheck c2, the old parent; its rebuild does, as Section 7.3
		// requires.
		incremental: "fossil's per-artifact crosslink leaves c2, the old parent, out of leaf",
	}
}

// reparentFirstCase is reparentCase with the parent tag arriving before c3,
// its target. Linking c3 afterwards writes c3's P-card link, c2 to c3; the
// recorded reparent, re-applied when the batch ends, replaces it.
func reparentFirstCase() rsCase {
	c := reparentCase()
	c.ID = "repostate.4.2.reparent-before-target"
	c.arts = with(chain()[:2], control("t", 9, "+parent {c3} {c1}"), chain()[2], chain()[3])
	// Section 4.2 re-applies a reparent at the end of the batch that
	// recorded it. fossil's test-crosslink links each artifact on its own,
	// so t and c3 never share a batch there; its rebuild links them in one.
	c.incremental = "fossil's per-artifact crosslink puts t and c3 in separate batches, " +
		"so the reparent recorded with t is not re-applied once c3 arrives"
	return c
}

// rejectionCases are artifacts the T-card rules reject: they derive
// nothing, so the rows are those of the rest of the script.
func rejectionCases() []rsCase {
	twoLinks := rows("plink", "c1>c2 1 @2 -")
	rejected := func(id, req string, x rsArt, tags ...string) rsCase {
		return rsCase{
			Case:     Case{ID: id, Req: req},
			arts:     with(chain()[:2], x),
			tables:   []string{"plink", "tagxref"},
			tags:     tags,
			want:     twoLinks,
			ignoring: []string{"+plink: c2>x 1 @5 -"},
			rebuild:  true,
		}
	}
	unsortedCheckin := literal("x", "C x\nD "+rsTime(5)+"\nP {c2}\nT *zz *\nT *ax *\nU u\n")
	unsortedControl := literal("x", "D "+rsTime(5)+"\nT *zz {c1} 1\nT *ax {c1} 1\nU u\n")
	cases := []rsCase{
		rejected("repostate.4.1.no-prefix", "RS-4.1-1",
			literal("x", "C x\nD "+rsTime(5)+"\nP {c2}\nT foo *\nU u\n"), "foo"),
		rejected("repostate.4.1.hex-name", "RS-4.1-2",
			literal("x", "C x\nD "+rsTime(5)+"\nP {c2}\nT *beef *\nU u\n"), "beef"),
		rejected("repostate.4.1.unsorted", "RS-4.1-3", unsortedCheckin, "ax", "zz"),
		rejected("repostate.4.3.no-effect", "RS-4.3-1", unsortedCheckin, "ax", "zz"),
		rejected("repostate.4.1.no-partial", "RS-4.1-4", unsortedControl, "ax", "zz"),
		rejected("repostate.4.4.opaque-control", "RS-4.4-2",
			literal("x", "D "+rsTime(9)+"\nT *foo {c1} v\n"), "foo"),
	}
	// A control artifact derives no link; ignoring the rules shows as tags.
	cases[4].ignoring = []string{"+tagxref: ax c1 2 x c1 '1' @5"}
	cases[5].ignoring = []string{"+tagxref: foo c1 2 x c1 'v' @9"}
	return append(cases, rsCase{
		// Assembly goes on past the opaque artifact: c3 and c4 still link.
		Case: Case{ID: "repostate.4.4.opaque-load-succeeds", Req: "RS-4.4-1"},
		arts: with(chain()[:2], literal("x", "D "+rsTime(5)+"\nT *foo {c1} v\n"),
			chain()[2], chain()[3]),
		tables:   []string{"leaf", "plink"},
		want:     join(chainPlink, chainLeaf),
		ignoring: []string{"-leaf: c4"},
		rebuild:  true,
	}, rsCase{
		Case:     Case{ID: "repostate.12.technote-non-self", Req: "RS-12-1"},
		arts:     with(chain()[:2], technote("e", 6, "*foo {c1} v")),
		tables:   []string{"event", "tagxref"},
		tags:     []string{"foo"},
		want:     rows("event", "c1 ci @1", "c2 ci @2"),
		ignoring: []string{"+event: e e @6", "+tagxref: foo c1 2 e c1 'v' @6"},
		rebuild:  true,
	})
}
