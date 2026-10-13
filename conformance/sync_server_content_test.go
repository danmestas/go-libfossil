package conformance

// serverContentCases are Sections 7.1 to 7.5: file and cfile, delta
// sources, private content, and SHA3 names.
func serverContentCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.7.1.pull-answer-file", Req: "SP-7.1-2"},
		body: []string{"{CV}", pullCard, "gimme {A1}", "{R}"},
		want: all(completed, payloadIs("file {A1} {A1.size}", "{A1.data}"), noCard("cfile ")),
	}, {
		Case: Case{ID: "sync.7.2.clone-verbatim", Req: "SP-7.2-3"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, verbatim),
	}, {
		Case: Case{ID: "sync.7.2.clone-cfile-only", Req: "SP-7.2-4"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, present("cfile {A1} "), noCard("file ")),
	}, {
		Case: Case{ID: "sync.11.4.sha3-names-kept", Req: "SP-11.4-1"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, present("cfile {M2} "), present("cfile {B1} ")),
	}, {
		// A2 is held, so the delta is consumed and ignored: A2 stays
		// stored in full. Whether a phantom is made for the delta's basis
		// is the receiver's choice (Section 7.4); fossil(1) 2.28 makes one.
		Case: Case{ID: "sync.7.4.verified-duplicate-server", Req: "SP-7.4-1"},
		body: []string{"{CV}", pushCard, "file {A2} {U} {DA2U.size}", "{DA2U.data}", "{R}"},
		want: all(completed, noErrors, held("A2", 1),
			repoCount("SELECT count(*) FROM delta JOIN blob USING(rid) WHERE uuid='{A2}'", 0)),
	}, {
		Case: Case{ID: "sync.7.5.server-unauthorized", Req: "SP-7.5-2"},
		body: []string{"{CV}", pushCard, "private", "file {N1} {N1.size}", "{N1.data}", "{R}"},
		want: all(completed, onlyError("not authorized to sync private content"), held("N1", 1),
			repoCount("SELECT count(*) FROM private JOIN blob USING(rid) WHERE uuid='{N1}'", 0)),
	}, {
		// Q is stored as a delta against private P; a clone by nobody,
		// who may not have P, gets Q in full.
		Case: Case{ID: "sync.7.5.no-private-delta", Req: "SP-7.5-4"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, noCard("cfile {P} "), tokenCount("cfile {Q} ", 4)),
	}, {
		// priv may have private content but did not ask for it with
		// send-private. fossil(1) answers the gimme with a private igot
		// (Appendix A.3, scenario 2), which the case allows.
		Case: Case{ID: "sync.7.5.private-needs-send-private", Req: "SP-7.5-6"},
		body: []string{"{LOGIN priv secret}", "{CV}", pullCard, "gimme {P}", "{R}"},
		want: all(completed, noErrors, noCard("file {P}"), noCard("private")),
	}, {
		// fossil(1)'s send_private has no shun filter, unlike its public
		// igot paths, so a shunned private artifact is still announced
		// (#349 asked otherwise; fossil wins). No requirement row covers private
		// inventory filtering, so the case rides on SP-9.5-1, send-private.
		Case:  Case{ID: "sync.7.7.shunned-private-igot", Req: "SP-9.5-1"},
		setup: []string{"INSERT INTO shun(uuid, mtime) VALUES('{P}', 0)"},
		body:  []string{"{LOGIN priv secret}", "{CV}", "pragma send-private", pullCard, "{R}"},
		want:  all(completed, noErrors, withCard("igot {P} 1")),
	}}
}

// serverUVCases are Section 7.3's rules for an unversioned file a client
// sends. uvw may write unversioned files; nobody may not.
func serverUVCases() []syncServerCase {
	uvw := []string{"{LOGIN uvw secret}", "{CV}", pushCard}
	with := func(cards ...string) []string { return append(append([]string{}, uvw...), cards...) }
	return []syncServerCase{{
		// fossil(1) URL-encodes the handler's error text, where the
		// draft fossilizes it; the check takes either.
		Case: Case{ID: "sync.7.3.flags-negative", Req: "SP-7.3-2"},
		body: with("uvfile n.txt {UVT} - 0 -1", "igot {U}", "{R}"),
		want: all(completed, onlyError("malformed+uvfile+line", "malformed uvfile line"),
			uvRow("name='n.txt'", 0), noCard("gimme {U}")),
	}, {
		Case: Case{ID: "sync.7.3.flags-reserved", Req: "SP-7.3-2"},
		body: with("uvfile n.txt {UVT} {N1} {N1.size} 2", "{N1.data}", "{R}"),
		want: all(completed, noErrors, uvRow("name='n.txt' AND sz={N1.size}", 1)),
	}, {
		// N1 is named by SHA1 in a repository whose policy is SHA3: the
		// row keeps the name the card gave it, or its catalog hash would
		// never match the sender's.
		Case: Case{ID: "sync.7.3.received-hash", Req: "SP-7.3-10"},
		body: with("uvfile n.txt {UVT} {N1} {N1.size} 0", "{N1.data}", "{R}"),
		want: all(completed, noErrors, uvRow("name='n.txt' AND hash='{N1}'", 1)),
	}, {
		// Accepted, the card would delete u.txt, being newer.
		Case: Case{ID: "sync.7.3.dash-with-payload", Req: "SP-7.3-4"},
		body: with("uvfile u.txt {UVT1} - {N1.size} 0", "{N1.data}", "{R}"),
		want: all(completed, onlyError("HASH+does+not+match", "hash mismatch"),
			uvRow("name='u.txt' AND hash='{UVH}'", 1)),
	}, {
		Case: Case{ID: "sync.7.3.status2-mtime", Req: "SP-7.3-5"},
		body: with("uvfile u.txt {UVT1} {UVH} 11 4", "{R}"),
		want: all(completed, noErrors,
			uvRow("name='u.txt' AND mtime={UVT1} AND hash='{UVH}' AND sz=11", 1)),
	}, {
		Case: Case{ID: "sync.7.3.omitted-content", Req: "SP-7.3-6"},
		body: with("uvfile w.txt {UVT1} {UVH} 11 4", "{R}"),
		want: all(completed, noErrors,
			uvRow("name='w.txt' AND hash='{UVH}' AND sz=0 AND content IS NULL", 1)),
	}, {
		Case: Case{ID: "sync.7.3.write-permission", Req: "SP-7.3-7"},
		body: []string{"{CV}", pushCard, "uvfile n.txt {UVT} {N1} {N1.size} 0", "{N1.data}", "{R}"},
		want: all(completed, uvRow("name='n.txt'", 0),
			onlyError("Write+permissions+for+unversioned+files+missing",
				"insufficient capabilities")),
	}, {
		Case: Case{ID: "sync.7.3.no-push-needed", Req: "SP-7.3-7"},
		body: []string{"{LOGIN uvw secret}", "{CV}", "uvfile n.txt {UVT} {N1} {N1.size} 0",
			"{N1.data}", "{R}"},
		want: all(completed, noErrors, uvRow("name='n.txt' AND sz={N1.size}", 1)),
	}}
}

// serverUVGimmeCases are Section 7.3's answers to uvgimme.
func serverUVGimmeCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.7.3.uvgimme-absent", Req: "SP-7.3-8"},
		body: []string{"{CV}", pullCard, "uvgimme nosuch", "{R}"},
		want: all(completed, noErrors, noCard("uvfile ")),
	}, {
		Case: Case{ID: "sync.7.3.uvgimme-answer", Req: "SP-7.3-9"},
		body: []string{"{CV}", pullCard, "uvgimme u.txt", "{R}"},
		want: all(completed, payloadIs("uvfile u.txt {UVT} {UVH} 11 0", "hello world")),
	}, {
		Case:  Case{ID: "sync.7.3.uvgimme-tombstone", Req: "SP-7.3-9"},
		setup: tombstone,
		body:  []string{"{CV}", pullCard, "uvgimme t.txt", "{R}"},
		want:  all(completed, withCard("uvfile t.txt {UVT} - 0 1")),
	}, {
		// A private repository: nobody may do nothing at all.
		Case:   Case{ID: "sync.7.3.uvgimme-no-read", Req: "SP-7.3-11"},
		setup:  []string{noAnonymousCaps("")},
		body:   []string{"{CV}", "uvgimme u.txt", "{R}"},
		want:   all(completed, noErrors, noCard("uvfile "), noCard("uvigot ")),
		fossil: "fossil(1) 2.28 answers uvgimme without checking Read (Section 7.3)",
	}, {
		Case:  Case{ID: "sync.7.3.uvgimme-clone-implies-read", Req: "SP-7.3-12"},
		setup: []string{noAnonymousCaps("g")},
		body:  []string{"{CV}", "uvgimme u.txt", "{R}"},
		want:  all(completed, payloadIs("uvfile u.txt {UVT} {UVH} 11 0", "hello world")),
	}}
}

// noAnonymousCaps gives nobody caps and every other built-in user none, as a
// repository that denies anonymous access is set up.
func noAnonymousCaps(caps string) string {
	return "UPDATE user SET cap='' WHERE login IN ('anonymous','reader','developer');" +
		" UPDATE user SET cap='" + caps + "' WHERE login='nobody'"
}

// serverCloneCases are Sections 8.1 and 8.2.
func serverCloneCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.8.1.invalid-seqno", Req: "SP-8.1-1"},
		body: []string{"{CV}", "clone 3 0", "{R}"},
		want: all(syncReply, onlyError("invalid clone sequence number"), nothingBut("error ")),
	}, {
		Case: Case{ID: "sync.8.1.invalid-seqno-rolls-back", Req: "SP-8.1-1"},
		body: []string{"{CV}", pushCard, "file {N1} {N1.size}", "{N1.data}", "clone 3 0", "{R}"},
		want: all(syncReply, onlyError("invalid clone sequence number"), held("N1", 0)),
	}, {
		// Section 8.1 withholds the fatal from a SEQNO that is not
		// digit-only and says no more. fossil(1) 2.28 builds differ: the
		// macOS build sends clone_seqno 0, the Linux build CI pins sends the
		// invalid-sequence error alone. Either way no content is sent, which
		// is what the case holds go-libfossil to.
		Case: Case{ID: "sync.8.1.negative-seqno", Req: "SP-8.1-1"},
		body: []string{"{CV}", "clone 3 -1", "{R}"},
		want: all(syncReply, noCard("cfile "), either(
			all(completed, noErrors, withCard("clone_seqno 0")),
			all(onlyError("invalid clone sequence number"), nothingBut("error ")))),
	}, {
		Case: Case{ID: "sync.8.2.reply-order", Req: "SP-8.2-1"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, ridOrder(1), before("cfile ", "clone_seqno 0"),
			before("clone_seqno 0", "push {SC} {PC}")),
	}, {
		Case: Case{ID: "sync.8.2.from-seqno", Req: "SP-8.2-1"},
		body: []string{"{CV}", "clone 3 4", "{R}"},
		want: all(completed, ridOrder(4)),
	}, {
		Case:  Case{ID: "sync.8.2.refusal-shape", Req: "SP-8.2-3"},
		setup: closed,
		body:  []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, onlyError("not authorized to clone"),
			before("push {SC} {PC}", "error "), nothingBut("push ", "error ", "pragma ")),
	}, {
		Case: Case{ID: "sync.8.2.cleanup-gimme", Req: "SP-8.2-6"},
		body: []string{"{CV}", "gimme {A1}", "{R}"},
		want: all(completed, noErrors, noCard("file ")),
	}, {
		Case: Case{ID: "sync.8.2.igot-ignored", Req: "SP-8.2-7"},
		body: []string{"{CV}", "clone 3 1", "igot {U}", "{R}"},
		want: all(completed, noErrors, noCard("gimme {U}")),
	}}
}
