package conformance

import "strings"

// Derived-state cases for tag types, direct application, propagation and
// blocking (Section 5), all on chain: c1 to c4.

// foo is a case comparing only the foo tag's rows.
func foo(id, req string, arts []rsArt, want []string, ignoring ...string) rsCase {
	return rsCase{
		Case:     Case{ID: id, Req: req},
		arts:     arts,
		tables:   []string{"tagxref"},
		tags:     []string{"foo"},
		want:     rows("tagxref", want...),
		ignoring: ignoring,
		rebuild:  true,
	}
}

// fooOn is what *foo on c2 at second 9 derives: c2's own row, inherited
// by c3 and c4.
var fooOn = []string{"foo c2 2 t c2 'v' @9", "foo c3 2 0 c2 'v' @9", "foo c4 2 0 c2 'v' @9"}

func tagTypeCases() []rsCase {
	typed := rsCase{
		Case:   Case{ID: "repostate.5.1.type-codes", Req: "RS-5.1-1"},
		arts:   with(chain(), control("t", 9, "*xyz {c2}", "+yes {c2}", "-zap {c2}")),
		tables: []string{"tagxref"},
		tags:   []string{"xyz", "yes", "zap"},
		want: rows("tagxref", "zap c2 0 t c2 NULL @9", "yes c2 1 t c2 NULL @9",
			"xyz c2 2 t c2 NULL @9", "xyz c3 2 0 c2 NULL @9", "xyz c4 2 0 c2 NULL @9"),
		ignoring: []string{"-tagxref: yes c2 1 t c2 NULL @9", "+tagxref: yes c2 2 t c2 NULL @9"},
		rebuild:  true,
	}
	badType := rsCase{
		Case:   Case{ID: "repostate.5.1.bad-prefix", Req: "RS-5.1-2"},
		arts:   with(chain()[:2], literal("t", "D "+rsTime(9)+"\nT ~x {c2}\nU u\n")),
		tables: []string{"event", "tagxref"},
		tags:   []string{"x", "~x"},
		want:   rows("event", "c1 ci @1", "c2 ci @2"),
		// Read as any type, ~ would give a row for x or ~x on c2.
		ignoring: []string{"+event: t g @9", "+tagxref: x c2 1 t c2 NULL @9"},
		rebuild:  true,
	}
	return []rsCase{typed, badType,
		foo("repostate.5.2.one-row", "RS-5.2-1",
			with(chain(), control("t1", 8, "*foo {c2} a"), control("t2", 9, "*foo {c2} v")),
			[]string{"foo c2 2 t2 c2 'v' @9", "foo c3 2 0 c2 'v' @9", "foo c4 2 0 c2 'v' @9"},
			"+tagxref: foo c2 2 t1 c2 'a' @8"),
		foo("repostate.5.3.older-tag-skipped", "RS-5.3-1",
			with(chain(), control("t1", 9, "*foo {c2} v"), control("t2", 8, "*foo {c2} old")),
			[]string{"foo c2 2 t1 c2 'v' @9", "foo c3 2 0 c2 'v' @9", "foo c4 2 0 c2 'v' @9"},
			"-tagxref: foo c2 2 t1 c2 'v' @9", "+tagxref: foo c2 2 t2 c2 'old' @8"),
		foo("repostate.5.3.cancel-older-than-inherited", "RS-5.3-1",
			with(chain(), control("t1", 9, "*foo {c1} v"), control("t2", 8, "-foo {c2}")),
			[]string{"foo c1 2 t1 c1 'v' @9", "foo c2 2 0 c1 'v' @9",
				"foo c3 2 0 c1 'v' @9", "foo c4 2 0 c1 'v' @9"},
			"-tagxref: foo c2 2 0 c1 'v' @9", "+tagxref: foo c2 0 t2 c2 NULL @8"),
		equalFirstWins(),
		foo("repostate.5.3.direct-row-shape", "RS-5.3-2",
			with(chain(), control("t", 9, "*foo {c2} v")), fooOn,
			"-tagxref: foo c2 2 t c2 'v' @9", "+tagxref: foo c2 2 0 c2 'v' @9"),
		foo("repostate.5.3.cancel-no-value", "RS-5.3-3",
			with(chain(), control("t1", 8, "*foo {c2} v"), control("t2", 9, "-foo {c2}")),
			[]string{"foo c2 0 t2 c2 NULL @9"},
			"-tagxref: foo c2 0 t2 c2 NULL @9", "+tagxref: foo c2 0 t2 c2 'v' @9"),
		// The cancel card's own value stays on the row it writes, as
		// fossil's tag_insert stores it.
		foo("repostate.5.3.cancel-keeps-value", "RS-5.3-3",
			with(chain(), control("t1", 8, "*foo {c2} v"), control("t2", 9, "-foo {c2} why")),
			[]string{"foo c2 0 t2 c2 'why' @9"},
			"-tagxref: foo c2 0 t2 c2 'why' @9", "+tagxref: foo c2 0 t2 c2 NULL @9"),
		cancelValueNotCached(),
		// Both cards are on t at second 9; the * card sorts before the -
		// card, writes the row, and the cancel meets an equal mtime.
		foo("repostate.5.3.card-order", "RS-5.3-7",
			with(chain(), control("t", 9, "*foo {c2} v", "-foo {c2}")), fooOn,
			"-tagxref: foo c2 2 t c2 'v' @9", "+tagxref: foo c2 0 t c2 NULL @9",
			"-tagxref: foo c3 2 0 c2 'v' @9", "-tagxref: foo c4 2 0 c2 'v' @9"),
		foo("repostate.5.3.singleton-local", "RS-5.3-4",
			with(chain(), control("t1", 8, "*foo {c1} v"), control("t2", 9, "+foo {c2} s")),
			[]string{"foo c1 2 t1 c1 'v' @8", "foo c2 1 t2 c2 's' @9"},
			"+tagxref: foo c3 2 0 c1 'v' @8"),
	}
}

// equalFirstWins applies two tags with equal times. Section 5.3 leaves the
// winner implementation-defined; fossil(1) keeps the first, and so does
// the case.
func equalFirstWins() rsCase {
	c := foo("repostate.5.3.equal-tag-first-wins", "RS-5.3-1",
		with(chain(), control("t1", 9, "*foo {c2} v"), control("t2", 9, "*foo {c2} second")),
		[]string{"foo c2 2 t1 c2 'v' @9", "foo c3 2 0 c2 'v' @9", "foo c4 2 0 c2 'v' @9"},
		"-tagxref: foo c2 2 t1 c2 'v' @9", "+tagxref: foo c2 2 t2 c2 'second' @9")
	c.each, c.rebuild = true, false
	return c
}

// fooFromC1 is *foo on c1 at second 9, inherited down the chain.
func fooFromC1() rsArt { return control("t", 9, "*foo {c1} v") }

func propagationCases() []rsCase {
	return []rsCase{
		foo("repostate.5.4.merge-no-propagation", "RS-5.4-1",
			with(merged(), control("t", 9, "*foo {b1} v")),
			[]string{"foo b1 2 t b1 'v' @9"}, "+tagxref: foo m 2 0 b1 'v' @9"),
		repropagateSingleton(),
		foo("repostate.5.4.inherited-tag", "RS-5.4-5",
			with(chain(), control("t", 9, "*foo {c2} v")), fooOn,
			"-tagxref: foo c3 2 0 c2 'v' @9", "+tagxref: foo c3 2 0 c3 'v' @3"),
		foo("repostate.5.4.walk-transitive", "RS-5.4-7",
			with(chain(), control("t", 9, "*foo {c2} v")), fooOn,
			"-tagxref: foo c4 2 0 c2 'v' @9"),
		foo("repostate.5.4.cancel-propagation", "RS-5.4-6",
			with(chain(), control("t1", 8, "*foo {c1} v"), control("t2", 9, "-foo {c3}")),
			[]string{"foo c1 2 t1 c1 'v' @8", "foo c2 2 0 c1 'v' @8", "foo c3 0 t2 c3 NULL @9"},
			"+tagxref: foo c4 2 0 c1 'v' @8"),
		doctored("repostate.5.4.equal-mtime-retained", "RS-5.4-3",
			[]rsArt{control("t1", 8, "*foo {c1} a"), control("t2", 9, "*foo {c2} b")},
			"UPDATE tagxref SET mtime = julianday('"+rsTime(9)+"')", "c3", "c4")(
			[]string{"foo c1 2 t1 c1 'a' @8", "foo c2 2 t2 c2 'b' @9",
				"foo c3 2 0 c1 'a' @9", "foo c4 2 0 c1 'a' @9"},
			"-tagxref: foo c3 2 0 c1 'a' @9", "+tagxref: foo c3 2 0 c2 'b' @9"),
		// Section 5.4, step 3: a cancel walk halts at c3, which has no foo
		// row, so c4 keeps its row, as in fossil 2.28.
		doctored("repostate.5.4.cancel-rowless", "RS-5.4-4",
			[]rsArt{fooFromC1(), control("x", 20, "-foo {c1}")},
			"DELETE FROM tagxref", "c3")(
			[]string{"foo c1 0 x c1 NULL @20", "foo c4 2 0 c1 'v' @9"},
			"-tagxref: foo c4 2 0 c1 'v' @9"),
		doctored("repostate.5.4.cancel-skips-newer-inherited", "RS-5.4-3",
			[]rsArt{fooFromC1(), control("x", 20, "-foo {c1}")},
			"UPDATE tagxref SET mtime = 2500000", "c3", "c4")(
			[]string{"foo c1 0 x c1 NULL @20", "foo c3 2 0 c1 'v' 5133240000000",
				"foo c4 2 0 c1 'v' 5133240000000"},
			"-tagxref: foo c3 2 0 c1 'v' 5133240000000",
			"-tagxref: foo c4 2 0 c1 'v' 5133240000000"),
	}
}

// repropagateSingleton adds c5 under c2 after a singleton on c2: crosslinking
// c5 re-propagates c2's tags, and the singleton must reach c5 as a cancel.
func repropagateSingleton() rsCase {
	c := foo("repostate.5.4.repropagate-singleton", "RS-5.4-2",
		with(chain()[:2], control("t1", 8, "*foo {c1} v"), control("t2", 9, "+foo {c2} s"),
			checkin("c5", 5, of("c2"))),
		[]string{"foo c1 2 t1 c1 'v' @8", "foo c2 1 t2 c2 's' @9"},
		"+tagxref: foo c5 2 0 c2 's' @9")
	c.each = true
	return c
}

// doctored loads chain and the first of more, then edits the foo rows of
// the check-ins named in on with edit, an UPDATE or DELETE of tagxref, to
// reach a state no known artifact sequence reaches, and loads the rest of
// more one at a time. Edited rows stay consistent along the chain: an
// inherited row newer than its child's would be re-propagated by the next
// crosslink, a change no requirement here is about.
func doctored(
	id, req string, more []rsArt, edit string, on ...string,
) func(want []string, ignoring ...string) rsCase {
	return func(want []string, ignoring ...string) rsCase {
		c := foo(id, req, with(chain(), more...), want, ignoring...)
		c.rebuild = false
		uuids := "'{" + strings.Join(on, "}', '{") + "}'"
		c.doctor = &rsDoctor{at: len(chain()) + 1, sql: edit +
			" WHERE tagid = (SELECT tagid FROM tag WHERE tagname = 'foo')" +
			" AND rid IN (SELECT rid FROM blob WHERE uuid IN (" + uuids + "))"}
		return c
	}
}

func blockingCases() []rsCase {
	return []rsCase{
		foo("repostate.5.5.direct-override-barrier", "RS-5.5-1",
			with(chain(), control("t1", 8, "*foo {c3} A"), control("t2", 9, "*foo {c1} B")),
			[]string{"foo c1 2 t2 c1 'B' @9", "foo c2 2 0 c1 'B' @9",
				"foo c3 2 t1 c3 'A' @8", "foo c4 2 0 c3 'A' @8"},
			"-tagxref: foo c3 2 t1 c3 'A' @8", "+tagxref: foo c3 2 0 c1 'B' @9"),
		foo("repostate.5.5.cancel-blocks", "RS-5.5-2",
			with(chain(), control("t1", 8, "-foo {c3}"), control("t2", 9, "*foo {c1} v")),
			[]string{"foo c1 2 t2 c1 'v' @9", "foo c2 2 0 c1 'v' @9", "foo c3 0 t1 c3 NULL @8"},
			"+tagxref: foo c4 2 0 c1 'v' @9"),
		foo("repostate.5.5.singleton-barrier", "RS-5.5-3",
			with(chain(), control("t1", 8, "+foo {c3} s"), control("t2", 9, "*foo {c1} v")),
			[]string{"foo c1 2 t2 c1 'v' @9", "foo c2 2 0 c1 'v' @9", "foo c3 1 t1 c3 's' @8"},
			"+tagxref: foo c4 2 0 c1 'v' @9"),
	}
}
