package conformance

// Cases for what draft-fossil-sync-protocol-01 adds on refusals and their
// bookkeeping: a payload cut short (Section 5.1), the error count of the
// closing comment (5.3), the end of a request after a failed login (6.4), a
// refused push (6.6) and a gimme for private content outside a private sync
// (8.6). The reference behavior is fossil(1) 2.28 and the -01 errata.

// readOnly gives nobody Read alone, so a pull is accepted and a push is not.
var readOnly = []string{"UPDATE user SET cap='o' WHERE login='nobody'"}

// privateA1 marks A1 private on the server.
var privateA1 = []string{"INSERT INTO private(rid) SELECT rid FROM blob WHERE uuid='{A1}'"}

// pullOnlyMessage is the push refusal that follows an accepted pull.
const pullOnlyMessage = `message pull\sonly\s-\snot\sauthorized\sto\spush`

// errorsCount checks the closing comment's error count.
func errorsCount(n string) replyCheck {
	return matches("# timestamp ", ` errors `+n+`$`)
}

func serverRefusalCases() []syncServerCase {
	return append(serverErrorCountCases(), []syncServerCase{{
		// The body ends inside the payload: the card is malformed, and
		// nothing is stored, though the bytes present are N1 in full.
		Case: Case{ID: "sync.5.1.truncated-payload-server", Req: "SP-5.1-3"},
		body: []string{"{CV}", pushCard, "file {N1} 100000", "{N1.data}"},
		want: all(completed, onlyError("malformed+file+line", "malformed file line"),
			held("N1", 0)),
		fossil: "fossil(1) 2.28 stores the bytes present, which hash to N1 (erratum " +
			"sync-protocol-01-s5.1-truncated-payload-receiver)",
	}, {
		// The login failure ends card parsing, but the pull before it
		// still gets its igot inventory and the reply its closing comment.
		Case: Case{ID: "sync.6.4.failure-end-of-request", Req: "SP-6.4-8"},
		body: []string{"{CV}", pullCard, "{LOGIN alice wrong}", "{R}"},
		want: all(completed, onlyError("login failed"), withCard("igot {A1}")),
	}, {
		// A1 is private and the request is not a private sync: a peer dated
		// 2020-04-13 or later is told the artifact exists and is private.
		Case:  Case{ID: "sync.8.6.private-gimme-igot", Req: "SP-8.6-2"},
		setup: privateA1,
		body:  []string{"{CV}", pullCard, "gimme {A1}", "{R}"},
		want:  all(completed, noErrors, withCard("igot {A1} 1"), noCard("file {A1}")),
	}, {
		Case:  Case{ID: "sync.8.6.private-gimme-old-peer", Req: "SP-8.6-2"},
		setup: privateA1,
		body: []string{"pragma client-version 21000 20200101 000000", pullCard,
			"gimme {A1}", "{R}"},
		want: all(completed, noErrors, noCard("igot {A1} 1"), noCard("file {A1}")),
	}, {
		Case:  Case{ID: "sync.6.6.pull-only-message", Req: "SP-6.6-2"},
		setup: readOnly,
		body:  []string{"{CV}", pullCard, pushCard, "{R}"},
		want:  all(completed, noErrors, withCard(pullOnlyMessage), withCard("igot {A1}")),
	}, {
		// Without a pull before it, the refusal resets and is an error,
		// and the pull and gimme after it are still processed.
		Case:  Case{ID: "sync.6.6.push-refused-continues", Req: "SP-6.6-2"},
		setup: readOnly,
		body:  []string{"{CV}", pushCard, pullCard, "gimme {A1}", "{R}"},
		want: all(completed, onlyError("not authorized to write", "push denied"),
			withCard("file {A1} {A1.size}")),
	}, {
		// The file card after the refused push ends card processing: the
		// gimme after it is not answered, but the pull's inventory is sent.
		Case:  Case{ID: "sync.6.6.file-after-refused-push", Req: "SP-6.6-3"},
		setup: readOnly,
		body: []string{"{CV}", pullCard, pushCard, "file {N1} {N1.size}", "{N1.data}",
			"gimme {A1}", "{R}"},
		want: all(completed, onlyError("not authorized to write", "push denied"),
			held("N1", 0), noCard("file {A1}"), withCard("igot {A1}")),
	}}...)
}

// serverErrorCountCases are Section 5.3's N: errors raised, not error cards
// present, and three refusals that are not counted.
func serverErrorCountCases() []syncServerCase {
	return []syncServerCase{{
		// The refused file card resets the push refusal's card away, but
		// both errors are counted.
		Case:  Case{ID: "sync.5.3.errors-counts-reset", Req: "SP-5.3-4"},
		setup: closed,
		body:  []string{"{CV}", pushCard, "file {N1} {N1.size}", "{N1.data}", "{R}"},
		want: all(completed, onlyError("not authorized to write", "push denied"),
			errorsCount("2")),
	}, {
		Case: Case{ID: "sync.5.3.errors-skips-bad-command", Req: "SP-5.3-4"},
		body: []string{"{CV}", pullCard, "frobnicate", "{R}"},
		want: all(completed, onlyError("bad command"), errorsCount("0")),
	}, {
		Case: Case{ID: "sync.5.3.errors-skips-private-refusal", Req: "SP-5.3-4"},
		body: []string{"{CV}", pullCard, "pragma send-private", "{R}"},
		want: all(completed, onlyError("not authorized to sync private content"),
			errorsCount("0")),
	}}
}

// clientRefusalCases are the client's side of Sections 5.1 and 5.3.
func clientRefusalCases() []syncClientCase {
	return []syncClientCase{{
		// The reply ends inside N3's payload: N1 before it is kept, the
		// short N3 is not stored.
		Case: Case{ID: "sync.5.1.truncated-payload", Req: "SP-5.1-4"},
		op:   opPull,
		replies: [][]string{{"{SV}", "file {N1} {N1.size}", "{N1.data}",
			"file {N3} 100000", "{N3.data}"}},
		want: every(stored("N1"), notStored("N3")),
		fc:   true,
		fossil: "fossil(1) 2.28 stores the bytes present, which hash to N3 (erratum " +
			"sync-protocol-01-s5.1-truncated-payload-receiver)",
	}, {
		Case: Case{ID: "sync.5.3.errors-not-acted-on", Req: "SP-5.3-5"},
		op:   opPull,
		replies: [][]string{{"{SV}", "file {N1} {N1.size}", "{N1.data}",
			"# timestamp 2026-01-01T00:00:00 errors 7"}},
		want: every(noFailure, stored("N1")),
		fc:   true,
	}}
}
