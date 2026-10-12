package conformance

import "testing"

// The date tag is fossil(1)'s check-in time override (`fossil amend
// --date`): its value, not the manifest's D card, sets the check-in's
// event.mtime, and the newest date tag wins. That contradicts
// draft-fossil-repo-state-00, whose Section 1 has the manifest date carry
// the check-in mtime and orders by the event.mtime derived from it;
// draft-fossil-repo-state-01 adds the override (Section 4.2, step 6, "Event
// time"). Until the suite is re-pinned to the -01 drafts these cases name no
// requirement and stay out of the coverage audit, with fossil(1) as their
// reference; they get their requirement ID with that re-pin.

// dateCase compares the event rows of c1 and c2, whose second check-in a
// date tag may move, with the control artifacts' own rows.
func dateCase(id string, arts []rsArt, want []string, ignoring ...string) rsCase {
	return rsCase{
		Case:     Case{ID: id},
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
	tagFirstCase := dateCase("repostate.fossil.date-tag-before-checkin", tagFirst,
		[]string{"c1 ci @1", "c2 ci @30", "t g @9"}, "-event: c2 ci @30", "+event: c2 ci @2")
	tagFirstCase.each = true
	return []rsCase{
		dateCase("repostate.fossil.date-tag-sets-mtime",
			with(twoCheckins, control("t", 9, "+date {c2} "+rsTime(30))),
			[]string{"c1 ci @1", "c2 ci @30", "t g @9"},
			"-event: c2 ci @30", "+event: c2 ci @2"),
		tagFirstCase,
		dateCase("repostate.fossil.date-tag-newest-wins",
			with(twoCheckins, control("t1", 9, "+date {c2} "+rsTime(40)),
				control("t2", 8, "+date {c2} "+rsTime(30))),
			[]string{"c1 ci @1", "c2 ci @40", "t1 g @9", "t2 g @8"},
			"-event: c2 ci @40", "+event: c2 ci @30"),
	}
}

func TestRepoStateDateTag(t *testing.T) {
	runRepoStateCases(t, dateTagCases())
}

func TestRepoStateDateTagReasons(t *testing.T) {
	checkRepoStateReasons(t, dateTagCases())
}
