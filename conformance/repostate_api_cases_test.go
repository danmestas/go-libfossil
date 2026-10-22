package conformance

import (
	"fmt"
	"strings"
)

func rsAPICases() []rsAPICase {
	var all []rsAPICase
	for _, table := range [][]rsAPICase{
		resolveCases(), resolveFormCases(), leafAPICases(), commitLeafCases(),
		branchAPICases(), writerAPICases(), integrityAPICases(), defaultBranchCases(),
		checkoutAPICases(), workingAPICases(), trunkAPICases(), verifyLeafCases(),
		mergeBaseAPICases(),
	} {
		all = append(all, table...)
	}
	for i := range all {
		all[i].Gap = repostateGaps[all[i].ID]
		all[i].Deviation = repostateDeviations[all[i].ID]
	}
	return all
}

// forked is c1 with two trunk children, a and b, b the newer; b closes
// itself when closed is set.
func forked(closed bool) []rsArt {
	var tags []string
	if closed {
		tags = []string{"+closed *"}
	}
	return []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("a", 2, of("c1")),
		checkin("b", 3, of("c1"), tags...),
	}
}

// c2Prefix is the first n hex digits of chain's c2, which depends only on
// c1 and c2, so a script may use it before it is built.
func c2Prefix(n int) string { return rsBuild(chain()).hashes["c2"][:n] }

func resolveCases() []rsAPICase {
	return []rsAPICase{
		// Tagged later, c1 carries the newer tag row; c3 has the newer
		// check-in time, which is what the rule compares.
		resolves("repostate.8.2.event-mtime", "RS-8.2-1",
			with(chain(), control("t1", 8, "+sym-rel {c3}"), control("t2", 9, "+sym-rel {c1}")),
			"rel", "c3"),
		// c3 and c2 inherit sym-trunk from c1, with c1's tag time; only
		// their own times tell them apart.
		resolves("repostate.8.2.trunk-chain", "RS-8.2-1", []rsArt{
			checkin("c1", 1, nil, rsRoot...),
			checkin("c3", 9, of("c1")), checkin("c2", 2, of("c1")),
		}, "trunk", "c3"),
		resolves("repostate.8.2.closed-newer", "RS-8.2-2", forked(true), "trunk", "b"),
		// c2, the newest trunk check-in, has a child, so is no leaf.
		resolves("repostate.8.2.not-leaf", "RS-8.2-3", []rsArt{
			checkin("c1", 1, nil, rsRoot...),
			checkin("c2", 9, of("c1")), checkin("c3", 3, of("c2")),
		}, "trunk", "c2"),
		resolves("repostate.8.2.unknown", "RS-8.2-4", chain(), "nosuch", ""),
		resolves("repostate.8.2.no-trunk-sym", "RS-8.2-4",
			[]rsArt{checkin("c1", 1, nil), checkin("c2", 2, of("c1"))}, "trunk", ""),
		resolves("repostate.9.fork-symbol", "RS-9-2", forked(false), "trunk", "b"),
		{
			Case: Case{ID: "repostate.8.5.resolution-not-leaves", Req: "RS-8.5-1"},
			arts: forked(true),
			run: func(e *rsEnv) error {
				if err := resolves("", "", nil, "trunk", "b").run(e); err != nil {
					return err
				}
				return openLeaves("", "", nil, "trunk", "a").run(e)
			},
		},
	}
}

func resolveFormCases() []rsAPICase {
	release := with(chain(), control("t1", 8, "+sym-release {c3}"))
	// A tag named like c2's hash prefix, on c1.
	hexTag := with(chain(), control("t1", 8, "+sym-"+c2Prefix(6)+" {c1}"))
	mergedIn := []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("b1", 2, of("c1"), rsFeat...),
		checkin("c2", 3, of("c1")),
		checkin("b2", 4, of("b1", "c2")),
		checkin("b3", 5, of("b2")),
	}
	return []rsAPICase{
		resolves("repostate.8.3.rid", "RS-8.3-1", chain(), "rid:{rid:c2}", "c2"),
		// Without the prefix the name would be read as c2's hash.
		resolves("repostate.8.3.tag-prefix", "RS-8.3-2", hexTag, "tag:"+c2Prefix(6), "c1"),
		// tip is the newest check-in on any branch, here b3 on feat.
		resolves("repostate.8.3.tip", "RS-8.3-3",
			with(branched(), checkin("b3", 6, of("b2"))), "tip", "b3"),
		resolves("repostate.8.3.root", "RS-8.3-5", branched(), "root:feat", "c1"),
		resolves("repostate.8.3.start", "RS-8.3-6",
			with(branched(), checkin("b3", 6, of("b2"))), "start:feat", "b1"),
		resolves("repostate.8.3.merge-in", "RS-8.3-7", mergedIn, "merge-in:feat", "c2"),
		resolves("repostate.8.3.name-timestamp", "RS-8.3-8", chain(),
			"trunk:2026-01-01T00:00:03", "c3"),
		resolves("repostate.8.3.hash-before-name", "RS-8.3-9", hexTag, c2Prefix(6), "c2"),
		resolves("repostate.8.3.ambiguous-prefix", "RS-8.3-10", ambiguous(), ambiguousPrefix(), ""),
		// Section 8.3 retries a name of more than four characters without
		// one trailing punctuation character, and one of more than five
		// without two; "rel." is too short and "release..." ends in three.
		resolves("repostate.8.3.trailing-punct", "RS-8.3-11", release, "release.", "c3"),
		resolves("repostate.8.3.trailing-punct-two", "RS-8.3-11", release, "release!?", "c3"),
		resolves("repostate.8.3.trailing-punct-three", "RS-8.3-11", release, "release...", ""),
		resolves("repostate.8.3.trailing-punct-short", "RS-8.3-11",
			with(chain(), control("t1", 8, "+sym-rel {c3}")), "rel.", ""),
		resolves("repostate.8.3.trailing-punct-not-listed", "RS-8.3-11", release, "release;", ""),
	}
}

// ambiguous is chain[:1] and a second root whose hash shares its first four
// hex digits with c1's: the comment is varied until it does.
func ambiguous() []rsArt {
	c1 := chain()[0]
	prefix := rsBuild([]rsArt{c1}).hashes["c1"][:4]
	for n := 0; n < 1<<22; n++ {
		twin := checkin("twin", 2, nil, rsRoot...)
		twin.body = strings.Replace(twin.body, "C twin", fmt.Sprintf("C twin%d", n), 1)
		if rsBuild([]rsArt{twin}).hashes["twin"][:4] == prefix {
			return []rsArt{c1, twin}
		}
	}
	panic("conformance: no artifact shares c1's hash prefix")
}

func ambiguousPrefix() string { return rsBuild(chain()[:1]).hashes["c1"][:4] }

func leafAPICases() []rsAPICase {
	forkedChain := with(chain(), checkin("f", 5, of("c3")))
	closedC4 := with(forkedChain, control("t", 9, "+closed {c4}"))
	return []rsAPICase{
		openLeaves("repostate.7.4.closed-leaf", "RS-7.4-1", closedC4, "trunk", "f"),
		openLeaves("repostate.7.4.reopened", "RS-7.4-1",
			with(closedC4, control("t2", 10, "-closed {c4}")), "trunk", "c4", "f"),
		openLeaves("repostate.7.5.branch-open-leaves", "RS-7.5-1", merged(), "feat", "b1"),
		// BranchLeaves selects a branch's members by their active branch
		// row: b2, which inherits feat, is on feat; c2, a leaf too, is not.
		openLeaves("repostate.6.1.branch-membership", "RS-6.1-1", branched(), "feat", "b2"),
		openLeaves("repostate.9.branch-not-open", "RS-9-3",
			with(branched(), control("t", 9, "+closed {b2}")), "feat"),
		leafAmbiguous("repostate.7.5.ambiguity-non-leaf", "RS-7.5-2", forkedChain, "c3", false),
		leafAmbiguous("repostate.7.5.ambiguity-open", "RS-7.5-3", forkedChain, "f", true),
		leafAmbiguous("repostate.7.5.ambiguity-closed", "RS-7.5-3", closedC4, "f", false),
	}
}

// untaggedChild is c1, whose branch tag is a singleton, and its child c2,
// which therefore carries no branch tag.
func untaggedChild() []rsArt {
	return []rsArt{
		checkin("c1", 1, nil, "+branch * trunk", "*sym-trunk *"),
		checkin("c2", 2, of("c1")),
	}
}

// defaultBranchCases resolve start: from c2, which carries no branch tag.
// The walk keeps to c2's branch, so it reaches c1 only when c2 is attributed
// to the branch c1's tag names: the default branch of Section 6.1.
func defaultBranchCases() []rsAPICase {
	onMain := []rsArt{
		checkin("c1", 1, nil, "+branch * main", "*sym-main *"),
		checkin("c2", 2, of("c1")),
	}
	return []rsAPICase{
		resolves("repostate.6.1.untagged-default-branch", "RS-6.1-2",
			untaggedChild(), "start:trunk", "c1"),
		mainBranch("repostate.6.1.main-branch-setting", "RS-6.1-4", "main",
			resolves("", "", onMain, "start:main", "c1")),
		// With main-branch set, an untagged check-in is not on trunk.
		mainBranch("repostate.6.1.main-branch-not-trunk", "RS-6.1-4", "main",
			resolves("", "", untaggedChild(), "start:trunk", "c2")),
	}
}

// mainBranch is c with the main-branch setting set to name in both
// repositories before c runs.
func mainBranch(id, req, name string, c rsAPICase) rsAPICase {
	c.Case = Case{ID: id, Req: req}
	inner := c.run
	c.run = func(e *rsEnv) error {
		if e.ref != nil {
			if _, err := e.fossilRun("settings", "main-branch", name); err != nil {
				return err
			}
		}
		r, err := e.open()
		if err != nil {
			return err
		}
		if err := r.SetConfig("main-branch", name); err != nil {
			return err
		}
		return inner(e)
	}
	return c
}
