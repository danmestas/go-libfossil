package conformance

import (
	"fmt"
	"strings"
)

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
	}, {
		// UVH is a SHA3 name: an unversioned file goes to an older server
		// no more than an artifact does. fossil(1) 2.28 fails an assertion
		// in blob_reset on this exchange, so it does not run.
		Case:    Case{ID: "sync.7.6.client-uv-gate", Req: "SP-7.6-1"},
		op:      clientOp{pull: true, push: true, uv: true},
		replies: [][]string{{"{SV0}", "pragma uv-push-ok", "{TS}"}},
		want:    every(sent(2, fossilizedRefusal), notSent(2, "uvfile u.txt")),
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
	}, {
		// The client's v.txt is newer than the peer's: its content wins and
		// is sent, as uvfile and not as the well-formed uvigot.
		Case: Case{ID: "sync.8.5.1.no-uvigot", Req: "SP-8.5.1-7"},
		op:   clientOp{pull: true, push: true, uv: true},
		setup: append(uvRows(uvN1("v.txt", "{UVT1}")),
			"INSERT INTO config(name, value, mtime) VALUES('max-upload', '1', 0)"),
		replies: [][]string{reply("pragma uv-push-ok", "uvigot v.txt {UVT} {UVH} 11")},
		want:    neverSent("uvigot "),
		fc:      true,
		fossil: "fossil(1) 2.28 sends the well-formed uvigot for a file once its output " +
			"passes max-upload: the source defect Section 8.5.1 names",
	}}
}

// clientLinkCases are Section 9.5's links on the client: the share-links
// mode that asks for them and the checks a link must pass to be kept.
func clientLinkCases() []syncClientCase {
	return append(clientLinkStoreCases(), clientLinkDiscardCases()...)
}

// opShareLinks pulls in share-links mode, fossil(1)'s --share-links.
var opShareLinks = clientOp{pull: true, shareLinks: true}

// linkRow counts the client's config rows for link:URL with value and mtime.
func linkRow(url, value string, mtime int) string {
	return fmt.Sprintf("SELECT count(*) FROM config WHERE name='link:%s' AND value='%s' "+
		"AND mtime=%d", url, value, mtime)
}

// noLinks checks that the client stored no link.
var noLinks = clientRows("SELECT count(*) FROM config WHERE name GLOB 'link:*'", 0)

// argNotEchoed checks that no request carries text from a link's ARG,
// which the cases spell with "opaque".
var argNotEchoed = eachCardSent(func(c wireCard) error {
	if strings.Contains(c.line, "opaque") {
		return expectf("%q echoes the link's ARG", c.line)
	}
	return nil
})

// clientLinkStoreCases are the request side and a kept link. In each, the
// igot after the links and the gimme it draws show the reply was read on.
func clientLinkStoreCases() []syncClientCase {
	return []syncClientCase{{
		Case:    Case{ID: "sync.9.5.req-links-sent", Req: "SP-9.5-6"},
		op:      opShareLinks,
		replies: [][]string{reply("igot {U}")},
		want:    every(sent(1, "pragma req-links"), sentBefore(1, "pull ", "pragma req-links")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.9.5.no-req-links-unshared", Req: "SP-9.5-6"},
		op:      opPull,
		replies: [][]string{reply("igot {U}")},
		want:    every(sent(2, "gimme {U}"), neverSent("pragma req-links")),
		fc:      true,
	}, {
		// Without share-links the client discards a link, and never
		// echoes its ARG.
		Case: Case{ID: "sync.9.5.link-not-echoed", Req: "SP-9.5-4"},
		op:   opPull,
		replies: [][]string{reply(`pragma link https://example.com/r opaque\sarg 1767225600`,
			"igot {U}")},
		want: every(sent(2, "gimme {U}"), argNotEchoed, noLinks),
		fc:   true,
	}, {
		// URL and ARG are stored defossilized: the space, backslash and
		// newline escapes come back as the bytes they stand for.
		Case: Case{ID: "sync.9.5.link-stored", Req: "SP-9.5-4"},
		op:   opShareLinks,
		replies: [][]string{reply(
			`pragma link https://example.com/a\sb opaque\sarg\\x\n 1767225600`, "igot {U}")},
		want: every(sent(2, "gimme {U}"), argNotEchoed,
			clientRows(linkRow("https://example.com/a b", "opaque arg\\x\n", 1767225600), 1),
			clientRows("SELECT count(*) FROM config WHERE name GLOB 'link:*'", 1)),
		fc: true,
	}, {
		// Acceptance does not depend on the request that carried req-links:
		// a link in round 2 is kept too.
		Case: Case{ID: "sync.9.5.link-later-round", Req: "SP-9.5-5"},
		op:   opShareLinks,
		replies: [][]string{reply("igot {U}"),
			reply("pragma link https://example.com/r opaque 100")},
		want: every(sent(2, "gimme {U}"),
			clientRows(linkRow("https://example.com/r", "opaque", 100), 1)),
		fc: true,
	}, {
		// A push+pull session still asks for links.
		Case:    Case{ID: "sync.9.5.req-links-push-pull", Req: "SP-9.5-6"},
		op:      clientOp{pull: true, push: true, shareLinks: true},
		replies: [][]string{reply("igot {U}")},
		want:    sent(1, "pragma req-links"),
		fc:      true,
	}, {
		// A link already held is replaced only by a strictly newer one:
		// a is older than its link, b newer and c the same age.
		Case: Case{ID: "sync.9.5.link-newer-mtime", Req: "SP-9.5-4"},
		op:   opShareLinks,
		setup: []string{"INSERT INTO config(name, value, mtime) VALUES" +
			"('link:https://example.com/a', 'a0', 100), " +
			"('link:https://example.com/b', 'b0', 300), " +
			"('link:https://example.com/c', 'c0', 200)"},
		replies: [][]string{reply("pragma link https://example.com/a opaque-a1 200",
			"pragma link https://example.com/b opaque-b1 200",
			"pragma link https://example.com/c opaque-c1 200", "igot {U}")},
		want: every(sent(2, "gimme {U}"), argNotEchoed,
			clientRows(linkRow("https://example.com/a", "opaque-a1", 200), 1),
			clientRows(linkRow("https://example.com/b", "b0", 300), 1),
			clientRows(linkRow("https://example.com/c", "c0", 200), 1)),
		fc: true,
	}}
}

// clientLinkDiscardCases are links a client in share-links mode discards.
func clientLinkDiscardCases() []syncClientCase {
	return []syncClientCase{{
		// ssh: and file: URLs parse, but not as HTTP(S).
		Case: Case{ID: "sync.9.5.link-not-http", Req: "SP-9.5-5"},
		op:   opShareLinks,
		replies: [][]string{reply("pragma link ssh://example.com/r opaque 100",
			"pragma link file:/r opaque 100", "igot {U}")},
		want: every(sent(2, "gimme {U}"), argNotEchoed, noLinks),
		fc:   true,
	}, {
		// An http: prefix with a bad port, and a form no URL parser takes.
		Case: Case{ID: "sync.9.5.link-unparsed-url", Req: "SP-9.5-5"},
		op:   opShareLinks,
		replies: [][]string{reply("pragma link https://example.com:8x/r opaque 100",
			"pragma link example.com/r opaque 100", "igot {U}")},
		want: every(sent(2, "gimme {U}"), argNotEchoed, noLinks),
		fc:   true,
		fossil: "fossil(1) 2.28 fails fatally on a link URL its parser rejects " +
			"rather than discarding the link",
	}, {
		Case: Case{ID: "sync.9.5.link-non-positive-mtime", Req: "SP-9.5-5"},
		op:   opShareLinks,
		replies: [][]string{reply("pragma link https://example.com/a opaque 0",
			"pragma link https://example.com/b opaque -5",
			"pragma link https://example.com/c opaque none", "igot {U}")},
		want: every(sent(2, "gimme {U}"), argNotEchoed, noLinks),
		fc:   true,
	}, {
		// MTIME is decimal; fossil(1) reads it with strtoll base 0.
		Case: Case{ID: "sync.9.5.link-mtime-not-decimal", Req: "SP-9.5-5"},
		op:   opShareLinks,
		replies: [][]string{reply("pragma link https://example.com/a opaque 0x10",
			"pragma link https://example.com/b opaque 12z", "igot {U}")},
		want: every(sent(2, "gimme {U}"), argNotEchoed, noLinks),
		fc:   true,
		fossil: "fossil(1) 2.28 reads MTIME with strtoll(..., 0), so it keeps 0x10 " +
			"as 16 and 12z as 12",
	}, {
		Case: Case{ID: "sync.9.5.link-token-count", Req: "SP-9.5-5"},
		op:   opShareLinks,
		replies: [][]string{reply("pragma link https://example.com/a opaque",
			"pragma link https://example.com/b opaque 100 more", "igot {U}")},
		want: every(sent(2, "gimme {U}"), argNotEchoed, noLinks),
		fc:   true,
	}}
}
