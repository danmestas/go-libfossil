package conformance

import "testing"

// The bgcolor, user and comment tags override a check-in's derived event row
// (bgcolor, euser, ecomment), whatever the tag's type, and a cancel clears
// the column. fossil(1) does this in tag_insert when the tag arrives after
// the check-in and in manifest_crosslink when it arrived first, while the
// check-in was still a phantom. draft-fossil-repo-state-01 does not describe
// these columns, so the cases name no requirement and stay out of the
// coverage audit, with fossil(1) as their reference.

// eventTagCase compares the bgcolor, euser and ecomment of the check-ins.
func eventTagCase(id string, arts []rsArt, want []string, ignoring ...string) rsCase {
	return rsCase{
		Case:     Case{ID: id},
		arts:     arts,
		tables:   []string{"eventcols"},
		want:     rows("eventcols", want...),
		ignoring: ignoring,
		rebuild:  true,
		each:     true,
	}
}

func eventTagCases() []rsCase {
	base := chain()[:2]
	none := "NULL NULL NULL"
	return []rsCase{
		eventTagCase("repostate.fossil.bgcolor-singleton",
			with(base, control("t", 9, "+bgcolor {c2} red")),
			[]string{"c1 " + none, "c2 'red' NULL NULL"},
			"-eventcols: c2 'red' NULL NULL", "+eventcols: c2 "+none),
		eventTagCase("repostate.fossil.bgcolor-propagating",
			with(chain(), control("t", 9, "*bgcolor {c2} red")),
			[]string{"c1 " + none, "c2 'red' NULL NULL", "c3 'red' NULL NULL",
				"c4 'red' NULL NULL"},
			"-eventcols: c3 'red' NULL NULL", "+eventcols: c3 "+none),
		eventTagCase("repostate.fossil.bgcolor-cancel",
			with(chain()[:3], control("t1", 8, "*bgcolor {c2} red"),
				control("t2", 9, "-bgcolor {c2}")),
			[]string{"c1 " + none, "c2 " + none, "c3 " + none},
			"-eventcols: c2 "+none, "+eventcols: c2 'red' NULL NULL"),
		eventTagCase("repostate.fossil.user-tag",
			with(base, control("t", 9, "+user {c2} bob")),
			[]string{"c1 " + none, "c2 NULL 'bob' NULL"},
			"-eventcols: c2 NULL 'bob' NULL", "+eventcols: c2 "+none),
		eventTagCase("repostate.fossil.comment-tag",
			with(base, control("t", 9, "+comment {c2} reworded")),
			[]string{"c1 " + none, "c2 NULL NULL 'reworded'"},
			"-eventcols: c2 NULL NULL 'reworded'", "+eventcols: c2 "+none),
	}
}

func TestRepoStateEventTags(t *testing.T) {
	runRepoStateCases(t, eventTagCases())
}

func TestRepoStateEventTagsReasons(t *testing.T) {
	checkRepoStateReasons(t, eventTagCases())
}
