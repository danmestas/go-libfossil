package conformance

import (
	"compress/zlib"
	"encoding/binary"
	"regexp"
)

// compressedReplies builds reply entities in the Section 4.1 container,
// each reply at its own zlib level; extra bytes, when given, follow each
// zlib stream.
func compressedReplies(levels []int, extra string,
	replies ...[]string) func(*syncFixture) [][]byte {
	return func(fx *syncFixture) [][]byte {
		var out [][]byte
		for i, r := range replies {
			out = append(out, append(compress(fx.body(r...), levels[i]), extra...))
		}
		return out
	}
}

// clientFramingCases are Sections 4.1 and 4.2: the container and the
// round.
func clientFramingCases() []syncClientCase {
	stored1 := reply("file {N1} {N1.size}", "{N1.data}")
	return []syncClientCase{{
		Case:    Case{ID: "sync.4.1.container-format-request", Req: "SP-4.1-1"},
		op:      opPull,
		replies: [][]string{reply()},
		want: eachRequest(func(i int, req peerRequest) error {
			if n := int(binary.BigEndian.Uint32(req.entity)); n != len(req.text) ||
				req.entity[4] != 0x78 {
				return expectf("request %d: length field %d for %d bytes, byte 4 %#x",
					i, n, len(req.text), req.entity[4])
			}
			return nil
		}),
	}, {
		// A length field larger than the inflated body is a capacity.
		Case: Case{ID: "sync.4.1.capacity-not-equality", Req: "SP-4.1-3"},
		op:   opPull,
		raw: func(fx *syncFixture) [][]byte {
			entity := compress(fx.body(stored1...), zlib.DefaultCompression)
			binary.BigEndian.PutUint32(entity, 4096)
			return [][]byte{entity}
		},
		ctype: typeCompressed,
		want:  stored("N1"),
	}, {
		Case:  Case{ID: "sync.4.1.trailing-bytes", Req: "SP-4.1-4"},
		op:    opPull,
		raw:   compressedReplies([]int{zlib.DefaultCompression}, "sixteen junk byt", stored1),
		ctype: typeCompressed,
		want:  stored("N1"),
	}, {
		Case: Case{ID: "sync.4.1.level-independent", Req: "SP-4.1-6"},
		op:   opPull,
		raw: compressedReplies([]int{zlib.BestSpeed, zlib.BestCompression}, "",
			reply("igot {N1}"), stored1),
		ctype: typeCompressed,
		want:  stored("N1"),
		fc:    true,
	}, {
		// A reply with no body at all is a complete round.
		Case: Case{ID: "sync.4.2.empty-body", Req: "SP-4.2-1"},
		op:   opPull,
		raw:  func(*syncFixture) [][]byte { return [][]byte{{}} },
		want: every(noFailure, rounds(1)),
		fc:   true,
	}}
}

// clientPayloadCases are Sections 3.9 and 5.1: the token window and
// byte-counted payloads.
func clientPayloadCases() []syncClientCase {
	return []syncClientCase{{
		Case: Case{ID: "sync.3.9.seventh-token-client", Req: "SP-3.9-2"},
		op:   clientOp{pull: true, push: true, uv: true},
		replies: [][]string{reply("uvfile v.txt {UVT} {N1} {N1.size} 0 extra", "{N1.data}",
			"igot {U}")},
		want: every(clientRows("SELECT count(*) FROM unversioned WHERE name='v.txt'", 1),
			sent(2, "gimme {U}")),
		fc: true,
	}, {
		Case:    Case{ID: "sync.5.1.file-binary", Req: "SP-5.1-1"},
		op:      opPull,
		replies: [][]string{reply("file {BIN} {BIN.size}", "{BIN.data}")},
		want:    expands("BIN"),
		fc:      true,
	}, {
		// BIN's payload does not end in LF: the next card line starts
		// straight after it.
		Case: Case{ID: "sync.5.1.adjacent-frames", Req: "SP-5.1-1"},
		op:   opPull,
		replies: [][]string{reply("file {BIN} {BIN.size}", "{BIN.data}",
			"file {N1} {N1.size}", "{N1.data}")},
		want: every(stored("BIN"), stored("N1")),
		fc:   true,
	}, {
		// CL's content is a card line: it is payload, not a card.
		Case:    Case{ID: "sync.5.1.card-like-payload", Req: "SP-5.1-1"},
		op:      opPull,
		replies: [][]string{reply("file {CL} {CL.size}", "{CL.data}")},
		want:    every(expands("CL"), neverSent("gimme {U}")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.5.1.negative-size", Req: "SP-5.1-1"},
		op:      opPull,
		replies: [][]string{reply("file {N1} -1", "igot {U}")},
		want:    every(notStored("N1"), neverSent("gimme {U}")),
		fc:      true,
	}}
}

// clientCardCases are Sections 5.2 and 5.3: card syntax on the client.
func clientCardCases() []syncClientCase {
	stored1 := []string{"file {N1} {N1.size}", "{N1.data}"}
	prefix := func(bad string) []string { return reply(append(stored1, bad, "igot {U}")...) }
	return []syncClientCase{{
		Case:    Case{ID: "sync.5.2.canonical-request", Req: "SP-5.2-1"},
		op:      clientOp{pull: true, push: true, login: true},
		setup:   quiet,
		replies: [][]string{reply("gimme {A1}", "igot {U}")},
		want:    every(atLeast(2), eachCardSent(canonical)),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.5.2.whitespace-runs", Req: "SP-5.2-2"},
		op:      opPull,
		replies: [][]string{reply("igot\t{U}", "igot  {U2}\r")},
		want:    every(sent(2, "gimme {U}"), sent(2, "gimme {U2}")),
		fc:      true,
	}, {
		// A no-break space is not a token separator: the message has one
		// argument, and the igot after it is read.
		Case:    Case{ID: "sync.5.2.non-ascii-space", Req: "SP-5.2-2"},
		op:      opPull,
		replies: [][]string{reply("message a b", "igot {U}")},
		want:    sent(2, "gimme {U}"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.5.2.error-decoding", Req: "SP-5.2-3"},
		op:      opPull,
		replies: [][]string{reply(`error a\sb\\c\nd`)},
		want:    reported("a b\\c\nd"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.5.2.unknown-prefix", Req: "SP-5.2-5"},
		op:      opPull,
		replies: [][]string{prefix("bogus card")},
		want: every(stored("N1"), notStored("U"), neverSent("gimme {U}"),
			reported("bogus card")),
		fc: true,
	}, {
		Case:    Case{ID: "sync.5.2.malformed-prefix", Req: "SP-5.2-8"},
		op:      opPull,
		replies: [][]string{prefix("gimme")},
		want: every(stored("N1"), notStored("U"), neverSent("gimme {U}"),
			reported("gimme")),
		fc: true,
	}, {
		Case:    Case{ID: "sync.5.2.private-surplus", Req: "SP-5.2-9"},
		op:      clientOp{pull: true, private: true},
		replies: [][]string{reply(append([]string{"private x"}, stored1...)...)},
		want:    every(stored("N1"), private("N1", 1)),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.5.2.igot-surplus", Req: "SP-5.2-10"},
		op:      opPull,
		replies: [][]string{reply("igot {U} 0 extra")},
		want:    every(sent(2, "gimme {U}"), private("U", 0)),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.5.2.long-line", Req: "SP-5.2-11"},
		op:      opPull,
		replies: [][]string{reply("message {LONG}", "igot {U}")},
		want:    sent(2, "gimme {U}"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.5.3.comment-not-card-client", Req: "SP-5.3-1"},
		op:      opPull,
		replies: [][]string{reply("#igot {U}")},
		want:    every(rounds(1), notStored("U")),
		fc:      true,
	}}
}

// loginForm is Section 6.1's login card, USER fossilized.
var loginForm = regexp.MustCompile(`^login al\\sice [0-9a-f]{40} [0-9a-f]{40}$`)

// clientAuthCases are Sections 6.1 to 6.5 on the client.
func clientAuthCases() []syncClientCase {
	login := clientOp{pull: true, login: true}
	return []syncClientCase{{
		Case:    Case{ID: "sync.6.1.login-form", Req: "SP-6.1-1"},
		op:      clientOp{pull: true, login: true, user: "al ice"},
		replies: [][]string{reply()},
		want: func(r *clientRun) error {
			req, err := r.request(1)
			if err != nil {
				return err
			}
			if !loginForm.MatchString(req.cards[0].line) {
				return expectf("%s: first card %q", r.who, req.cards[0].line)
			}
			return nil
		},
	}, {
		Case:    Case{ID: "sync.6.2.nonce-covers-body", Req: "SP-6.2-1"},
		op:      login,
		replies: [][]string{reply()},
		want: func(r *clientRun) error {
			fields, tail, err := r.loginCard(1)
			if err != nil {
				return err
			}
			if want := sha1Hex(tail); fields[2] != want {
				return expectf("%s: nonce %s, SHA-1 of the body after it %s",
					r.who, fields[2], want)
			}
			return nil
		},
		fc: true,
	}, {
		// Appendix A.5's user, password and project code, which is F's:
		// its shared secret is the one the appendix gives.
		Case:    Case{ID: "sync.6.3.shared-secret", Req: "SP-6.3-1"},
		op:      clientOp{pull: true, login: true, password: "secret-passphrase"},
		replies: [][]string{reply()},
		want:    signedWith("d7a235115bf97d126c42140196c4fc6087312353"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.6.4.signature", Req: "SP-6.4-1"},
		op:      login,
		replies: [][]string{reply()},
		want:    signedWith(sharedSecret(syncProjectCode, "alice", "secret")),
		fc:      true,
	}}
}
