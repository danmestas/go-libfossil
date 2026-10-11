package conformance

import "strings"

// More client cases: the SHA3 gate, admission, and the pull-only message.

// fossilizedRefusal is sha3Refused as the card carries it.
const fossilizedRefusal = `error Fossil\sversion\s2.0\sor\slater\srequired.`

// clientGateCases are Sections 7.6 and 9.1 on the client. A1 is named by
// SHA3 and B1 by SHA1; the peer asks for both.
func clientGateCases() []syncClientCase {
	both := []string{"gimme {A1}", "gimme {B1}", "{TS}"}
	return []syncClientCase{{
		Case:    Case{ID: "sync.7.6.client-gate", Req: "SP-7.6-1"},
		op:      opPush,
		setup:   quiet,
		replies: [][]string{append([]string{"{SV0}"}, both...)},
		want: every(sent(2, fossilizedRefusal), sent(2, "file {B1} {B1.size}"),
			notSent(2, "file {A1}")),
		fc: true,
	}, {
		// A server-version with no DATE or TIME still sets VERSION.
		Case:    Case{ID: "sync.9.1.client-records-short", Req: "SP-9.1-2"},
		op:      opPush,
		setup:   quiet,
		replies: [][]string{append([]string{"pragma server-version 19999"}, both...)},
		want: every(sent(2, fossilizedRefusal), sent(2, "file {B1} {B1.size}"),
			notSent(2, "file {A1}")),
		fc: true,
	}, {
		// Before any server-version the client's own release applies.
		Case:    Case{ID: "sync.7.6.client-initial-version", Req: "SP-7.6-3"},
		op:      opPush,
		setup:   quiet,
		replies: [][]string{{"gimme {A1}", "{TS}"}},
		want:    every(sent(2, "file {A1} {A1.size}"), notSent(2, "error ")),
		fc:      true,
	}}
}

// clientAdmissionCases are Section 7.7 on the client.
func clientAdmissionCases() []syncClientCase {
	shunN1 := []string{"INSERT INTO shun(uuid, mtime) VALUES('{N1}', 0)"}
	return []syncClientCase{{
		// The igot after the file shows the reply was read on.
		Case:    Case{ID: "sync.7.7.shunned-file-client", Req: "SP-7.7-1"},
		op:      opPull,
		setup:   shunN1,
		replies: [][]string{reply("file {N1} {N1.size}", "{N1.data}", "igot {U}")},
		want:    every(notStored("N1"), sent(2, "gimme {U}")),
		fc:      true,
	}, {
		Case:  Case{ID: "sync.7.7.shunned-cfile", Req: "SP-7.7-2"},
		op:    opPull,
		setup: shunN1,
		replies: [][]string{reply("cfile {N1} {N1.size} {N1.zsize}", "{N1.z}", "{N1.pad}",
			"igot {U}")},
		want: every(notStored("N1"), sent(2, "gimme {U}")),
		fc:   true,
	}, {
		// The basis S is shunned, not the target: T is kept as a delta,
		// and S, which the scan leaves out, is never asked for. fossil(1)
		// 2.28 aborts on this exchange, checking T's hash, so it does not
		// run.
		Case:    Case{ID: "sync.7.7.shunned-basis", Req: "SP-7.7-3"},
		op:      opPull,
		setup:   []string{"INSERT INTO shun(uuid, mtime) VALUES('{S}', 0)"},
		replies: [][]string{reply("file {T} {S} {DTS.size}", "{DTS.data}")},
		want: every(clientRows("SELECT count(*) FROM blob WHERE uuid='{T}'", 1),
			neverSent("gimme {S}")),
	}}
}

// clientPullOnlyCases are Section 10.2's pull-only message.
func clientPullOnlyCases() []syncClientCase {
	return []syncClientCase{{
		// The request already built keeps push; the one after drops it.
		Case:  Case{ID: "sync.10.2.pull-only", Req: "SP-10.2-2"},
		op:    opSync,
		setup: quiet,
		replies: [][]string{reply(`message pull\sonly\sx`, "igot {U}"),
			reply("igot {U2}")},
		want: every(sentPrefix(2, "push "), sentPrefix(3, "pull "), notSent(3, "push ")),
		fc:   true,
	}, {
		// The gimme before the message is answered, the one after is not.
		Case:    Case{ID: "sync.10.2.pull-only-gimme", Req: "SP-10.2-2"},
		op:      opSync,
		setup:   quiet,
		replies: [][]string{reply("gimme {A1}", `message pull\sonly\s`, "gimme {A2}")},
		want:    every(sent(2, "file {A1} {A1.size}"), notSent(2, "file {A2}")),
		fc:      true,
	}, {
		Case:  Case{ID: "sync.10.2.other-message", Req: "SP-10.2-3"},
		op:    opSync,
		setup: quiet,
		replies: [][]string{reply(`message pull\sonlyx`, "igot {U}"),
			reply("igot {U2}")},
		want: every(sentPrefix(2, "push "), sentPrefix(3, "push ")),
		fc:   true,
	}}
}

// clientShortUVIGotCases is Section 8.5.1's ban on the short uvigot. A
// one-byte max-upload makes fossil(1) reach its output cap at once.
func clientShortUVIGotCases() []syncClientCase {
	return []syncClientCase{{
		Case: Case{ID: "sync.8.5.1.no-short-uvigot", Req: "SP-8.5.1-7"},
		op:   clientOp{pull: true, push: true, uv: true},
		setup: append(uvRows("'t.txt', 1, {UVT1}, NULL, 0, 0, NULL", uvN1("v.txt", "{UVT1}")),
			"INSERT INTO config(name, value, mtime) VALUES('max-upload', '1', 0)"),
		replies: [][]string{reply("pragma uv-push-ok", "uvigot t.txt {UVT} {N1} 15",
			"uvigot v.txt {UVT} {UVH} 11")},
		want: eachCardSent(func(c wireCard) error {
			// The short form leaves HASH empty: four tokens, two SPs together.
			if c.op() == "uvigot" && len(strings.Fields(c.line)) != 5 {
				return expectf("%q is the short uvigot", c.line)
			}
			return nil
		}),
		fc: true,
		fossil: "fossil(1) 2.28 sends the short uvigot for a tombstone once its output " +
			"passes max-upload: the source defect Section 8.5.1 names",
	}}
}
