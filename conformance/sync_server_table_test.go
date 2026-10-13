package conformance

// The server cases, by section of the draft. Each request differs from a
// request another case shows to be answered normally in one respect, and
// each refusal names its reason: the error card's text, as the draft fixes
// it or, where the draft fixes none, as each implementation words it.

const (
	pullCard = "pull {SC} {PC}"
	pushCard = "push {SC} {PC}"
)

var (
	// closed takes every capability from nobody.
	closed = []string{"UPDATE user SET cap='' WHERE login='nobody'"}
	// tombstone adds a deleted unversioned file, t.txt. go-libfossil and
	// fossil(1) both cache the catalog hash in config; it is dropped so
	// the row counts.
	tombstone = []string{
		"INSERT INTO unversioned(name, rcvid, mtime, hash, sz, encoding, content)" +
			" VALUES('t.txt', 1, {UVT}, NULL, 0, 0, NULL)",
		"DELETE FROM config WHERE name='uv-hash'",
	}
)

// uvRow counts the unversioned rows matching a condition.
func uvRow(cond string, n int) replyCheck {
	return repoCount("SELECT count(*) FROM unversioned WHERE "+cond, n)
}

// held counts the stored, non-phantom copies of an artifact.
func held(name string, n int) replyCheck {
	return repoCount("SELECT count(*) FROM blob WHERE uuid='{"+name+"}' AND size>=0", n)
}

// wrongHashAborts is why the wrong-hash file cases are not sent to fossil(1):
// xfer_accept_file records the error but still stores the content under
// HASH, and verify_before_commit then aborts the process at commit.
const wrongHashAborts = "fossil(1) 2.28 stores the content after the error " +
	"and aborts at commit when verify_before_commit sees the mismatch"

// serverRoundCases are Section 3: round structure, phantoms, cookies and
// the token window.
func serverRoundCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.3.2.reply-ends-timestamp", Req: "SP-3.2-7"},
		body: []string{"{CV}", pullCard, "{R}"},
		want: completed,
	}, {
		Case: Case{ID: "sync.3.3.server-igot-push", Req: "SP-3.3-1"},
		body: []string{"{CV}", pushCard, "igot {U}", "{R}"},
		want: all(completed, withCard("gimme {U}")),
	}, {
		// The server creates a phantom only for content a push can deliver.
		Case: Case{ID: "sync.3.3.server-igot-pull-only", Req: "SP-3.3-1"},
		body: []string{"{CV}", pullCard, "igot {U}", "{R}"},
		want: all(completed, noCard("gimme {U}")),
	}, {
		Case: Case{ID: "sync.3.3.gimme-pull-only-server", Req: "SP-3.3-4"},
		body: []string{"{CV}", pushCard, "gimme {A1}", "{R}"},
		want: all(completed, noErrors, noCard("file {A1}")),
	}, {
		Case: Case{ID: "sync.3.3.gimme-absent-server", Req: "SP-3.3-5"},
		body: []string{"{CV}", pullCard, "gimme {U}", "{R}"},
		want: all(completed, noErrors, noCard("file {U}")),
	}, {
		Case: Case{ID: "sync.3.6.server-foreign-cookies", Req: "SP-3.6-2"},
		body: []string{"{CV}", pullCard, "cookie x", "cookie y", "{R}"},
		want: all(completed, noErrors, withCard("igot {A1}")),
	}, {
		// uvfile has six tokens; a seventh is past the window every
		// receiver sees, so the card is an ordinary uvfile.
		Case: Case{ID: "sync.3.9.seventh-token-server", Req: "SP-3.9-2"},
		body: []string{"{LOGIN uvw secret}", "{CV}", pushCard,
			"uvfile n.txt {UVT} {N1} {N1.size} 0 extra", "{N1.data}", "{R}"},
		want: all(completed, noErrors, uvRow("name='n.txt' AND sz={N1.size}", 1)),
	}}
}

// serverTransportCases are Sections 4 and 4.1, and the framing of 5.1.
func serverTransportCases() []syncServerCase {
	return []syncServerCase{{
		Case:  Case{ID: "sync.4.debug-type-dispatches", Req: "SP-4-3"},
		ctype: typeDebug,
		body:  []string{"{CV}", pullCard, "{R}"},
		want:  all(completed, withCard("igot {A1}")),
	}, {
		Case: Case{ID: "sync.4.any-path-dispatches", Req: "SP-4-3"},
		path: "/anything/xfer",
		body: []string{"{CV}", pullCard, "{R}"},
		want: all(completed, withCard("igot {A1}")),
	}, {
		Case: Case{ID: "sync.4.1.container-format-reply", Req: "SP-4.1-1"},
		body: []string{"{CV}", pushCard, "igot {U}", "{R}"},
		want: all(completed, container),
	}, {
		Case: Case{ID: "sync.4.1.clone-uncompressed", Req: "SP-4.1-5"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, mediaType(typeUncompressed)),
	}, {
		Case: Case{ID: "sync.5.1.cfile-padding", Req: "SP-5.1-2"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, withCard("clone_seqno 0"), eachCard(cfilePadding)),
	}, {
		Case: Case{ID: "sync.5.2.canonical-reply-pull", Req: "SP-5.2-1"},
		body: []string{"{CV}", pullCard, "{R}"},
		want: all(completed, withCard("igot {A1}"), eachCard(canonical)),
	}, {
		Case: Case{ID: "sync.5.2.canonical-reply-clone", Req: "SP-5.2-1"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, withCard("clone_seqno 0"), eachCard(canonical)),
	}, {
		// uvigot, with five tokens, is the longest card a pull reply holds.
		Case: Case{ID: "sync.3.9.sender-six-tokens-server", Req: "SP-3.9-1"},
		body: []string{"{CV}", pushCard, pullCard, "igot {U}", "pragma uv-hash {Z1}", "{R}"},
		want: all(completed, withCard("uvigot u.txt {UVT} {UVH} 11"), eachCard(sixTokens)),
	}}
}

// serverCardCases are Section 5.2's request-card rules. The bad-command
// text follows Section 5.2: LINE with its LF, fossilized as one token.
func serverCardCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.5.2.bad-command", Req: "SP-5.2-4"},
		body: []string{"{CV}", pullCard, "bogus x", "gimme {A1}", "{R}"},
		want: all(completed, onlyError("bad command: bogus x\n"), withCard("file {A1} {A1.size}")),
	}, {
		Case: Case{ID: "sync.5.2.whitespace-runs-server", Req: "SP-5.2-2"},
		body: []string{"{CV}", "pull\t{SC}\v\v{PC}", "gimme\f {A1}\r", "{R}"},
		want: all(completed, noErrors, withCard("file {A1} {A1.size}")),
	}, {
		// A no-break space is not a separator: the gimme's one argument is
		// not a hash, so A1 is not sent.
		Case: Case{ID: "sync.5.2.non-ascii-space-server", Req: "SP-5.2-2"},
		body: []string{"{CV}", pullCard, "gimme {A1}\u00a0", "{R}"},
		want: all(completed, noCard("file {A1}")),
	}, {
		// The file the gimme put in the reply is discarded with it.
		Case: Case{ID: "sync.5.2.bad-command-resets", Req: "SP-5.2-4"},
		body: []string{"{CV}", pullCard, "gimme {A1}", "bogus", "{R}"},
		want: all(completed, onlyError("bad command: bogus\n"), noCard("file {A1}")),
	}, {
		Case: Case{ID: "sync.5.2.wrong-arity-server", Req: "SP-5.2-6"},
		body: []string{"{CV}", pullCard, "gimme {A1} extra", "gimme {A2}", "{R}"},
		want: all(completed, onlyError("bad command: gimme {A1} extra\n"),
			noCard("file {A1}"), withCard("file {A2} {A2.size}")),
	}, {
		Case: Case{ID: "sync.5.2.client-error-card", Req: "SP-5.2-6"},
		body: []string{"{CV}", pullCard, "error foo", "gimme {A1}", "{R}"},
		want: all(completed, onlyError("bad command: error foo\n"),
			withCard("file {A1} {A1.size}")),
	}, {
		Case:     Case{ID: "sync.5.2.client-clone-seqno", Req: "SP-5.2-6"},
		body:     []string{"{CV}", "clone_seqno 5", "{R}"},
		want:     all(completed, onlyError("bad command: clone_seqno 5\n")),
		deviates: all(completed, noErrors),
	}, {
		// A negative SIZE fails the file handler's validation: the reply
		// is reset, so the gimme's file goes, and parsing stops, so the
		// igot asks for nothing. fossil(1) URL-encodes the handler's
		// text where the draft has it fossilized; the check takes either.
		Case: Case{ID: "sync.5.2.file-handler-failure", Req: "SP-5.2-7"},
		body: []string{"{CV}", pushCard, pullCard, "gimme {A1}", "file {N1} -1",
			"igot {U}", "{R}"},
		want: all(completed, onlyError("malformed+file+line", "malformed file line"),
			noCard("file {A1}"), noCard("gimme {U}")),
	}, {
		// A well-framed file whose content is not HASH fails the handler
		// the same way: reset, error, and no later card is parsed.
		Case:    Case{ID: "sync.5.2.file-wrong-hash", Req: "SP-5.2-7"},
		notSent: wrongHashAborts,
		body: []string{"{CV}", pushCard, pullCard, "gimme {A1}", "file {U} {N1.size}",
			"{N1.data}", "igot {U2}", "{R}"},
		want: all(completed, onlyError("wrong+hash+on+received+artifact:+{U}",
			"wrong hash on received artifact: {U}"),
			noCard("file {A1}"), noCard("gimme {U2}"), held("U", 0)),
	}, {
		// §7.5: pre-storage verification applies to a reconstructable
		// non-clone file, so a delta against a held basis is expanded and
		// checked against HASH before it is stored.
		Case:    Case{ID: "sync.5.2.file-delta-wrong-hash", Req: "SP-5.2-7"},
		notSent: wrongHashAborts,
		body: []string{"{CV}", pushCard, pullCard, "gimme {A1}", "file {U} {A1} {DTA1.size}",
			"{DTA1.data}", "igot {U2}", "{R}"},
		want: all(completed, onlyError("wrong+hash+on+received+artifact:+{U}",
			"wrong hash on received artifact: {U}"),
			noCard("file {A1}"), noCard("gimme {U2}")),
	}, {
		// A HASH that is no artifact name fails the handler's validation.
		Case: Case{ID: "sync.5.2.file-bad-name", Req: "SP-5.2-7"},
		body: []string{"{CV}", pushCard, pullCard, "gimme {A1}", "file {Z1}0 {N1.size}",
			"{N1.data}", "igot {U2}", "{R}"},
		want: all(completed, onlyError("malformed+file+line", "malformed file line"),
			noCard("file {A1}"), noCard("gimme {U2}")),
	}, {
		Case: Case{ID: "sync.5.2.private-surplus-server", Req: "SP-5.2-9"},
		body: []string{"{CV}", "private extra", "{R}"},
		want: all(completed, onlyError("not authorized to sync private content")),
	}, {
		Case: Case{ID: "sync.5.2.igot-surplus-server", Req: "SP-5.2-10"},
		body: []string{"{CV}", pushCard, "igot {U} 0 extra", "{R}"},
		want: all(completed, noErrors, withCard("gimme {U}")),
	}, {
		Case: Case{ID: "sync.5.3.comment-not-card-server", Req: "SP-5.3-1"},
		body: []string{"{CV}", pullCard, "#gimme {A1}", "{R}"},
		want: all(completed, noErrors, noCard("file {A1}")),
	}, {
		Case:  Case{ID: "sync.5.3.timestamp-form", Req: "SP-5.3-2"},
		setup: closed,
		body:  []string{"{CV}", pullCard, "{R}"},
		want: all(completed, recentUTC, matches("# timestamp ",
			`^# timestamp \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d errors (0|[1-9][0-9]*)$`)),
	}}
}

// serverErrorCases are Section 10.1: where errors reset the reply, where
// they append, and what they leave committed.
func serverErrorCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.10.1.status-200", Req: "SP-10.1-4"},
		body: []string{"{CV}", pullCard, "igot", "{R}"},
		want: all(syncReply, onlyError("bad command: igot\n")),
	}, {
		Case: Case{ID: "sync.10.1.bad-command-keeps-mutations", Req: "SP-10.1-5"},
		body: []string{"{CV}", pushCard, "file {N1} {N1.size}", "{N1.data}", "bogus", "{R}"},
		want: all(completed, onlyError("bad command: bogus\n"), held("N1", 1)),
	}, {
		// A clone refusal resets the reply: the file already in it goes.
		Case:  Case{ID: "sync.10.1.reset-before-error", Req: "SP-10.1-2"},
		setup: []string{"UPDATE user SET cap='o' WHERE login='nobody'"},
		body:  []string{"{CV}", pullCard, "gimme {A1}", "clone 3 1", "{R}"},
		want: all(completed, noCard("file {A1}"),
			onlyError("not authorized to clone")),
	}, {
		Case: Case{ID: "sync.10.1.non-reset-append", Req: "SP-10.1-3"},
		body: []string{"{CV}", pullCard, "gimme {A1}", "pragma send-private", "{R}"},
		want: all(completed, withCard("file {A1} {A1.size}"),
			onlyError("not authorized to sync private content")),
	}, {
		Case: Case{ID: "sync.9.5.send-private-unauthorized", Req: "SP-9.5-1"},
		body: []string{"{CV}", "pragma send-private", pullCard, "gimme {A1}", "{R}"},
		want: all(completed, withCard("file {A1} {A1.size}"), withCard("igot {A2}"),
			onlyError("not authorized to sync private content")),
	}}
}

// serverAuthCases are Section 6: who may log in, and what a login grants.
// Each login request is the same push of N1 to a repository whose nobody
// may do nothing, so the push succeeds only through the login.
func serverAuthCases() []syncServerCase {
	push := []string{pushCard, "file {N1} {N1.size}", "{N1.data}", "{R}"}
	login := func(how string, cv string) []string {
		return append([]string{"{LOGIN alice secret" + how + "}", cv}, push...)
	}
	return []syncServerCase{{
		Case:  Case{ID: "sync.6.4.named-ok", Req: "SP-6.4-3"},
		setup: closed,
		body:  login("", "{CV0}"),
		want:  all(completed, noErrors, held("N1", 1)),
	}, {
		// A client the cookie-login gate admits may still log in in the
		// body.
		Case:  Case{ID: "sync.6.5.server-body-login", Req: "SP-6.5-3"},
		setup: closed,
		body:  login("", "{CV}"),
		want:  all(completed, noErrors, held("N1", 1)),
	}, {
		Case:  Case{ID: "sync.6.4.signature-case", Req: "SP-6.4-3"},
		setup: closed,
		body:  login(" upper", "{CV}"),
		want:  all(syncReply, onlyError("login failed"), held("N1", 0)),
	}, {
		Case:  Case{ID: "sync.6.4.nonce-checked", Req: "SP-6.4-5"},
		setup: closed,
		body:  login(" stale", "{CV}"),
		want:  all(syncReply, onlyError("login failed"), held("N1", 0)),
	}, {
		// The file the gimme staged is discarded. fossil(1) still sends the
		// inventory the pull asked for, as it does after any card error.
		Case: Case{ID: "sync.6.4.failure-shape", Req: "SP-6.4-6"},
		body: []string{"{CV}", pullCard, "gimme {A1}", "{LOGIN alice wrong}", "{R}"},
		want: all(syncReply, onlyError("login failed"), noCard("file ")),
	}, {
		Case: Case{ID: "sync.6.1.duplicate-login", Req: "SP-6.1-2"},
		body: []string{"{LOGIN alice secret}", "{LOGIN alice secret}", "{CV}", pullCard, "{R}"},
		// fossil(1) 2.28 sends "multiple\slogin\cards", itself mis-escaped,
		// so only the start of the text is compared.
		want: all(syncReply, onlyError("multiple login"), noCard("igot ")),
	}}
}

// serverLoginCases are the rest of Section 6: logins that need no
// signature, the capability union, and capability checks.
func serverLoginCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.6.4.nobody-no-signature", Req: "SP-6.4-2"},
		body: []string{"{LOGIN nobody secret zero}", "{CV}", pullCard, "{R}"},
		want: all(completed, noErrors, withCard("igot {A1}")),
	}, {
		Case: Case{ID: "sync.6.4.anonymous-no-signature", Req: "SP-6.4-2"},
		body: []string{"{LOGIN anonymous secret zero}", "{CV}", pullCard, "{R}"},
		want: all(completed, noErrors, withCard("igot {A1}")),
	}, {
		// nobody may read and alice may write: both are needed.
		Case: Case{ID: "sync.6.4.login-unions", Req: "SP-6.4-7"},
		setup: []string{"UPDATE user SET cap='o' WHERE login='nobody'",
			"UPDATE user SET cap='i' WHERE login='alice'"},
		body: []string{"{LOGIN alice secret}", "{CV}", pullCard, pushCard,
			"file {N1} {N1.size}", "{N1.data}", "{R}"},
		want: all(completed, noErrors, held("N1", 1), withCard("igot {A1}")),
	}, {
		Case:  Case{ID: "sync.6.6.read-required", Req: "SP-6.6-1"},
		setup: closed,
		body:  []string{"{CV}", pullCard, "{R}"},
		want:  all(completed, onlyError("not authorized to read"), noCard("igot ")),
	}, {
		Case:  Case{ID: "sync.6.6.write-required", Req: "SP-6.6-1"},
		setup: closed,
		body:  []string{"{CV}", pushCard, "{R}"},
		want:  all(completed, onlyError("not authorized to write")),
	}, {
		// nobody may read, but not clone.
		Case:  Case{ID: "sync.6.6.clone-required", Req: "SP-6.6-1"},
		setup: readOnly,
		body:  []string{"{CV}", "clone 3 1", "{R}"},
		want:  all(completed, onlyError("not authorized to clone")),
	}, {
		// alice may read, write and clone, but not sync private content.
		Case: Case{ID: "sync.6.6.private-required", Req: "SP-6.6-1"},
		body: []string{"{LOGIN alice secret}", "{CV}", "pragma send-private", pullCard, "{R}"},
		want: all(completed, onlyError("not authorized to sync private content"),
			withCard("igot {A1}")),
	}, {
		// alice may write, but is no administrator.
		Case: Case{ID: "sync.6.6.admin-required", Req: "SP-6.6-1"},
		body: []string{"{LOGIN alice secret}", "{CV}", pushCard,
			"config /config {CFGP.size}", "{CFGP.data}", "{R}"},
		want: all(completed, onlyError("not authorized to push configuration"),
			repoCount("SELECT count(*) FROM config WHERE name='project-name'"+
				" AND value='pushed'", 0)),
	}}
}
