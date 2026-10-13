package conformance

import (
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
)

// uvRows adds unversioned rows to the client, then drops its cached
// catalog hash so they count. Content is stored uncompressed.
func uvRows(rows ...string) []string {
	out := append([]string{}, quiet...)
	for _, r := range rows {
		out = append(out, "INSERT INTO unversioned"+
			"(name, rcvid, mtime, hash, sz, encoding, content) VALUES("+r+")")
	}
	return append(out, "DELETE FROM config WHERE name='uv-hash'")
}

// uvN1 is the values of an unversioned row named name holding N1's
// content, uncompressed, at mtime.
func uvN1(name, mtime string) string {
	return "'" + name + "', 1, " + mtime +
		", '{N1}', 15, 0, CAST('a new artifact'||char(10) AS BLOB)"
}

// catalogHash checks request 1's uv-hash against Section 8.5.1's formula,
// computed here from the client's rows: the SHA-1 of "NAME YYYY-MM-DD
// HH:MM:SS HASH" LF per live row, sorted by name bytes.
func catalogHash(r *clientRun) error {
	conn, err := db.OpenSQL(r.repo, db.OpenConfig{}, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	rows, err := conn.Query("SELECT name, datetime(mtime, 'unixepoch'), hash FROM unversioned" +
		" WHERE hash IS NOT NULL ORDER BY name COLLATE BINARY")
	if err != nil {
		return err
	}
	defer rows.Close()
	var text strings.Builder
	for rows.Next() {
		var name, when, hash string
		if err := rows.Scan(&name, &when, &hash); err != nil {
			return err
		}
		fmt.Fprintf(&text, "%s %s %s\n", name, when, hash)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return sent(1, "pragma uv-hash "+sha1Hex(text.String()))(r)
}

// clientUVCases are Section 8.5.1's send table, uv-hash and catalog hash.
func clientUVCases() []syncClientCase {
	uvSync := clientOp{pull: true, push: true, uv: true}
	return []syncClientCase{{
		Case:    Case{ID: "sync.8.5.1.seed-excludes-tombstones", Req: "SP-8.5.1-1"},
		op:      uvSync,
		setup:   uvRows("'t.txt', 1, {UVT}, NULL, 0, 0, NULL"),
		replies: [][]string{reply("pragma uv-push-ok")},
		want: every(sent(2, "uvfile u.txt {UVT} {UVH} 11 0"),
			neverSent("uvfile t.txt")),
		fc: true,
	}, {
		// uvfile, with six tokens, is the longest card a client sends.
		Case:    Case{ID: "sync.3.9.sender-six-tokens-client", Req: "SP-3.9-1"},
		op:      clientOp{pull: true, push: true, uv: true, login: true},
		setup:   quiet,
		replies: [][]string{reply("pragma uv-push-ok")},
		want:    every(sent(2, "uvfile u.txt {UVT} {UVH} 11 0"), eachCardSent(sixTokens)),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.8.5.1.uv-hash-once", Req: "SP-8.5.1-2"},
		op:      uvSync,
		setup:   quiet,
		replies: [][]string{reply("igot {U}"), reply("igot {U2}")},
		want: every(atLeast(3), sent(1, "pragma uv-hash {UVCAT}"),
			notSent(2, "pragma uv-hash"), notSent(3, "pragma uv-hash")),
		fc: true,
	}, {
		// Names that sort differently by bytes and by case, and a
		// tombstone, which the hash leaves out.
		Case: Case{ID: "sync.8.5.1.catalog-hash", Req: "SP-8.5.1-3"},
		op:   uvSync,
		setup: uvRows(uvN1("a", "{UVT}"),
			uvN1("B", "{UVT1}"),
			"'t.txt', 1, {UVT}, NULL, 0, 0, NULL"),
		replies: [][]string{reply()},
		want:    catalogHash,
		fc:      true,
	}, {
		Case:  Case{ID: "sync.8.5.1.pull-only-drops", Req: "SP-8.5.1-6"},
		op:    uvSync,
		setup: uvRows(uvN1("v.txt", "{UVT1}")),
		replies: [][]string{reply("pragma uv-pull-only", "uvigot u.txt 1767225000 {UVH} 11",
			"uvigot v.txt {UVT} {UVH} 11")},
		want: neverSent("uvfile "),
		fc:   true,
	}}
}

// clientUVStatusCases are Table 6: what a client does for each status a
// uvigot has against its own row.
func clientUVStatusCases() []syncClientCase {
	return []syncClientCase{{
		// s0.txt is not held (status 0); u.txt is held with other content
		// and is older (status 1), so it is deleted and asked for.
		Case: Case{ID: "sync.8.5.1.status-0-1", Req: "SP-8.5.1-5"},
		op:   clientOp{pull: true, push: true, uv: true},
		replies: [][]string{reply("pragma uv-pull-only", "uvigot s0.txt {UVT} {N1} 15",
			"uvigot u.txt {UVT1} {N1} 15")},
		want: every(sent(2, "uvgimme s0.txt"), sent(2, "uvgimme u.txt"),
			clientRows("SELECT count(*) FROM unversioned WHERE name='u.txt'", 0)),
	}, {
		Case:    Case{ID: "sync.8.5.1.status-2", Req: "SP-8.5.1-5"},
		op:      clientOp{pull: true, push: true, uv: true},
		replies: [][]string{reply("pragma uv-pull-only", "uvigot u.txt {UVT1} {UVH} 11")},
		want: every(neverSent("uvgimme "),
			clientRows("SELECT count(*) FROM unversioned WHERE name='u.txt' AND mtime={UVT1}", 1)),
	}, {
		// u.txt is the peer's content, older (status 4): only its mtime is
		// sent. v.txt is other content, newer (status 5): it is sent
		// whole. w.txt matches (status 3): nothing is sent.
		Case: Case{ID: "sync.8.5.1.status-3-4-5", Req: "SP-8.5.1-5"},
		op:   clientOp{pull: true, push: true, uv: true},
		setup: uvRows(uvN1("v.txt", "{UVT1}"),
			uvN1("w.txt", "{UVT}")),
		replies: [][]string{reply("pragma uv-push-ok", "uvigot u.txt 1767225000 {UVH} 11",
			"uvigot v.txt {UVT} {UVH} 11", "uvigot w.txt {UVT} {N1} 15")},
		want: every(sent(2, "uvfile u.txt {UVT} {UVH} 11 4"),
			sentPayload(2, "uvfile v.txt {UVT1} {N1} 15 0", "{N1.data}"),
			notSent(2, "uvfile w.txt")),
		fc: true,
	}}
}

// clientMessageCases are Sections 9 and 10 on the client: pragmas, error
// and message cards.
func clientMessageCases() []syncClientCase {
	stored1 := []string{"file {N1} {N1.size}", "{N1.data}"}
	return []syncClientCase{{
		Case:    Case{ID: "sync.9.unknown-pragma", Req: "SP-9-1"},
		op:      opPull,
		replies: [][]string{reply(append([]string{"pragma zzz a b c d e"}, stored1...)...)},
		want:    stored("N1"),
		fc:      true,
	}, {
		// The byte after a config payload is consumed whatever it is.
		Case: Case{ID: "sync.9.3.config-opaque", Req: "SP-9.3-4"},
		op:   opPull,
		replies: [][]string{reply(append([]string{"config /zzz 3", "{CFGX.data}"},
			stored1...)...)},
		want: stored("N1"),
		fc:   true,
	}, {
		Case:    Case{ID: "sync.9.wrong-direction-client", Req: "SP-9-2"},
		op:      opPull,
		replies: [][]string{reply(append([]string{"pragma client-version 1"}, stored1...)...)},
		want:    stored("N1"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.9.5.send-private-every-round", Req: "SP-9.5-2"},
		op:      clientOp{pull: true, private: true},
		replies: [][]string{reply("igot {U}"), reply("igot {U2}")},
		want: every(atLeast(3), eachRequest(func(i int, req peerRequest) error {
			if index(req.cards, "pragma send-private") < 0 {
				return expectf("request %d has no send-private", i)
			}
			return nil
		})),
		fc: true,
	}, {
		Case:    Case{ID: "sync.10.1.error-prefix", Req: "SP-10.1-1"},
		op:      opPull,
		replies: [][]string{reply(append(stored1, "error boom", "igot {U}")...)},
		want:    every(stored("N1"), notStored("U"), neverSent("gimme {U}")),
		fc:      true,
	}, {
		// N1 came in the first round; an error in the second keeps it,
		// and nothing continues the session.
		Case:    Case{ID: "sync.10.1.later-error-prefix", Req: "SP-10.1-1"},
		op:      opSync,
		setup:   quiet,
		replies: [][]string{reply(append(stored1, "gimme {A1}")...), reply("error x")},
		want:    every(stored("N1"), rounds(2)),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.10.1.error-continues-when-due", Req: "SP-10.1-1"},
		op:      opPull,
		replies: [][]string{reply("igot {U}", "error x")},
		want:    sent(2, "gimme {U}"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.10.2.message-advisory", Req: "SP-10.2-1"},
		op:      opPull,
		replies: [][]string{reply(append([]string{`message hello\sworld`}, stored1...)...)},
		want:    every(stored("N1"), reported("hello world")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.10.2.message-arity", Req: "SP-10.2-1"},
		op:      opPull,
		replies: [][]string{reply("message a b", "igot {U}")},
		want:    every(rounds(1), notStored("U")),
		fc:      true,
	}}
}

// clientIntegrityCases are Section 10.1's two kinds of handler-reported
// error. One that leaves the stored artifacts consistent, here a file card
// whose HASH is no artifact name, keeps the prefix and continues. A file or
// cfile whose content does not match its HASH is a fail-fast path: the
// round commits nothing, the igot before it included, and no further round
// starts. fossil(1) stores the content and aborts at commit.
func clientIntegrityCases() []syncClientCase {
	wrongDelta := []string{"igot {U}", "file {N1} {A1} {DTA1.size}", "{DTA1.data}"}
	return []syncClientCase{{
		Case:    Case{ID: "sync.10.1.content-error-prefix", Req: "SP-10.1-6"},
		op:      opPull,
		replies: [][]string{reply("igot {U}", "file {Z1}0 {N1.size}", "{N1.data}")},
		want:    every(phantom("U"), notStored("N1"), reported("malformed file line")),
		fc:      true,
	}, {
		// A uvfile whose content is not its HASH is a handler-reported
		// content error: the igot before it stays, and the session goes on.
		Case:    Case{ID: "sync.10.1.uvfile-wrong-hash-prefix", Req: "SP-10.1-6"},
		op:      clientOp{pull: true, push: true, uv: true},
		replies: [][]string{reply("igot {U}", "uvfile u.txt {UVT} {N1} 3 0", "abc")},
		want:    every(phantom("U"), mentions("hash mismatch", "HASH does not match")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.10.1.uvfile-wrong-hash-continues", Req: "SP-10.1-7"},
		op:      clientOp{pull: true, push: true, uv: true},
		replies: [][]string{reply("igot {U}", "uvfile u.txt {UVT} {N1} 3 0", "abc")},
		want:    sent(2, "gimme {U}"),
		fc:      true,
	}, {
		// The igot's phantom is a continuation condition (Section 3.7).
		Case:    Case{ID: "sync.10.1.content-error-continues", Req: "SP-10.1-7"},
		op:      opPull,
		replies: [][]string{reply("igot {U}", "file {Z1}0 {N1.size}", "{N1.data}")},
		want:    sent(2, "gimme {U}"),
		fc:      true,
	}, {
		// DTA1 turns A1 into T, not N1: the igot's phantom goes with the
		// round.
		Case:    Case{ID: "sync.10.1.integrity-error-prefix", Req: "SP-10.1-8"},
		op:      opPull,
		replies: [][]string{reply(wrongDelta...)},
		want: every(notStored("U"), notStored("N1"),
			mentions("mismatch", "wrong hash", "does not match")),
		fc: true,
	}, {
		// The same reply: the phantom would call for a second request, but
		// none is sent.
		Case:    Case{ID: "sync.10.1.integrity-error-continues", Req: "SP-10.1-8"},
		op:      opPull,
		replies: [][]string{reply(wrongDelta...)},
		want:    rounds(1),
		fc:      true,
	}, {
		// Full content that is not N1's, so no delta is involved.
		Case:    Case{ID: "sync.10.1.integrity-error-full-file", Req: "SP-10.1-8"},
		op:      opPull,
		replies: [][]string{reply("igot {U}", "file {N1} 3", "abc")},
		want:    every(notStored("U"), notStored("N1"), rounds(1)),
		fc:      true,
	}, {
		Case: Case{ID: "sync.10.1.integrity-error-cfile", Req: "SP-10.1-8"},
		op:   opPull,
		replies: [][]string{reply("igot {U}", "cfile {N1} {A1} {DTA1.size} {DTA1.zsize}",
			"{DTA1.z}", "{DTA1.pad}")},
		want: every(notStored("U"), notStored("N1"), rounds(1)),
		fc:   true,
	}, {
		// In a clone, A1, stored before the wrong delta, goes with the round,
		// and the clone_seqno that calls for another round is not followed.
		Case: Case{ID: "sync.10.1.integrity-error-clone", Req: "SP-10.1-8"},
		op:   clientOp{clone: true, login: true},
		replies: [][]string{append(cloneReply([]string{"A1"}),
			"cfile {N1} {A1} {DTA1.size} {DTA1.zsize}", "{DTA1.z}", "{DTA1.pad}",
			"clone_seqno 2", pushCard)},
		want: every(rounds(1), notKept("A1"),
			mentions("mismatch", "wrong hash", "does not match")),
	}, {
		// Section 9.5: avoid-delta-manifests is a hint for a pending commit;
		// the gimme after it is answered as ever.
		Case:    Case{ID: "sync.9.5.avoid-delta-manifests-transfer", Req: "SP-9.5-7"},
		op:      opSync,
		setup:   quiet,
		replies: [][]string{reply("pragma avoid-delta-manifests", "gimme {A1}")},
		want:    every(noFailure, sentPrefix(2, "file {A1} ")),
		fc:      true,
	}}
}
