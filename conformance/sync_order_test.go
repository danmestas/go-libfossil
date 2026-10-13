package conformance

// Cases for the order in which a server handles request cards (Sections 5.2,
// 6.4 and 6.6), its gimme for its own phantoms at the end of a pushed
// request (3.3, 6.4), and a clone reply cut short by a card the client
// cannot read (5.2, 10.1). Each puts a card where only in-order handling
// gives the reference answer: output ahead of a refusal, a card ahead of the
// push or login card it depends on, a card after a refusal that ends the
// request. The reference behavior is fossil(1) 2.28.

// serverVersion is the pragma a client-version with DATE and TIME is
// answered with in place, so it is output a later reset discards.
const serverVersion = "pragma server-version"

// phantomU makes U a phantom on the server.
var phantomU = []string{
	"INSERT INTO blob(uuid, size, content, rcvid) VALUES('{U}', -1, NULL, 0)",
	"INSERT INTO phantom(rid) SELECT rid FROM blob WHERE uuid='{U}'",
}

// manyPhantoms makes 600 phantoms on the server, more than one reply asks
// for.
var manyPhantoms = []string{
	"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<600)" +
		" INSERT INTO blob(uuid, size, content, rcvid)" +
		" SELECT printf('%040x', i), -1, NULL, 0 FROM n",
	"INSERT INTO phantom(rid) SELECT rid FROM blob WHERE size<0",
}

func serverOrderCases() []syncServerCase {
	return append(append(serverRefusalOrderCases(), serverRefusalResetCases()...),
		serverPhantomGimmeCases()...)
}

// serverRefusalOrderCases are refusals whose reset, or whose end of card
// processing, only output around them shows.
func serverRefusalOrderCases() []syncServerCase {
	return []syncServerCase{{
		Case:  Case{ID: "sync.6.6.push-refusal-resets", Req: "SP-6.6-2"},
		setup: closed,
		body:  []string{"{CV}", pushCard, "{R}"},
		want: all(completed, onlyError("not authorized to write"),
			noCard(serverVersion)),
	}, {
		// The refused push does not end card processing: the client-version
		// after it is answered.
		Case:  Case{ID: "sync.6.6.push-refusal-continues-output", Req: "SP-6.6-2"},
		setup: closed,
		body:  []string{pushCard, "{CV}", "{R}"},
		want: all(completed, onlyError("not authorized to write"),
			count(serverVersion, 1)),
	}, {
		Case:  Case{ID: "sync.10.1.pull-refusal-resets", Req: "SP-10.1-2"},
		setup: closed,
		body:  []string{"{CV}", pullCard, "{R}"},
		want: all(completed, onlyError("not authorized to read"),
			noCard(serverVersion)),
	}, {
		// The refused pull ends card processing: the client-version after it
		// is not answered.
		Case:  Case{ID: "sync.6.6.pull-refusal-ends", Req: "SP-6.6-1"},
		setup: closed,
		body:  []string{pullCard, "{CV}", "{R}"},
		want: all(completed, onlyError("not authorized to read"),
			noCard(serverVersion)),
	}, {
		// §10.1: the clone batch, its cookie, clone_seqno and push card are
		// sent at the clone card, as page_xfer sends them, so the bad
		// command's reset discards them.
		Case: Case{ID: "sync.10.1.clone-then-bad-command", Req: "SP-10.1-2"},
		body: []string{"{CV}", "clone 3 1", "bogus", "{R}"},
		want: all(completed, onlyError("bad command: bogus\n"), noCard("cfile "),
			noCard("clone_seqno"), noCard("push "), noCard("cookie ")),
	}}
}

// serverRefusalResetCases continues serverRefusalOrderCases with the
// refusals that reset the reply; the table is split to stay short.
func serverRefusalResetCases() []syncServerCase {
	return []syncServerCase{{
		// The refused push resets too: g may clone but not write.
		Case:  Case{ID: "sync.10.1.clone-then-refused-push", Req: "SP-10.1-2"},
		setup: []string{"UPDATE user SET cap='g' WHERE login='nobody'"},
		body:  []string{"{CV}", "clone 3 1", pushCard, "{R}"},
		want: all(completed, onlyError("not authorized to write"), noCard("cfile "),
			noCard("clone_seqno"), noCard("cookie ")),
	}, {
		// The clone's unversioned catalog is sent at the clone card too.
		Case: Case{ID: "sync.10.1.clone-uv-catalog-reset", Req: "SP-10.1-2"},
		setup: []string{"INSERT INTO config(name, value, mtime) VALUES('uv-sync', '1', 0)",
			"DELETE FROM config WHERE name='uv-hash'"},
		body: []string{"{CV}", "clone 3 1", "bogus", "{R}"},
		want: all(completed, onlyError("bad command: bogus\n"), noCard("uvigot "),
			noCard("pragma uv-pull-only")),
	}, {
		// page_xfer answers req-clusters at the pragma, so a later reset
		// discards the cluster igots.
		Case: Case{ID: "sync.10.1.req-clusters-reset", Req: "SP-10.1-2"},
		body: []string{"{CV}", "pragma req-clusters", "bogus", "{R}"},
		want: all(completed, onlyError("bad command: bogus\n"),
			noCard("# sending-clusters"), noCard("igot {C}")),
	}, {
		// The file card comes before the push card that would enable it.
		Case: Case{ID: "sync.6.6.file-before-push", Req: "SP-6.6-3"},
		body: []string{"{CV}", "file {N1} {N1.size}", "{N1.data}", pushCard, "{R}"},
		want: all(completed, onlyError("not authorized to write"), held("N1", 0)),
	}, {
		// nobody may not read; alice may, but her login comes after the
		// pull, which is refused under nobody's capabilities.
		Case:  Case{ID: "sync.6.4.cards-before-login", Req: "SP-6.4-7"},
		setup: closed,
		body:  []string{"{CV}", pullCard, "{LOGIN alice secret}", "{R}"},
		want: all(completed, onlyError("not authorized to read"),
			noCard("igot ")),
	}, {
		// The login failure ends card parsing, but the push before it still
		// gets its phantom gimme.
		Case: Case{ID: "sync.6.4.failure-end-of-request-push", Req: "SP-6.4-8"},
		body: []string{"{CV}", pushCard, "igot {U}", "{LOGIN alice wrong}", "{R}"},
		want: all(completed, onlyError("login failed"), withCard("gimme {U}")),
	}}
}

// serverPhantomGimmeCases are a pushed request's gimme cards: one for each
// of the server's phantoms, whatever made it, sent after the card loop.
func serverPhantomGimmeCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.3.3.server-igot-phantom", Req: "SP-3.3-1"},
		body: []string{"{CV}", pushCard, "igot {U}", "{R}"},
		want: all(completed, withCard("gimme {U}"),
			repoCount("SELECT count(*) FROM phantom JOIN blob USING(rid)"+
				" WHERE uuid='{U}'", 1)),
	}, {
		// The file after the igot fills the phantom, so it is not asked for.
		Case: Case{ID: "sync.3.3.server-igot-then-file", Req: "SP-3.3-1"},
		body: []string{"{CV}", pushCard, "igot {N1}", "file {N1} {N1.size}", "{N1.data}",
			"{R}"},
		want: all(completed, noErrors, held("N1", 1), noCard("gimme {N1}")),
	}, {
		Case:  Case{ID: "sync.6.4.push-own-phantoms", Req: "SP-6.4-8"},
		setup: phantomU,
		body:  []string{"{CV}", pushCard, "{R}"},
		want:  all(completed, noErrors, withCard("gimme {U}")),
	}, {
		Case:  Case{ID: "sync.6.4.pull-no-phantom-gimme", Req: "SP-6.4-8"},
		setup: phantomU,
		body:  []string{"{CV}", pullCard, "{R}"},
		want:  all(completed, noErrors, noCard("gimme ")),
	}, {
		// The client asked for U, so it does not hold it: the server does
		// not ask it back.
		Case:  Case{ID: "sync.6.4.push-phantom-peer-lacks", Req: "SP-6.4-8"},
		setup: phantomU,
		body:  []string{"{CV}", pushCard, "gimme {U}", "{R}"},
		want:  all(completed, noErrors, noCard("gimme {U}")),
	}, {
		// fossil(1) asks for at most 500 phantoms in one reply. The 500 is
		// fossil's request_phantoms(&xfer, 500), not an RFC number: -01
		// Section 3.3 only says the scan is over the server's phantoms.
		Case:  Case{ID: "sync.6.4.push-phantom-bound", Req: "SP-6.4-8"},
		setup: manyPhantoms,
		body:  []string{"{CV}", pushCard, "{R}"},
		want:  all(completed, noErrors, count("gimme ", 500)),
	}}
}

// clientCloneBadCardCases are a clone reply cut short by a card the client
// cannot read: the reply-card loop ends there, and the second clone round is
// still made. The push card after it was not read, so that round is the
// cleanup. The clone then fails, and both clients delete what it stored, so
// the cards kept before the bad one are seen only in the rounds.
func clientCloneBadCardCases() []syncClientCase {
	return []syncClientCase{{
		Case: Case{ID: "sync.5.2.clone-unknown-prefix", Req: "SP-5.2-5"},
		op:   opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "bogus card",
			"cfile {N3} {N3.size} {N3.zsize}", "{N3.z}", "{N3.pad}", "clone_seqno 3", pushCard)},
		want: every(rounds(2), noOperation(2), failsWith("bogus card")),
		fc:   true,
	}, {
		Case:    Case{ID: "sync.5.2.clone-malformed-prefix", Req: "SP-5.2-8"},
		op:      opClone,
		replies: [][]string{cloneReply([]string{"N1"}, "cfile {N3} x 1")},
		want:    every(rounds(2), noOperation(2), failsWith("malformed cfile line")),
		fc:      true,
	}}
}
