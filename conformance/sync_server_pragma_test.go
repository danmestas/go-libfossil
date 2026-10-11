package conformance

import "strings"

// serverDirectionCases are Section 8.5 and 8.5.1: project codes and the
// unversioned catalog.
func serverDirectionCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.8.5.project-mismatch-pull", Req: "SP-8.5-1"},
		body: []string{"{CV}", "pull {SC} {Z1}", "{R}"},
		want: all(completed, onlyError("wrong project"), noCard("igot ")),
	}, {
		Case: Case{ID: "sync.8.5.project-mismatch-push", Req: "SP-8.5-1"},
		body: []string{"{CV}", "push {SC} {Z1}", "file {N1} {N1.size}", "{N1.data}", "{R}"},
		want: all(completed, onlyError("wrong project"), held("N1", 0)),
	}, {
		Case: Case{ID: "sync.8.5.1.hash-match", Req: "SP-8.5.1-4"},
		body: []string{"{CV}", pullCard, "pragma uv-hash {UVCAT}", "{R}"},
		want: all(completed, noCard("uvigot "), noCard("pragma uv-")),
	}, {
		Case:  Case{ID: "sync.8.5.1.catalog-with-tombstones", Req: "SP-8.5.1-4"},
		setup: tombstone,
		body:  []string{"{CV}", pullCard, "pragma uv-hash {Z1}", "{R}"},
		want: all(completed, withCard("uvigot u.txt {UVT} {UVH} 11"),
			withCard("uvigot t.txt {UVT} - 0"), count("uvigot ", 2)),
	}, {
		Case: Case{ID: "sync.9.2.push-ok-needs-write", Req: "SP-9.2-1"},
		body: []string{"{CV}", pullCard, "pragma uv-hash {Z1}", "{R}"},
		want: all(completed, count("pragma uv-pull-only", 1), noCard("pragma uv-push-ok")),
	}, {
		Case: Case{ID: "sync.9.2.push-ok-with-write", Req: "SP-9.2-1"},
		body: []string{"{LOGIN uvw secret}", "{CV}", pullCard, "pragma uv-hash {Z1}", "{R}"},
		want: all(completed, count("pragma uv-push-ok", 1), noCard("pragma uv-pull-only")),
	}}
}

// serverPragmaCases are Section 9: unknown and wrong-direction pragmas,
// a bare pragma, and the commit lock.
func serverPragmaCases() []syncServerCase {
	return []syncServerCase{{
		Case: Case{ID: "sync.9.unknown-pragma-server", Req: "SP-9-1"},
		body: []string{"{CV}", "pragma zzz 1", pullCard, "{R}"},
		want: all(completed, noErrors, withCard("igot {A1}")),
	}, {
		Case: Case{ID: "sync.9.wrong-direction-server", Req: "SP-9-2"},
		body: []string{"{CV}", "pragma uv-push-ok", "pragma server-version 1", pullCard, "{R}"},
		want: all(completed, noErrors, withCard("igot {A1}")),
	}, {
		Case: Case{ID: "sync.9.bare-pragma", Req: "SP-9-3"},
		body: []string{"{CV}", pullCard, "pragma", "gimme {A1}", "{R}"},
		want: all(completed, onlyError("bad command: pragma\n"), withCard("file {A1} {A1.size}")),
	}, {
		// Logged in, so the lock's LOGIN is a name: fossil(1) leaves it
		// empty for nobody, an empty token no canonical card can carry.
		Case: Case{ID: "sync.9.4.ci-lock-conflict", Req: "SP-9.4-1"},
		body: []string{"{LOGIN alice secret}", "{CV}", pushCard, "pragma ci-lock {M2} aaaa",
			"pragma ci-lock {M2} bbbb", "{R}"},
		want: all(completed, count("pragma ci-lock-fail ", 1),
			matches("pragma ci-lock-fail ", `^pragma ci-lock-fail alice \d+$`)),
	}, {
		Case: Case{ID: "sync.9.4.ci-lock-same-client", Req: "SP-9.4-1"},
		body: []string{"{LOGIN alice secret}", "{CV}", pushCard, "pragma ci-lock {M2} aaaa",
			"pragma ci-lock {M2} aaaa", "{R}"},
		want: all(completed, noErrors, noCard("pragma ci-lock-fail")),
	}}
}

// serverConfigCases are Section 9.3.
func serverConfigCases() []syncServerCase {
	return []syncServerCase{{
		Case:  Case{ID: "sync.9.3.reqconfig-no-read", Req: "SP-9.3-1"},
		setup: closed,
		body:  []string{"{CV}", "reqconfig /project", "{R}"},
		want:  all(completed, noErrors, noCard("config ")),
	}, {
		Case: Case{ID: "sync.9.3.reqconfig-unknown", Req: "SP-9.3-2"},
		body: []string{"{CV}", pullCard, "reqconfig /bogus", "{R}"},
		want: all(completed, noErrors, noCard("config ")),
	}, {
		Case: Case{ID: "sync.9.3.scalar-form", Req: "SP-9.3-3"},
		body: []string{"{CV}", pullCard, "reqconfig /project", "{R}"},
		want: all(completed, present("config /config "), eachCard(scalarConfig)),
	}, {
		// A signed SIZE is not a config card at all, so no payload is
		// read: the igot after it is the next card.
		Case: Case{ID: "sync.9.3.config-signed-size", Req: "SP-9.3-5"},
		body: []string{"{CV}", pushCard, "config /config -1", "igot {U}", "{R}"},
		want: all(completed, onlyError("bad command: config /config -1\n"), withCard("gimme {U}")),
	}}
}

// scalarConfig checks a config card is the Section 9.3 scalar form:
// config /config SIZE, then MTIME 'NAME' value 'VALUE' and one LF.
func scalarConfig(c wireCard) error {
	if c.op() != "config" {
		return nil
	}
	if !strings.HasPrefix(c.line, "config /config ") {
		return expectf("%q is not config /config", c.line)
	}
	text := string(c.payload)
	if !scalarPayload.MatchString(text) {
		return expectf("%q: payload %q is not MTIME 'NAME' value 'VALUE' LF", c.line, text)
	}
	return nil
}
