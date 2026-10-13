package conformance

// clientFileCases are Section 7.1 and the integrity check of 7.7.
func clientFileCases() []syncClientCase {
	return []syncClientCase{{
		Case:    Case{ID: "sync.7.1.file-text", Req: "SP-7.1-1"},
		op:      opPull,
		replies: [][]string{reply("file {N1} {N1.size}", "{N1.data}")},
		want:    expands("N1"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.7.1.file-empty", Req: "SP-7.1-1"},
		op:      opPull,
		replies: [][]string{reply("file {E} 0")},
		want:    expands("E"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.7.1.file-sha3", Req: "SP-7.1-1"},
		op:      opPull,
		replies: [][]string{reply("file {N3} {N3.size}", "{N3.data}")},
		want:    expands("N3"),
		fc:      true,
	}, {
		// The payload is not N1's content. The LF after it is an empty
		// line.
		Case:    Case{ID: "sync.7.7.hash-mismatch", Req: "SP-7.7-4"},
		op:      opPull,
		replies: [][]string{reply("file {N1} 3", "abc")},
		want:    every(notStored("N1"), mentions("mismatch", "wrong hash")),
		fc:      true,
	}}
}

// clientCFileCases are Section 7.2 and the cfile padding of 5.1, in pull
// replies: a client receives cfile in any reply, and a pull keeps the
// repository for the checks, where a clone that fails removes it.
func clientCFileCases() []syncClientCase {
	cfile := func(key string) []string {
		return []string{"cfile {" + key + "} {" + key + ".size} {" + key + ".zsize}",
			"{" + key + ".z}"}
	}
	join := func(parts ...[]string) []string {
		var all []string
		for _, p := range parts {
			all = append(all, p...)
		}
		return reply(all...)
	}
	return []syncClientCase{{
		Case: Case{ID: "sync.5.1.cfile-padding-received", Req: "SP-5.1-2"},
		op:   opPull,
		replies: [][]string{join(cfile("N1"), []string{"{N1.pad}"}, cfile("N3"),
			[]string{"{N3.pad}"})},
		want: every(expands("N1"), expands("N3")),
		fc:   true,
	}, {
		Case:    Case{ID: "sync.7.2.cfile-full-adjacent", Req: "SP-7.2-1"},
		op:      opPull,
		replies: [][]string{join(cfile("N1"), cfile("N3"))},
		want:    every(expands("N1"), expands("N3")),
		fc:      true,
	}, {
		Case: Case{ID: "sync.7.2.cfile-delta", Req: "SP-7.2-1"},
		op:   opPull,
		replies: [][]string{join(cfile("S"),
			[]string{"cfile {T} {S} {T.size} {DTS.zsize}", "{DTS.z}"})},
		want: expands("T"),
		fc:   true,
	}, {
		Case: Case{ID: "sync.7.2.cfile-usize-metadata", Req: "SP-7.2-2"},
		op:   opPull,
		replies: [][]string{reply("cfile {N1} {N1.size1} {N1.zsize}", "{N1.z}",
			"file {N3} {N3.size}", "{N3.data}")},
		want: every(expands("N1"), expands("N3")),
		fc:   true,
	}, {
		// CSIZE counts four bytes after the zlib stream; the next card
		// starts after them.
		Case: Case{ID: "sync.7.2.cfile-trailing-stream", Req: "SP-7.2-1"},
		op:   opPull,
		replies: [][]string{join([]string{"cfile {N1} {N1.size} {N1J.zsize}", "{N1J.z}"},
			cfile("N3"))},
		want: every(expands("N1"), expands("N3")),
		fc:   true,
	}}
}

// clientUVFileCases are Section 7.3 on the client. A client that only
// pulls unversioned files has no fossil(1) run: fossil pull takes no -u.
func clientUVFileCases() []syncClientCase {
	uvPull := clientOp{pull: true, uv: true}
	return []syncClientCase{{
		// FLAGS 1 suppresses the payload: the five bytes SIZE claims are
		// not read, and the igot after the card is.
		Case:    Case{ID: "sync.7.3.uvfile-no-payload", Req: "SP-7.3-1"},
		op:      uvPull,
		replies: [][]string{reply("uvfile x.txt {UVT} - 5 1", "igot {U}")},
		want:    sent(2, "gimme {U}"),
	}, {
		// NAME is fossilized: the row is named with the space restored.
		Case:    Case{ID: "sync.7.3.uvfile-name-client", Req: "SP-7.3-1"},
		op:      uvPull,
		replies: [][]string{reply(`uvfile a\sb.txt {UVT} {N1} {N1.size} 0`, "{N1.data}")},
		want: clientRows("SELECT count(*) FROM unversioned WHERE name='a b.txt'"+
			" AND hash='{N1}'", 1),
	}, {
		// Accepted, the card would delete u.txt, being newer.
		Case:    Case{ID: "sync.7.3.flags-negative-client", Req: "SP-7.3-2"},
		op:      uvPull,
		replies: [][]string{reply("uvfile u.txt {UVT1} - 0 -1")},
		want: clientRows(
			"SELECT count(*) FROM unversioned WHERE name='u.txt' AND hash='{UVH}'", 1),
	}, {
		Case:    Case{ID: "sync.7.3.status2-mtime-client", Req: "SP-7.3-5"},
		op:      uvPull,
		replies: [][]string{reply("uvfile u.txt {UVT1} {UVH} 11 4")},
		want: clientRows("SELECT count(*) FROM unversioned WHERE name='u.txt' AND mtime={UVT1}"+
			" AND hash='{UVH}' AND sz=11", 1),
	}, {
		// t.txt is deleted here, later than the peer's copy: the client
		// sends the deletion.
		Case: Case{ID: "sync.7.3.deletion-form", Req: "SP-7.3-3"},
		op:   clientOp{pull: true, push: true, uv: true},
		setup: append(append([]string{}, quiet...),
			"INSERT INTO unversioned(name, rcvid, mtime, hash, sz, encoding, content)"+
				" VALUES('t.txt', 1, {UVT1}, NULL, 0, 0, NULL)",
			"DELETE FROM config WHERE name='uv-hash'"),
		replies: [][]string{reply("pragma uv-push-ok", "uvigot t.txt {UVT} {N1} {N1.size}")},
		want:    sent(2, "uvfile t.txt {UVT1} - 0 1"),
		fc:      true,
	}}
}

// clientDeltaCases are Section 7.4.
func clientDeltaCases() []syncClientCase {
	return []syncClientCase{{
		// A2 is held, so the delta is consumed and ignored: A2 stays
		// stored in full. Whether a phantom is made for the delta's basis
		// is the receiver's choice (Section 7.4); fossil(1) 2.28 makes one.
		Case:    Case{ID: "sync.7.4.verified-duplicate", Req: "SP-7.4-1"},
		op:      opPull,
		replies: [][]string{reply("file {A2} {U} {DA2U.size}", "{DA2U.data}")},
		want: every(expands("A2"),
			clientRows("SELECT count(*) FROM delta JOIN blob USING(rid) WHERE uuid='{A2}'", 0)),
		fc: true,
	}, {
		Case:    Case{ID: "sync.7.4.delta-held-basis", Req: "SP-7.4-2"},
		op:      opPull,
		replies: [][]string{reply("file {T} {A1} {DTA1.size}", "{DTA1.data}")},
		want:    expands("T"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.7.4.private-basis-phantom", Req: "SP-7.4-2"},
		op:      clientOp{pull: true, private: true},
		replies: [][]string{reply("private", "file {T} {U} {DTS.size}", "{DTS.data}")},
		want:    every(phantom("U"), private("U", 1)),
		fc:      true,
	}}
}

// clientPrivateCases are Section 7.5 on the client.
func clientPrivateCases() []syncClientCase {
	priv := clientOp{pull: true, private: true}
	return []syncClientCase{{
		Case: Case{ID: "sync.7.5.private-once", Req: "SP-7.5-3"},
		op:   priv,
		replies: [][]string{reply("private", "file {N1} {N1.size}", "{N1.data}",
			"file {N3} {N3.size}", "{N3.data}")},
		want: every(private("N1", 1), stored("N3"), private("N3", 0)),
		fc:   true,
	}, {
		Case: Case{ID: "sync.7.5.marker-survives", Req: "SP-7.5-1"},
		op:   priv,
		replies: [][]string{reply("igot {N1}", "private"),
			reply("file {N1} {N1.size}", "{N1.data}")},
		want: every(stored("N1"), private("N1", 1)),
		fc:   true,
	}, {
		Case:  Case{ID: "sync.7.5.marker-skips-uvfile", Req: "SP-7.5-1"},
		op:    clientOp{pull: true, push: true, private: true, uv: true},
		setup: quiet,
		replies: [][]string{reply("private", "uvfile x.txt {UVT} - 0 1", "# c", "private",
			"file {N1} {N1.size}", "{N1.data}")},
		want: every(stored("N1"), private("N1", 1)),
		fc:   true,
	}, {
		Case:    Case{ID: "sync.7.5.unauthorized-private-igot", Req: "SP-7.5-5"},
		op:      opPull,
		replies: [][]string{reply("igot {A1} 1", "igot {U} 1")},
		want:    every(rounds(1), private("A1", 1), notStored("U")),
		fc:      true,
	}, {
		// P is private. The first request goes before any server date is
		// known, and the reply dates the server before 2020-04-13, which
		// would withhold igot P 1 outside a private sync: neither stops the
		// listing. The gimme for B1 makes the second request.
		Case:  Case{ID: "sync.8.6.private-inventory", Req: "SP-8.6-1"},
		op:    clientOp{push: true, private: true},
		setup: quiet,
		replies: [][]string{{"pragma server-version 22800 20200101 000000", "gimme {B1}",
			"{TS}"}},
		want: every(rounds(2), sent(1, "igot {P} 1"), sent(2, "igot {P} 1"),
			sent(2, "file {B1} {B1.size}")),
		fc: true,
	}}
}
