package conformance

import "slices"

// cloneReply is a clone reply delivering artifacts by placeholder, each as
// a cfile padded as Section 5.1 says, then the cards after them.
func cloneReply(keys []string, after ...string) []string {
	var cards []string
	for _, k := range keys {
		cards = append(cards, "cfile {"+k+"} {"+k+".size} {"+k+".zsize}", "{"+k+".z}",
			"{"+k+".pad}")
	}
	return reply(append(cards, after...)...)
}

// noOperation checks request i carries no clone, pull or push card.
func noOperation(i int) clientCheck {
	return every(notSent(i, "clone "), notSent(i, "pull "), notSent(i, "push "))
}

// clientCloneCases are Sections 3.7(e), 8 and 8.2 on the client.
func clientCloneCases() []syncClientCase {
	return []syncClientCase{{
		Case:    Case{ID: "sync.8.first-request", Req: "SP-8-1"},
		op:      clientOp{clone: true, login: true},
		replies: [][]string{cloneReply([]string{"N1"}, "clone_seqno 0", pushCard)},
		want: func(r *clientRun) error {
			req, err := r.request(1)
			if err != nil {
				return err
			}
			got := lines(req.cards)
			if len(got) != 3 || !slices.Contains(got, "clone 3 1") ||
				index(req.cards, "pragma client-version ") != 0 ||
				!nonceComment.MatchString(got[2]) {
				return expectf("%s: first request %q, want client-version, clone 3 1, randomness",
					r.who, got)
			}
			return nil
		},
		fc: true,
	}, {
		// Neither reqconfig nor uv-hash goes in round 0. fossil(1)'s clone
		// asks for configuration in the next round; go-libfossil's Clone
		// asks for neither in any round, having no option for either.
		Case:    Case{ID: "sync.8.4.round-0-defers-config", Req: "SP-8.4-1"},
		op:      opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "clone_seqno 0", pushCard)},
		want:    every(atLeast(2), notSent(1, "reqconfig "), notSent(1, "pragma uv-hash ")),
		fc:      true,
	}, {
		// SP-7.7-4 holds in a clone too: the cfile delta DTA1 against the
		// A1 just received does not give N1, so the clone fails rather than
		// keep an artifact under a name it does not have.
		Case: Case{ID: "sync.7.7.clone-delta-wrong-hash", Req: "SP-7.7-4"},
		op:   clientOp{clone: true, login: true},
		replies: [][]string{append(cloneReply([]string{"A1"}),
			"cfile {N1} {A1} {DTA1.size} {DTA1.zsize}", "{DTA1.z}", "{DTA1.pad}",
			"clone_seqno 0", pushCard)},
		want: mentions("mismatch", "wrong hash", "does not match"),
	}, {
		// And for a delta whose basis arrives later in the same reply: DTS
		// against S, which comes after it, does not give N1. The clone fails
		// and, as a failed clone does, removes the repository it was making.
		Case: Case{ID: "sync.7.7.clone-late-basis-wrong-hash", Req: "SP-7.7-4"},
		op:   clientOp{clone: true, login: true},
		replies: [][]string{cloneReply(nil,
			"cfile {N1} {S} {DTS.size} {DTS.zsize}", "{DTS.z}", "{DTS.pad}",
			"cfile {S} {S.size} {S.zsize}", "{S.z}", "{S.pad}",
			"clone_seqno 0", pushCard)},
		want: every(notKept("N1"), names("N1"),
			mentions("mismatch", "wrong hash", "does not match")),
	}, {
		Case: Case{ID: "sync.8.2.clone-advancing-cursor", Req: "SP-8.2-2"},
		op:   opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "clone_seqno 2", pushCard),
			cloneReply([]string{"N3"}, "clone_seqno 3", pushCard),
			cloneReply([]string{"S"}, "clone_seqno 0", pushCard)},
		want: every(sent(2, "clone 3 2"), sent(3, "clone 3 3"), noOperation(4)),
		fc:   true,
	}, {
		// A NEXT that is not decimal leaves the sequence at 1, and the
		// push after it is still read.
		Case:    Case{ID: "sync.8.2.nondecimal-next", Req: "SP-8.2-2"},
		op:      opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "clone_seqno x7", pushCard)},
		want:    sent(2, "clone 3 1"),
		fc:      true,
	}, {
		Case: Case{ID: "sync.8.2.clone-missing-push", Req: "SP-8.2-4"},
		op:   opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "clone_seqno 2", pushCard),
			cloneReply([]string{"N3"}, "clone_seqno 3")},
		want: every(sent(2, "clone 3 2"), noOperation(3), rounds(3)),
		fc:   true,
	}, {
		Case:    Case{ID: "sync.8.2.clone-zero-cleanup", Req: "SP-8.2-5"},
		op:      opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "clone_seqno 0", pushCard)},
		want:    every(rounds(2), noOperation(2)),
		fc:      true,
	}, {
		// T arrives as a delta against S, which does not: the cleanup may
		// ask for S, but no pull follows it, so the clone ends with S a
		// phantom. A third reply is scripted so that a pull, were one sent,
		// would be seen rather than fail the exchange.
		Case: Case{ID: "sync.8.2.clone-zero-phantom", Req: "SP-8.2-5"},
		op:   opClone,
		replies: [][]string{reply("cfile {T} {S} {T.size} {DTS.zsize}", "{DTS.z}", "{DTS.pad}",
			"clone_seqno 0", pushCard), reply(), reply("file {S} {S.size}", "{S.data}")},
		want: every(noFailure, rounds(2), noOperation(2)),
		fc:   true,
	}}
}

// clientCloneStallCases are Section 3.7(e)'s stop for a clone that does
// not advance.
func clientCloneStallCases() []syncClientCase {
	return []syncClientCase{{
		// The second reply stores nothing and repeats the sequence: a
		// positive sequence alone does not continue.
		Case: Case{ID: "sync.8.2.clone-stalled-cursor", Req: "SP-3.7-5"},
		op:   opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "clone_seqno 2", pushCard),
			reply("clone_seqno 2", pushCard)},
		want: rounds(2),
		fc:   true,
	}}
}

// clientCloneErrorCases are Sections 8.3 and 10.1: errors in a clone.
func clientCloneErrorCases() []syncClientCase {
	return []syncClientCase{{
		// No push came, so the forced second round is the cleanup.
		Case:    Case{ID: "sync.8.3.clone-first-error-only", Req: "SP-8.3-1"},
		op:      opClone,
		replies: [][]string{reply(`error not\sauthorized\sto\sclone`)},
		want:    every(rounds(2), noOperation(2)),
		fc:      true,
	}, {
		Case: Case{ID: "sync.8.3.clone-first-error-content", Req: "SP-8.3-1"},
		op:   opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "error x",
			"cfile {N3} {N3.size} {N3.zsize}", "{N3.z}", "{N3.pad}", "clone_seqno 3", pushCard)},
		want: sent(2, "clone 3 3"),
		fc:   true,
	}, {
		// After the first round an error ends the reply: N3 and the push
		// after it are not read, but N1 was stored, so a cleanup follows.
		Case: Case{ID: "sync.8.3.later-error", Req: "SP-10.1-1"},
		op:   opClone,
		replies: [][]string{cloneReply([]string{"S"}, "clone_seqno 2", pushCard),
			cloneReply([]string{"N1"}, "error boom", "cfile {N3} {N3.size} {N3.zsize}",
				"{N3.z}", "{N3.pad}", "clone_seqno 3", pushCard)},
		want: every(rounds(3), noOperation(3)),
		fc:   true,
	}}
}
