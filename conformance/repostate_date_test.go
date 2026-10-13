package conformance

// The date tag is fossil(1)'s check-in time override (`fossil amend
// --date`): draft-fossil-repo-state-01 Section 4.2, step 6, "Event time",
// makes its value, not the manifest's D card, the check-in's event.mtime,
// with the newest date tag winning. Where the step defines a cancel, an
// unparsable value or a value of 0 differently from fossil(1) 2.28, which
// stores no time, or a time of 0, once the check-in is crosslinked (erratum
// repo-state-01-s4.2-date-tag-event-time), the case follows the draft and
// says so in its fossil field.

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
	}, dateTagFallbackCases()...)
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
		"applied after crosslink, leaving c2 with no time (erratum " +
		"repo-state-01-s4.2-date-tag-event-time)"
	unparsable := dateCase("repostate.4.2.date-tag-unparsable", "RS-4.2-14",
		with(twoCheckins, control("t", 9, "+date {c2} not-a-date")),
		[]string{"c1 ci @1", "c2 ci @2", "t g @9"},
		"-event: c2 ci @2", "+event: c2 ci <nil>")
	unparsable.fossil = "fossil(1) 2.28 stores julianday('not-a-date'), NULL, as c2's " +
		"time once it is crosslinked (erratum repo-state-01-s4.2-date-tag-event-time)"
	zero := dateCase("repostate.4.2.date-tag-zero", "RS-4.2-14",
		with(twoCheckins, control("t", 9, "+date {c2} 0")),
		[]string{"c1 ci @1", "c2 ci @2", "t g @9"},
		"-event: c2 ci @2", "+event: c2 ci -210866760000000")
	zero.fossil = "fossil(1) 2.28 stores julianday('0'), a time of 0, with no " +
		"positivity check (erratum repo-state-01-s4.2-date-tag-event-time)"
	return []rsCase{cancel, unparsable, zero}
}
