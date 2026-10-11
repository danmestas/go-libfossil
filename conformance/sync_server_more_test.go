package conformance

// More server cases: version negotiation and the SHA3 gate it drives,
// admission, logins go-libfossil does not take, and the pragmas it does
// not answer.

// serverVersionCases are Sections 3.2 and 9.1: server-version.
func serverVersionCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.3.2.server-version-before-clone", Req: "SP-3.2-8"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, before("pragma server-version ", "cfile ")),
	}, {
		Case: Case{ID: "sync.9.1.server-version-full", Req: "SP-9.1-1"},
		body: []string{"{CV}", pullCard, "{R}"},
		want: all(completed, count("pragma server-version ", 1),
			matches("pragma server-version ", `^pragma server-version \d+ \d{8} \d{6}$`)),
	}, {
		// A client-version without DATE and TIME gets no answer.
		Case: Case{ID: "sync.9.1.server-version-short", Req: "SP-9.1-1"},
		body: []string{"pragma client-version 22800", pullCard, "{R}"},
		want: all(completed, withCard("igot {A1}"), noCard("pragma server-version")),
	}}
}

// sha3Refused is the error Section 7.6 sends in place of a SHA3 artifact.
const sha3Refused = "Fossil version 2.0 or later required."

// serverGateCases are Section 7.6 on the server. A1 is named by SHA3 and
// B1 by SHA1, so each request asks for one of each: the gate refuses the
// first and the reply goes on to the second.
func serverGateCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.7.6.server-gate", Req: "SP-7.6-1"},
		body: []string{"{CV0}", pullCard, "gimme {A1}", "gimme {B1}", "{R}"},
		want: all(completed, onlyError(sha3Refused), noCard("file {A1}"),
			withCard("file {B1} {B1.size}")),
	}, {
		// Appendix A.2: the error takes each SHA3 artifact's place.
		Case: Case{ID: "sync.7.6.clone-gate", Req: "SP-7.6-1"},
		body: []string{"{CV0}", "clone 3 1", "{R}"},
		want: all(completed, present("cfile {B1} "), eachCard(sha1Only),
			withCard(`error Fossil\sversion\s2.0\sor\slater\srequired.`),
			withCard("clone_seqno 0")),
	}, {
		// The file the first gimme staged stays: the gate appends.
		Case: Case{ID: "sync.10.1.sha3-gate-appends", Req: "SP-10.1-3"},
		body: []string{"{CV0}", pullCard, "gimme {B1}", "gimme {A1}", "{R}"},
		want: all(completed, onlyError(sha3Refused), withCard("file {B1} {B1.size}")),
	}, {
		Case: Case{ID: "sync.7.6.server-gate-no-version", Req: "SP-7.6-2"},
		body: []string{pullCard, "gimme {A1}", "gimme {B1}", "{R}"},
		want: all(completed, onlyError(sha3Refused), noCard("file {A1}"),
			withCard("file {B1} {B1.size}")),
	}, {
		// UVH is a SHA3 name.
		Case:    Case{ID: "sync.7.6.uvgimme-gate", Req: "SP-7.6-1"},
		body:    []string{"{CV0}", pullCard, "uvgimme u.txt", "{R}"},
		want:    all(completed, onlyError(sha3Refused), noCard("uvfile ")),
		notSent: "fossil(1) 2.28 crashes on it (SIGSEGV in send_unclustered)",
	}}
}

// sha1Only checks a cfile card names its artifact by SHA1.
func sha1Only(c wireCard) error {
	if c.op() == "cfile" && len(c.tokens()[1]) != 40 {
		return expectf("%q sends a SHA3 artifact", c.line)
	}
	return nil
}

// serverAdmissionCases are Sections 6.4, 6.5, 7.7 and 9.3: what the
// server admits.
func serverAdmissionCases() []syncServerCase {
	push := []string{"{CV}", pushCard, "file {N1} {N1.size}", "{N1.data}", "{R}"}
	return []syncServerCase{{
		// The igot after the file shows parsing went on.
		Case:  Case{ID: "sync.7.7.shunned-file-server", Req: "SP-7.7-1"},
		setup: []string{"INSERT INTO shun(uuid, mtime) VALUES('{N1}', 0)"},
		body: []string{"{CV}", pushCard, "file {N1} {N1.size}", "{N1.data}", "igot {U}",
			"{R}"},
		want: all(completed, noErrors, held("N1", 0), withCard("gimme {U}")),
	}, {
		// alice's password is stored as itself, not as the shared secret.
		Case: Case{ID: "sync.6.4.cleartext-fallback", Req: "SP-6.4-4"},
		setup: append([]string{"UPDATE user SET pw='secret' WHERE login='alice'"},
			closed...),
		body: append([]string{"{LOGIN alice secret}"}, push...),
		want: all(completed, noErrors, held("N1", 1)),
	}, {
		Case:   Case{ID: "sync.6.5.server-cookie-login", Req: "SP-6.5-4"},
		setup:  closed,
		cookie: "alice secret",
		body:   push,
		want:   all(completed, noErrors, held("N1", 1)),
	}, {
		Case: Case{ID: "sync.9.3.config-needs-admin", Req: "SP-9.3-6"},
		body: []string{"{CV}", pushCard, "config /config {CFGP.size}", "{CFGP.data}", "{R}"},
		want: all(completed, onlyError("not authorized to push configuration"),
			repoCount("SELECT count(*) FROM config WHERE name='project-name'"+
				" AND value='pushed'", 0)),
	}}
}

// serverPragmaMoreCases are Sections 5.3, 8.4 and 9: pragmas go-libfossil
// does not answer.
func serverPragmaMoreCases() []syncServerCase {
	lock := []string{"{LOGIN alice secret}", "{CV}", pushCard}
	return []syncServerCase{{
		// Unlocked by its holder, the check-in can be locked by another.
		Case: Case{ID: "sync.9.4.ci-unlock", Req: "SP-9.4-2"},
		body: append(append([]string{}, lock...), "pragma ci-lock {M2} {Z1}",
			"pragma ci-unlock {Z1}", "pragma ci-lock {M2} {Z2}", "{R}"),
		want: all(completed, noErrors, noCard("pragma ci-lock-fail")),
	}, {
		Case: Case{ID: "sync.9.5.no-links-unasked", Req: "SP-9.5-3"},
		body: []string{"{CV}", pullCard, "{R}"},
		want: all(completed, withCard("igot {A1}"), noCard("pragma link")),
	}, {
		// C is a cluster.
		Case: Case{ID: "sync.5.3.cluster-brackets", Req: "SP-5.3-3"},
		body: []string{"{CV}", pullCard, "pragma req-clusters", "{R}"},
		want: all(completed, before("# sending-clusters", "igot {C}"),
			before("igot {C}", "# end-of-clusters"), count("# sending-clusters", 1)),
	}, {
		// With C's tag gone there is no cluster to announce.
		Case:  Case{ID: "sync.5.3.cluster-brackets-empty", Req: "SP-5.3-3"},
		setup: []string{"DELETE FROM tagxref WHERE tagid=7"},
		body:  []string{"{CV}", pullCard, "pragma req-clusters", "{R}"},
		want: all(completed, withCard("igot {A1}"), noCard("# sending-clusters"),
			noCard("# end-of-clusters")),
	}, {
		// uv-sync is the setting that makes fossil(1) send its catalog
		// with a clone.
		Case: Case{ID: "sync.8.4.clone-uv-catalog", Req: "SP-8.4-2"},
		setup: []string{"INSERT INTO config(name, value, mtime) VALUES('uv-sync', '1', 0)",
			"DELETE FROM config WHERE name='uv-hash'"},
		body: []string{"{CV}", "clone 3 1", "{R}"},
		want: all(completed, count("pragma uv-pull-only", 1),
			withCard("uvigot u.txt {UVT} {UVH} 11")),
	}}
}
