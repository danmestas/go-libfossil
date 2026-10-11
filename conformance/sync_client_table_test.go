package conformance

// The client cases, by section of the draft. A reply script differs from
// one another case shows to be handled normally in one respect.

var (
	// quiet clears the client's unsent table. fossil(1)'s client pushes
	// unsent artifacts unasked and go-libfossil's announces them; with
	// none, either pushes only what a gimme asks for.
	quiet = []string{"DELETE FROM unsent"}
)

// phantomOf makes the client hold a phantom for an artifact named by
// placeholder, with more SQL after it.
func phantomOf(name string, more ...string) []string {
	return append([]string{
		"INSERT INTO blob(uuid, size, content) VALUES('{" + name + "}', -1, NULL)",
		"INSERT INTO phantom(rid) SELECT rid FROM blob WHERE uuid='{" + name + "}'",
	}, more...)
}

func reply(cards ...string) []string { return append([]string{"{SV}"}, append(cards, "{TS}")...) }

// clientRoundCases are Section 3.2: what every request carries, and where.
func clientRoundCases() []syncClientCase {
	return []syncClientCase{{
		Case:    Case{ID: "sync.3.2.client-version-every-round", Req: "SP-3.2-1"},
		op:      opPush,
		setup:   quiet,
		replies: [][]string{reply("gimme {A1}")},
		want: every(rounds(2), eachRequest(func(i int, req peerRequest) error {
			if index(req.cards, "pragma client-version ") < 0 {
				return expectf("request %d has no client-version", i)
			}
			return nil
		})),
		fc: true,
	}, {
		Case:    Case{ID: "sync.3.2.client-version-first", Req: "SP-3.2-2"},
		op:      opPull,
		replies: [][]string{reply("igot {U}")},
		want:    sentBefore(2, "pragma client-version ", "gimme {U}"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.2.randomness-last", Req: "SP-3.2-3"},
		op:      opPush,
		setup:   quiet,
		replies: [][]string{reply()},
		want:    every(rounds(1), randomnessLast),
		fc:      true,
	}, {
		// The randomness is under the login's signature, so it still
		// comes last.
		Case:    Case{ID: "sync.3.2.randomness-last-login", Req: "SP-3.2-3"},
		op:      clientOp{pull: true, login: true},
		replies: [][]string{reply("igot {U}")},
		want:    every(atLeast(2), randomnessLast),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.2.randomness-last-clone", Req: "SP-3.2-3"},
		op:      opClone,
		replies: [][]string{reply("clone_seqno 0", pushCard)},
		want:    every(rounds(2), randomnessLast),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.2.send-private-first", Req: "SP-3.2-4"},
		op:      clientOp{pull: true, private: true},
		replies: [][]string{reply("igot {U}")},
		want: every(sentBefore(1, "pragma send-private", "pragma client-version "),
			sentBefore(2, "pragma client-version ", "pragma send-private")),
		fc: true,
	}, {
		Case:    Case{ID: "sync.3.2.operation-every-round", Req: "SP-3.2-5"},
		op:      opSync,
		setup:   quiet,
		replies: [][]string{reply("igot {U}", "gimme {A1}"), reply("igot {U2}")},
		want: every(atLeast(3), eachRequest(func(i int, req peerRequest) error {
			if index(req.cards, "push ") < 0 || index(req.cards, "pull ") < 0 {
				return expectf("request %d lacks push or pull: %q", i, summary(req.cards))
			}
			return nil
		})),
		fc: true,
	}, {
		Case:    Case{ID: "sync.3.2.cookie-after-operation", Req: "SP-3.2-6"},
		op:      opSync,
		setup:   quiet,
		replies: [][]string{reply("cookie abc", "igot {U}")},
		want:    cookieAfterOperation(2, "abc"),
		fc:      true,
	}}
}

// clientPhantomCases are Section 3.3's phantoms.
func clientPhantomCases() []syncClientCase {
	return []syncClientCase{{
		Case:    Case{ID: "sync.3.3.igot-gimme", Req: "SP-3.3-1"},
		op:      opPull,
		replies: [][]string{reply("igot {U}")},
		want:    sent(2, "gimme {U}"),
		fc:      true,
	}, {
		// The phantom set is persistent (Table 1): one the peer could not
		// supply is still there for the next session.
		Case:    Case{ID: "sync.3.3.igot-phantom-persists", Req: "SP-3.3-1"},
		op:      opSync,
		setup:   quiet,
		replies: [][]string{reply("igot {U}")},
		want:    phantom("U"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.3.igot-held", Req: "SP-3.3-1"},
		op:      opPull,
		replies: [][]string{reply("igot {M2}")},
		want:    every(rounds(1), neverSent("gimme ")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.3.igot-push-only-client", Req: "SP-3.3-1"},
		op:      opPush,
		setup:   quiet,
		replies: [][]string{reply("igot {U}")},
		want:    every(rounds(1), notStored("U")),
		fc:      true,
	}, {
		// The delta is kept, its basis becomes the phantom asked for,
		// and the target is never asked for again.
		Case: Case{ID: "sync.3.3.delta-late-basis", Req: "SP-3.3-2"},
		op:   opPull,
		replies: [][]string{reply("file {T} {S} {DTS.size}", "{DTS.data}"),
			reply("file {S} {S.size}", "{S.data}")},
		want: every(sent(2, "gimme {S}"), neverSent("gimme {T}"), expands("T")),
		fc:   true,
	}}
}

// clientGimmeCases are the rest of Section 3.3: which phantoms the client
// asks for, and what it does with a gimme.
func clientGimmeCases() []syncClientCase {
	return []syncClientCase{{
		Case: Case{ID: "sync.3.3.gimme-skips-shunned", Req: "SP-3.3-3"},
		op:   opPull,
		setup: phantomOf("U",
			"INSERT INTO shun(uuid, mtime) VALUES('{U}', 0)"),
		replies: [][]string{reply()},
		want:    every(rounds(1), neverSent("gimme {U}")),
		fc:      true,
	}, {
		Case: Case{ID: "sync.3.3.gimme-skips-private", Req: "SP-3.3-3"},
		op:   opPull,
		setup: phantomOf("U",
			"INSERT INTO private(rid) SELECT rid FROM blob WHERE uuid='{U}'"),
		replies: [][]string{reply()},
		want:    every(rounds(1), neverSent("gimme {U}")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.3.gimme-pull-only-client", Req: "SP-3.3-4"},
		op:      opPull,
		replies: [][]string{reply("gimme {A1}")},
		want:    every(noFailure, rounds(1), neverSent("file ")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.3.gimme-absent-client", Req: "SP-3.3-5"},
		op:      opPush,
		setup:   quiet,
		replies: [][]string{reply("gimme {U}")},
		want:    every(noFailure, rounds(1), neverSent("file ")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.3.gimme-answered", Req: "SP-3.3-6"},
		op:      opPush,
		setup:   quiet,
		replies: [][]string{reply("gimme {A1}")},
		want:    sentPayload(2, "file {A1} {A1.size}", "{A1.data}"),
		fc:      true,
	}}
}

// clientTerminationCases are Sections 3.6 and 3.7: the cookie, and when
// the client sends another request.
func clientTerminationCases() []syncClientCase {
	return []syncClientCase{{
		Case:    Case{ID: "sync.3.6.cookie-replaced", Req: "SP-3.6-1"},
		op:      opPull,
		replies: [][]string{reply("cookie a", "igot {U}"), reply("cookie b", "igot {U2}")},
		want:    every(sent(2, "cookie a"), sent(3, "cookie b")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.7.empty-pull", Req: "SP-3.7-7"},
		op:      opPull,
		replies: [][]string{reply()},
		want:    every(noFailure, rounds(1)),
		fc:      true,
	}, {
		// Stored content continues the session while a phantom remains,
		// here one the client already held: limb (a).
		Case:    Case{ID: "sync.3.7.content-continues", Req: "SP-3.7-1"},
		op:      opPull,
		setup:   phantomOf("U2"),
		replies: [][]string{reply("file {N1} {N1.size}", "{N1.data}")},
		want:    every(stored("N1"), rounds(2)),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.7.phantom-no-progress", Req: "SP-3.7-6"},
		op:      opPull,
		replies: [][]string{reply("igot {U}")},
		want:    every(noFailure, rounds(2)),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.3.7.seeded-phantom-once", Req: "SP-3.7-6"},
		op:      opPull,
		setup:   phantomOf("U"),
		replies: [][]string{reply()},
		want:    every(noFailure, rounds(1), sent(1, "gimme {U}")),
		fc:      true,
	}, {
		// Content appended for a gimme continues the session, once.
		Case:    Case{ID: "sync.3.7.gimme-content-continues", Req: "SP-3.7-2"},
		op:      opSync,
		setup:   quiet,
		replies: [][]string{reply("gimme {A1}")},
		want:    every(rounds(2), sent(2, "file {A1} {A1.size}")),
		fc:      true,
	}, {
		// Content stored with no phantom left is not a reason to go on.
		Case:    Case{ID: "sync.3.7.content-alone-stops", Req: "SP-3.7-7"},
		op:      opPull,
		replies: [][]string{reply("file {N3} {N3.size}", "{N3.data}")},
		want:    every(stored("N3"), rounds(1)),
		fc:      true,
	}, {
		Case: Case{ID: "sync.3.7.uvgimme-round", Req: "SP-3.7-4"},
		op:   clientOp{pull: true, uv: true},
		replies: [][]string{reply("pragma uv-pull-only", "uvigot n.txt {UVT} {N1} {N1.size}"),
			reply("uvfile n.txt {UVT} {N1} {N1.size} 0", "{N1.data}")},
		want: every(sent(2, "uvgimme n.txt"), rounds(3)),
	}, {
		Case:    Case{ID: "sync.3.7.clone-first-round", Req: "SP-3.7-5"},
		op:      opClone,
		replies: [][]string{reply("clone_seqno 0", pushCard)},
		want:    every(rounds(2), notSent(2, "clone ")),
		fc:      true,
	}}
}
