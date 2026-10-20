package conformance

// The date tag is fossil(1)'s check-in time override (`fossil amend
// --date`): draft-fossil-repo-state-01 Section 4.2, step 6, "Event time",
// makes its value, not the manifest's D card, the check-in's event.mtime,
// with the newest date tag winning. Where the step defines a cancel, an
// unparsable value or a value of 0 differently from fossil(1) 2.28, which
// stores no time, or a time of 0, once the check-in is crosslinked (the
// draft's note on step 6), the case follows the draft and says so in its
// fossil field.

// dateCase compares the event rows of c1 and c2, whose second check-in a
// date tag may move, with the control artifacts' own rows.
func dateCase(id, req string, arts []rsArt, want []string, ignoring ...string) rsCase {
	return rsCase{
		Case:     Case{ID: id, Req: req},
		arts:     arts,
		tables:   []string{"event"},
		want:     rows("event", want...),
		ignoring: ignoring,
		rebuild:  true,
	}
}

func dateTagCases() []rsCase {
	twoCheckins := chain()[:2]
	tagFirst := []rsArt{chain()[0], control("t", 9, "+date {c2} "+rsTime(30)), chain()[1]}
	tagFirstCase := dateCase("repostate.4.2.date-tag-before-checkin", "RS-4.2-10", tagFirst,
		[]string{"c1 ci @1", "c2 ci @30", "t g @9"}, "-event: c2 ci @30", "+event: c2 ci @2")
	tagFirstCase.each = true
	return append([]rsCase{
		dateCase("repostate.4.2.date-tag-sets-mtime", "RS-4.2-9",
			with(twoCheckins, control("t", 9, "+date {c2} "+rsTime(30))),
			[]string{"c1 ci @1", "c2 ci @30", "t g @9"},
			"-event: c2 ci @30", "+event: c2 ci @2"),
		tagFirstCase,
		dateCase("repostate.4.2.date-tag-newest-wins", "RS-4.2-11",
			with(twoCheckins, control("t1", 9, "+date {c2} "+rsTime(40)),
				control("t2", 8, "+date {c2} "+rsTime(30))),
			[]string{"c1 ci @1", "c2 ci @40", "t1 g @9", "t2 g @8"},
			"-event: c2 ci @40", "+event: c2 ci @30"),
		// The tag reaches c3 and c4 in tagxref, but their times stay theirs.
		dateCase("repostate.4.2.date-tag-propagating", "RS-4.2-12",
			with(chain(), control("t", 9, "*date {c2} "+rsTime(30))),
			[]string{"c1 ci @1", "c2 ci @30", "c3 ci @3", "c4 ci @4", "t g @9"},
			"-event: c3 ci @3", "+event: c3 ci @30"),
		{
			Case:     Case{ID: "repostate.4.2.date-tag-keeps-omtime", Req: "RS-4.2-15"},
			arts:     with(twoCheckins, control("t", 9, "+date {c2} "+rsTime(30))),
			tables:   []string{"omtime"},
			want:     rows("omtime", "c1 @1", "c2 @2"),
			ignoring: []string{"-omtime: c2 @2", "+omtime: c2 @30"},
			rebuild:  true,
		},
	}, append(append(dateTagFallbackCases(), dateTagFirstFallbackCases()...),
		dateArrivalCases()...)...)
}

// dateArrivalCases are Section 5.3, step 1's arrival order: c1's propagating
// date tag (t1, @9) is newer than c2's direct one (t2, @8). The direct tag
// applied first keeps its value and stops the propagation at c2, which it
// retimes; the propagation arriving first leaves an inherited row on c2 that
// makes the direct tag skipped, and an inherited date tag retimes nothing,
// so c2 keeps its D-card time. Each is loaded in its own order, and rebuilt
// in it, since a rebuild crosslinks in storage order.
func dateArrivalCases() []rsCase {
	arrival := func(id string, tags []rsArt, want []string, ignoring ...string) rsCase {
		return rsCase{
			Case:     Case{ID: id, Req: "RS-5.3-5"},
			arts:     append(chain()[:2], tags...),
			tables:   []string{"event", "tagxref"},
			tags:     []string{"date"},
			want:     want,
			ignoring: ignoring,
			each:     true,
			rebuild:  true,
		}
	}
	inherited := control("t1", 9, "*date {c1} "+rsTime(30))
	direct := control("t2", 8, "+date {c2} "+rsTime(40))
	c1Rows := []string{"event: c1 ci @30", "event: t1 g @9", "event: t2 g @8",
		"tagxref: date c1 2 t1 c1 '" + rsTime(30) + "' @9"}
	return []rsCase{
		arrival("repostate.5.3.direct-before-inherited", []rsArt{direct, inherited},
			append([]string{"event: c2 ci @40",
				"tagxref: date c2 1 t2 c2 '" + rsTime(40) + "' @8"}, c1Rows...),
			"-tagxref: date c2 1 t2 c2 '"+rsTime(40)+"' @8",
			"+tagxref: date c2 2 0 c1 '"+rsTime(30)+"' @9"),
		arrival("repostate.5.3.inherited-before-direct", []rsArt{inherited, direct},
			append([]string{"event: c2 ci @2",
				"tagxref: date c2 2 0 c1 '" + rsTime(30) + "' @9"}, c1Rows...),
			"-tagxref: date c2 2 0 c1 '"+rsTime(30)+"' @9",
			"+tagxref: date c2 1 t2 c2 '"+rsTime(40)+"' @8", "-event: c2 ci @2",
			"+event: c2 ci @40"),
	}
}

// dateTagFallbackCases are the date tags step 6 ignores, leaving the D-card
// time, where fossil(1) stores no time or a time of 0 instead.
func dateTagFallbackCases() []rsCase {
	twoCheckins := chain()[:2]
	cancel := dateCase("repostate.4.2.date-tag-cancel", "RS-4.2-13",
		with(twoCheckins, control("t1", 8, "+date {c2} "+rsTime(30)),
			control("t2", 9, "-date {c2}")),
		[]string{"c1 ci @1", "c2 ci @2", "t1 g @8", "t2 g @9"},
		"-event: c2 ci @2", "+event: c2 ci <nil>")
	cancel.fossil = "fossil(1) 2.28 tag_insert writes julianday(NULL) for a cancel " +
		"applied after crosslink, leaving c2 with no time where step 6 restores " +
		"its D-card time (4.2 step 6, note)"
	unparsable := dateCase("repostate.4.2.date-tag-unparsable", "RS-4.2-14",
		with(twoCheckins, control("t", 9, "+date {c2} not-a-date")),
		[]string{"c1 ci @1", "c2 ci @2", "t g @9"},
		"-event: c2 ci @2", "+event: c2 ci <nil>")
	unparsable.fossil = "fossil(1) 2.28 stores julianday('not-a-date'), NULL, as c2's " +
		"time once it is crosslinked, where step 6 ignores the tag and keeps the " +
		"D-card time (4.2 step 6, note)"
	zero := dateCase("repostate.4.2.date-tag-zero", "RS-4.2-14",
		with(twoCheckins, control("t", 9, "+date {c2} 0")),
		[]string{"c1 ci @1", "c2 ci @2", "t g @9"},
		"-event: c2 ci @2", "+event: c2 ci -210866760000000")
	zero.fossil = "fossil(1) 2.28 stores julianday('0'), a time of 0, with no " +
		"positivity check, where step 6 ignores a time that is not positive and " +
		"keeps the D-card time (4.2 step 6, note)"
	return []rsCase{cancel, unparsable, zero}
}

// dateTagFirstFallbackCases are dateTagFallbackCases with the tags arriving
// while c2 is a phantom: step 6 gives the same D-card time in either order.
// They have no ".rebuild" case, which would load the same artifacts as the
// cases above; for the cancel, fossil's own rebuild disagrees with its
// per-artifact crosslink, storing no time.
func dateTagFirstFallbackCases() []rsCase {
	first := func(id, req string, tags []rsArt, want []string, ignoring ...string) rsCase {
		arts := append(append([]rsArt{chain()[0]}, tags...), chain()[1])
		c := dateCase(id, req, arts, append([]string{"c1 ci @1", "c2 ci @2"}, want...),
			ignoring...)
		c.each, c.rebuild = true, false
		return c
	}
	zero := first("repostate.4.2.date-tag-zero-before-checkin", "RS-4.2-14",
		[]rsArt{control("t", 9, "+date {c2} 0")},
		[]string{"t g @9"}, "-event: c2 ci @2", "+event: c2 ci -210866760000000")
	zero.fossil = "fossil(1) 2.28 manifest_crosslink reads julianday('0'), a time of 0, " +
		"where step 6 ignores a time that is not positive and keeps the D-card time " +
		"(4.2 step 6, note)"
	return []rsCase{
		first("repostate.4.2.date-tag-cancel-before-checkin", "RS-4.2-13",
			[]rsArt{control("t1", 8, "+date {c2} "+rsTime(30)), control("t2", 9, "-date {c2}")},
			[]string{"t1 g @8", "t2 g @9"}, "-event: c2 ci @2", "+event: c2 ci @30"),
		first("repostate.4.2.date-tag-unparsable-before-checkin", "RS-4.2-14",
			[]rsArt{control("t", 9, "+date {c2} not-a-date")},
			[]string{"t g @9"}, "-event: c2 ci @2", "+event: c2 ci <nil>"),
		zero,
	}
}
