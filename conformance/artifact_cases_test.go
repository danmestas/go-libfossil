package conformance

import (
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// The hand-written artifact cases, by draft section. IDs match libfossil's
// suite wherever it has the same case.

// Signature wrappers (Section 4.1.1). The signature bytes are opaque.
const (
	pgpHead = "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n"
	pgpSig  = "-----BEGIN PGP SIGNATURE-----\n\nc2lnbmF0dXJl\n-----END PGP SIGNATURE-----\n"
	sshHead = "-----BEGIN SSH SIGNED MESSAGE-----\nHash: SHA256\n\n"
	sshSig  = "-----BEGIN SSH SIGNATURE-----\nc2lnbmF0dXJl\n-----END SSH SIGNATURE-----\n"
)

// signedControl is the control base with its Z card, as a signed artifact
// wraps it.
var signedControl = string(withZ(baseControl))

// raw builds a case whose input is used as given, without a Z card added.
func raw(id, req, input string, accept bool) artifactCase {
	return artifactCase{Case: Case{ID: id, Req: req}, input: []byte(input), accept: accept}
}

// says sets the substring fossil's rejection reason must contain.
func says(c artifactCase, reason string) artifactCase {
	c.fossilSays = reason
	return c
}

// zRejected builds a rejection the Z card itself is meant to cause, so the
// input need not pass deck.VerifyZ.
func zRejected(id, req, input string) artifactCase {
	c := raw(id, req, input, false)
	c.zCase = true
	return c
}

// signedCase builds an accepted signed input; Marshal writes only the
// inner content, so none of them re-encode.
func signedCase(id, req, input string) artifactCase {
	c := raw(id, req, input, true)
	c.nonCanonical = true
	c.check = wantType(deck.Control)
	return c
}

var framingCases = []artifactCase{
	raw("artifact.4.1.empty", "AF-4.1-1", "", false),
	raw("artifact.4.1.no-final-lf", "AF-4.1-1", strings.TrimSuffix(signedControl, "\n"), false),
	raw("artifact.4.1.1.outer-no-lf", "AF-4.1-1",
		pgpHead+signedControl+strings.TrimSuffix(pgpSig, "\n"), false),
	raw("artifact.4.1.short", "AF-4.1-2", "D 2026\n", false),
	rejected("artifact.4.1.lowercase", "AF-4.1-2", edit(baseManifest, "C base", "c base")),
	rejected("artifact.4.1.first-bare", "AF-4.1-2",
		edit(baseManifest, "C base\\smanifest\n", "C\n")),
	rejected("artifact.4.1.first-tab", "AF-4.1-2", edit(baseManifest, "C base", "C\tbase")),
	rejected("artifact.4.1.leading-space", "AF-4.1-2", " "+baseManifest),
	rejected("artifact.4.1.guard", "AF-4.1-2", "# inert fixture\n"+baseManifest),
	says(raw("artifact.4.2.residual", "AF-4.1-3", signedControl+"garbage\n", false),
		"extra characters at end of card"),
	// Section 4.1.2: the marker follows the final card, so the card loop
	// reads it as a '#' card and rejects it for card order, after the Z card.
	says(raw("artifact.4.1.2.sterilized", "AF-4.1.2-1", string(withZ(baseManifest))+
		"# Remove this line to create a well-formed Fossil manifest.\n", false),
		"cards not in lexicographical order"),
	says(raw("artifact.4.1.2.signed-guard", "AF-4.1.2-1", pgpHead+signedControl+
		"# Remove this line to create a well-formed Fossil control artifact.\n"+pgpSig, false),
		"cards not in lexicographical order"),
}

var signatureCases = []artifactCase{
	signedCase("artifact.4.1.1.pgp", "AF-4.1.1-1", pgpHead+signedControl+pgpSig),
	signedCase("artifact.4.1.1.ssh", "AF-4.1.1-2", sshHead+signedControl+sshSig),
	raw("artifact.4.1.1.leading-space", "AF-4.1.1-3", " "+pgpHead+signedControl+pgpSig, false),
	// One dash short: the first 34 bytes are no longer a header line.
	raw("artifact.4.1.1.header-short", "AF-4.1.1-3",
		"-----BEGIN PGP SIGNED MESSAGE----\n\n"+signedControl+pgpSig, false),
	signedCase("artifact.4.1.1.pgp-crlf", "AF-4.1.1-4",
		strings.Replace(pgpHead, "\n\n", "\n\r\n", 1)+signedControl+pgpSig),
	raw("artifact.4.1.1.missing-blank", "AF-4.1.1-4",
		strings.Replace(pgpHead, "\n\n", "\n", 1)+signedControl+pgpSig, false),
	signedCase("artifact.4.1.1.pgp-no-footer", "AF-4.1.1-5", pgpHead+signedControl),
	// The cut is at the last armor line: a W body holding one is content.
	{
		Case: Case{ID: "artifact.4.1.1.armor-in-body", Req: "AF-4.1.1-5"},
		input: []byte(pgpHead + string(withZ(edit(baseWiki, "W 12\nhello\nworld\n\n",
			"W 32\nx\n-----BEGIN PGP SIGNATURE-----\n\n"))) + pgpSig),
		accept: true, nonCanonical: true,
		check: func(d *deck.Deck) error {
			return want("W", string(d.W), "x\n-----BEGIN PGP SIGNATURE-----\n")
		},
	},
	signedCase("artifact.4.1.1.ssh-short", "AF-4.1.1-6",
		sshHead+signedControl+"-----BEGIN SSH SIGNATURE----x\nc2lnbmF0dXJl\n"),
	signedCase("artifact.4.1.1.pgp-short", "AF-4.1.1-6",
		pgpHead+signedControl+"-----BEGIN PGP SIGNATURE----x\nc2lnbmF0dXJl\n"),
	// The Z card covers the stripped content only, never the wrapper.
	zRejected("artifact.6.2.signed-z-outer", "AF-6.2-2",
		pgpHead+baseControl+zCard(pgpHead+baseControl)+pgpSig),
}

var grammarCases = []artifactCase{
	says(rejected("artifact.4.2.card-tab", "AF-4.2-1", edit(baseManifest, "D 2026", "D\t2026")),
		"extra characters at end of card"),
	says(rejected("artifact.4.2.card-cr", "AF-4.2-1", edit(baseManifest, "U alice", "U\ralice")),
		"extra characters at end of card"),
	loose("artifact.4.2.token-tab", "AF-4.2-2",
		edit(baseManifest, "C base\\smanifest", "C base\tmanifest"),
		func(d *deck.Deck) error { return want("C", d.C, "base\tmanifest") }),
	loose("artifact.4.2.token-cr", "AF-4.2-2",
		edit(baseManifest, "C base\\smanifest", "C base\rmanifest"),
		func(d *deck.Deck) error { return want("C", d.C, "base\rmanifest") }),
	loose("artifact.4.2.file-tab", "AF-4.2-2", edit(baseManifest, "F a.txt ", "F a\tb.txt "),
		func(d *deck.Deck) error {
			return want("F[0]", d.F[0].Name+" "+d.F[0].UUID, "a\tb.txt "+h1)
		}),
	rejected("artifact.4.2.hash-cr", "AF-4.2-2", edit(baseDelta, "B "+h1+"\n", "B "+h1+"\r\n")),
	loose("artifact.4.2.login-cr", "AF-4.2-2", edit(baseManifest, "U alice\n", "U alice\r\n"),
		func(d *deck.Deck) error { return want("U", *d.U, "alice\r") }),
	rejected("artifact.4.2.double-space", "AF-4.2-3",
		edit(baseManifest, "F a.txt "+h1, "F a.txt  "+h1)),
	// Section 4.2: a line ending in a space carries a present, empty last
	// token, which the C, J, N and T cards keep as an empty value.
	accepted("artifact.4.7.c-empty", "AF-4.7.4-2",
		edit(baseManifest, "C base\\smanifest\n", "C \n"),
		func(d *deck.Deck) error { return want("C", d.C, "") }),
	loose("artifact.4.7.j-empty-value", "AF-4.2-3",
		edit(baseTicket, "J status Open\n", "J status \n"),
		func(d *deck.Deck) error { return want("J[1]", d.J[1], deck.TicketField{Name: "status"}) }),
	accepted("artifact.4.7.n-empty", "AF-4.2-3", edit(baseWiki, "N text/x-markdown\n", "N \n"),
		func(d *deck.Deck) error { return want("N", d.N, "") }),
	loose("artifact.4.7.f-empty-permission", "AF-4.2-3",
		edit(baseManifest, "F a.txt "+h1+"\n", "F a.txt "+h1+" \n"),
		func(d *deck.Deck) error { return want("F[0].Perm", d.F[0].Perm, "") }),
	accepted("artifact.4.7.16.t-empty-value", "AF-4.7.16-4", edit(baseControl, " blue\n", " \n"),
		func(d *deck.Deck) error { return want("T[0].Value", d.T[0].Value, "") }),
	rejected("artifact.4.2.2.unknown-O", "AF-4.2.2-2",
		edit(baseManifest, "P "+h4, "O unknown\nP "+h4)),
	rejected("artifact.4.2.2.unknown-S", "AF-4.2.2-2",
		edit(baseManifest, "T *branch", "S unknown\nT *branch")),
	rejected("artifact.4.2.2.unknown-V", "AF-4.2.2-2", baseManifest+"V unknown\n"),
	rejected("artifact.4.2.2.unknown-X", "AF-4.2.2-2", baseManifest+"X unknown\n"),
	rejected("artifact.4.2.2.unknown-Y", "AF-4.2.2-2", baseManifest+"Y unknown\n"),
	rejected("artifact.4.2.2.unknown-a", "AF-4.2.2-1", baseManifest+"a unknown\n"),
	// '1' sorts below every letter, so its only ordered place is first,
	// where the line-1 gate is what rejects it.
	says(rejected("artifact.4.2.2.unknown-1", "AF-4.1-2", "1 unknown\n"+baseManifest),
		"line 1 not recognized"),
	// 0x7F sorts above U whether bytes are compared signed or unsigned;
	// fossil compares them signed, so a byte from 0x80 up sorts first.
	rejected("artifact.4.2.2.unknown-del", "AF-4.2.2-1", baseManifest+"\x7f unknown\n"),
	// An empty line is a card whose letter is LF.
	// fossil reports the empty line against the card before it.
	says(rejected("artifact.4.2.2.blank-line", "AF-4.2.2-1",
		edit(baseManifest, "U alice\n", "\nU alice\n")),
		"extra characters at end of card"),
}

// cardIs checks one string field against its decoded value.
func cardIs(field string, get func(*deck.Deck) string, value string) func(*deck.Deck) error {
	return func(d *deck.Deck) error { return want(field, get(d), value) }
}

var escapeCases = []artifactCase{
	accepted("artifact.4.3.escape-pairs", "AF-4.3-1",
		edit(baseManifest, "C base\\smanifest", `C a\sb\nc\td\re\vf\fg\\h\0i`),
		cardIs("C", func(d *deck.Deck) string { return d.C }, "a b\nc\td\re\vf\fg\\h\x00i")),
	loose("artifact.4.3.unknown-escape", "AF-4.3-2",
		edit(baseManifest, "C base\\smanifest", `C a\qb`),
		cardIs("C", func(d *deck.Deck) string { return d.C }, "aqb")),
	loose("artifact.4.3.trailing-backslash", "AF-4.3-2",
		edit(baseManifest, "C base\\smanifest", `C trail\`),
		cardIs("C", func(d *deck.Deck) string { return d.C }, `trail\`)),
	accepted("artifact.4.3.1.a-filename", "AF-4.3.1-1",
		edit(baseAttachment, "A report.txt", `A my\sreport.txt`),
		cardIs("A.Filename", func(d *deck.Deck) string { return d.A.Filename }, "my report.txt")),
	accepted("artifact.4.3.1.a-target", "AF-4.3.1-1",
		edit(baseAttachment, "A report.txt "+h1, `A report.txt Home\sPage`),
		cardIs("A.Target", func(d *deck.Deck) string { return d.A.Target }, "Home Page")),
	accepted("artifact.4.3.1.c-comment", "AF-4.3.1-1", baseManifest,
		cardIs("C", func(d *deck.Deck) string { return d.C }, "base manifest")),
	accepted("artifact.4.3.1.f-filename", "AF-4.3.1-1",
		edit(baseManifest, "F a.txt ", `F a\sz.txt `),
		cardIs("F[0].Name", func(d *deck.Deck) string { return d.F[0].Name }, "a z.txt")),
	accepted("artifact.4.3.1.f-old-name", "AF-4.3.1-1",
		edit(baseManifest, " w old.txt", ` w old\sname.txt`),
		cardIs("F[2].OldName", func(d *deck.Deck) string { return d.F[2].OldName },
			"old name.txt")),
	accepted("artifact.4.3.1.h-title", "AF-4.3.1-1",
		edit(baseForum, "I "+h2+"\n", `H a\stitle`+"\n"),
		cardIs("H", func(d *deck.Deck) string { return d.H }, "a title")),
	accepted("artifact.4.3.1.j-value", "AF-4.3.1-1", baseTicket,
		cardIs("J[0].Value", func(d *deck.Deck) string { return d.J[0].Value }, "more text")),
	accepted("artifact.4.3.1.l-title", "AF-4.3.1-1", baseWiki,
		cardIs("L", func(d *deck.Deck) string { return d.L }, "Home Page")),
	accepted("artifact.4.3.1.n-mimetype", "AF-4.3.1-1",
		edit(baseWiki, "N text/x-markdown", `N text/x\smarkdown`),
		cardIs("N", func(d *deck.Deck) string { return d.N }, "text/x markdown")),
	accepted("artifact.4.3.1.t-name", "AF-4.3.1-1",
		edit(baseManifest, "T +sym-trunk *", `T +sym-my\strunk *`),
		cardIs("T[1].Name", func(d *deck.Deck) string { return d.T[1].Name }, "sym-my trunk")),
	accepted("artifact.4.3.1.t-value", "AF-4.3.1-1",
		edit(baseManifest, "T *branch * trunk", `T *branch * my\strunk`),
		cardIs("T[0].Value", func(d *deck.Deck) string { return d.T[0].Value }, "my trunk")),
	accepted("artifact.4.3.1.u-login", "AF-4.3.1-1", edit(baseManifest, "U alice", `U al\sice`),
		cardIs("U", func(d *deck.Deck) string { return *d.U }, "al ice")),
	accepted("artifact.4.3.1.j-name-raw", "AF-4.3.1-2",
		edit(baseTicket, "J status Open", `J sta\stus Open`),
		cardIs("J[1].Name", func(d *deck.Deck) string { return d.J[1].Name }, `sta\stus`)),
	accepted("artifact.4.3.1.w-body-raw", "AF-4.3.1-2",
		edit(baseWiki, "W 12\nhello\nworld\n\n", "W 13\nhello\\sworld\n\n"),
		cardIs("W", func(d *deck.Deck) string { return string(d.W) }, "hello\\sworld\n")),
	rejected("artifact.4.3.1.d-raw", "AF-4.3.1-2",
		edit(baseManifest, "D 2026-01-02T", `D 2026-01-02\T`)),
	rejected("artifact.4.3.1.hash-raw", "AF-4.3.1-2",
		edit(baseManifest, "F a.txt "+h1, `F a.txt 0\`+h1[1:])),
	// \1 is no escape, so the token decodes to h1 and is checked as a hash.
	loose("artifact.4.3.1.a-target-escaped-hash", "AF-4.3.1-3",
		edit(baseAttachment, "A report.txt "+h1, `A report.txt 0\`+h1[1:]),
		cardIs("A.Target", func(d *deck.Deck) string { return d.A.Target }, h1)),
}

var orderCases = []artifactCase{
	rejected("artifact.4.4.descending", "AF-4.4-1", edit(baseManifest,
		"C base\\smanifest\nD "+ts+"\n", "D "+ts+"\nC base\\smanifest\n")),
	loose("artifact.4.4.event-n-after-p", "AF-4.4-2", edit(baseEvent,
		"N text/x-markdown\nP "+h2+"\n", "P "+h2+"\nN text/x-markdown\n"),
		cardIs("N", func(d *deck.Deck) string { return d.N }, "text/x-markdown")),
	rejected("artifact.4.4.non-event-n-after-p", "AF-4.4-2", edit(baseWiki,
		"N text/x-markdown\nP "+h1+"\n", "P "+h1+"\nN text/x-markdown\n")),
	// Appendix C: receivers differ on a repeated P card and the draft does
	// not settle it, so fossil(1) decides: it accepts and concatenates.
	loose("artifact.4.5.1.p-repeat", "AF-4.5.1-2",
		edit(baseManifest, "P "+h4+" "+h5+"\n", "P "+h4+"\nP "+h5+"\n"),
		func(d *deck.Deck) error { return want("P", d.P, []string{h4, h5}) }),
	loose("artifact.4.5.z-repeat", "AF-4.5.1-3", baseManifest+"Z "+md5Token+"\n", nil),
	accepted("artifact.4.5.2.F-ordered", "AF-4.5.2-1", baseManifest,
		func(d *deck.Deck) error { return want("len(F)", len(d.F), 3) }),
	accepted("artifact.4.5.2.J-ordered", "AF-4.5.2-2", baseTicket,
		func(d *deck.Deck) error { return want("len(J)", len(d.J), 2) }),
	accepted("artifact.4.5.2.M-ordered", "AF-4.5.2-3", baseCluster,
		func(d *deck.Deck) error { return want("M", d.M, []string{h1, h2}) }),
	rejected("artifact.4.5.2.F-descending", "AF-4.5.2-1", edit(baseManifest,
		"F a.txt "+h1+"\nF b/c.txt "+h2+" x\n", "F b/c.txt "+h2+" x\nF a.txt "+h1+"\n")),
	rejected("artifact.4.5.2.F-duplicate", "AF-4.5.2-1",
		edit(baseManifest, "F b/c.txt", "F a.txt")),
	// Raw order and decoded order disagree here: "a\sz.txt" sorts after
	// "a.txt" on the wire but before it once \s is a space.
	accepted("artifact.4.5.2.decoded-file-order", "AF-4.5.2-1",
		edit(baseManifest, "F a.txt "+h1+"\n", `F a\sz.txt `+h8+"\nF a.txt "+h1+"\n"),
		cardIs("F[0].Name", func(d *deck.Deck) string { return d.F[0].Name }, "a z.txt")),
	rejected("artifact.4.5.2.decoded-file-descending", "AF-4.5.2-1",
		edit(baseManifest, "F a.txt "+h1+"\n", "F a.txt "+h1+"\n"+`F a\sz.txt `+h8+"\n")),
	rejected("artifact.4.5.2.J-descending", "AF-4.5.2-2", edit(baseTicket,
		"J +comment more\\stext\nJ status Open\n", "J status Open\nJ +comment more\\stext\n")),
	rejected("artifact.4.5.2.J-duplicate", "AF-4.5.2-2",
		edit(baseTicket, "J status Open", "J +comment Open")),
	rejected("artifact.4.5.2.M-descending", "AF-4.5.2-3",
		edit(baseCluster, baseCluster, "M "+h2+"\nM "+h1+"\n")),
	rejected("artifact.4.5.2.M-duplicate", "AF-4.5.2-3", edit(baseCluster, "M "+h2, "M "+h1)),
	rejected("artifact.4.5.2.t-equal", "AF-4.5.2-4",
		edit(baseControl, "T -sym-old "+h2+"\n", "T +bgcolor "+h1+" blue\n")),
	accepted("artifact.4.5.2.t-target-order", "AF-4.5.2-4",
		edit(baseControl, "T -sym-old "+h2, "T +bgcolor "+h2), nil),
	rejected("artifact.4.5.2.t-target-descending", "AF-4.5.2-4", edit(baseControl,
		"T +bgcolor "+h1+" blue\n", "T +bgcolor "+h2+" blue\nT +bgcolor "+h1+"\n")),
	accepted("artifact.4.5.2.t-sign-order", "AF-4.5.2-4", edit(baseControl,
		"T +bgcolor "+h1+" blue\n", "T *bgcolor "+h1+" blue\nT +bgcolor "+h1+" blue\n"), nil),
	rejected("artifact.4.5.2.t-sign-descending", "AF-4.5.2-4", edit(baseControl,
		"T +bgcolor "+h1+" blue\n", "T +bgcolor "+h1+" blue\nT *bgcolor "+h1+" blue\n")),
	accepted("artifact.4.5.2.p-order-kept", "AF-4.5.2-5",
		edit(baseManifest, "P "+h4+" "+h5, "P "+h5+" "+h4+" "+h5),
		func(d *deck.Deck) error { return want("P", d.P, []string{h5, h4, h5}) }),
	accepted("artifact.4.5.2.q-repeat", "AF-4.5.2-6",
		edit(baseManifest, "Q +"+h6+" "+h7+"\n", "Q +"+h6+" "+h7+"\nQ +"+h6+" "+h7+"\n"),
		func(d *deck.Deck) error { return want("len(Q)", len(d.Q), 2) }),
	accepted("artifact.4.5.2.q-unordered", "AF-4.5.2-6",
		edit(baseManifest, "Q +"+h6+" "+h7+"\n", "Q +"+h6+" "+h7+"\nQ -"+h1+"\n"), nil),
}

// pathnameCases puts each name in the first F card of the manifest base.
// Every name sorts before the base's second file, b/c.txt.
func pathnameCases() []artifactCase {
	bad := []struct{ slug, name string }{
		{"leading-slash", "/a.txt"}, {"dot", "."}, {"dotdot", ".."},
		{"dot-prefix", "./a.txt"}, {"dotdot-prefix", "../a.txt"},
		{"double-slash", "a//c.txt"}, {"trailing-slash", "a/"},
		{"dot-element", "a/./c.txt"}, {"dotdot-element", "a/../c.txt"},
		{"dot-tail", "a/."}, {"dotdot-tail", "a/.."},
	}
	good := []struct{ slug, wire, decoded string }{
		{"backslash", `a\\b.txt`, `a\b.txt`}, {"space", `a\sb.txt`, "a b.txt"},
		{"control-byte", "a\x01b.txt", "a\x01b.txt"}, {"invalid-utf8", "a\xffb.txt", "a\xffb.txt"},
		{"dotfile", ".a.txt", ".a.txt"}, {"inner-dots", "a..b.txt", "a..b.txt"},
		{"dots-prefix", "..a.txt", "..a.txt"}, {"dot-dir", "a/.c.txt", "a/.c.txt"},
	}
	var cases []artifactCase
	for _, b := range bad {
		cases = append(cases, rejected("artifact.4.6.1.f-"+b.slug, "AF-4.6.1-1",
			edit(baseManifest, "F a.txt ", "F "+b.name+" ")))
	}
	for _, g := range good {
		cases = append(cases, accepted("artifact.4.6.1.f-"+g.slug, "AF-4.6.1-2",
			edit(baseManifest, "F a.txt ", "F "+g.wire+" "),
			cardIs("F[0].Name", func(d *deck.Deck) string { return d.F[0].Name }, g.decoded)))
	}
	return append(cases,
		rejected("artifact.4.6.1.old-name-leading-slash", "AF-4.6.1-1",
			edit(baseManifest, " w old.txt", " w /old.txt")),
		rejected("artifact.4.6.1.old-name-dotdot-prefix", "AF-4.6.1-1",
			edit(baseManifest, " w old.txt", " w ../old.txt")),
		rejected("artifact.4.6.1.a-leading-slash", "AF-4.6.1-1",
			edit(baseAttachment, "A report.txt", "A /report.txt")),
		rejected("artifact.4.6.1.a-double-slash", "AF-4.6.1-1",
			edit(baseAttachment, "A report.txt", "A a//report.txt")),
	)
}

// wikiNameCases puts each name in the wiki base's L card.
func wikiNameCases() []artifactCase {
	long := strings.Repeat("x", 100)
	bad := []struct{ slug, name string }{
		{"leading-space", `\sHome`}, {"leading-tab", `\tHome`}, {"inner-tab", `Ho\tme`},
		{"inner-lf", `Ho\nme`}, {"double-space", `Home\s\sPage`}, {"trailing-space", `Home\s`},
		{"101-bytes", long + "x"},
	}
	good := []struct{ slug, name string }{
		{"one-byte", "x"}, {"100-bytes", long}, {"del-byte", "Home\x7f"},
		{"invalid-utf8", "Home\xff\xfe"},
	}
	var cases []artifactCase
	for _, b := range bad {
		cases = append(cases, rejected("artifact.4.6.2.l-"+b.slug, "AF-4.6.2-1",
			edit(baseWiki, `L Home\sPage`, "L "+b.name)))
	}
	for _, g := range good {
		cases = append(cases, accepted("artifact.4.6.2.l-"+g.slug, "AF-4.6.2-2",
			edit(baseWiki, `L Home\sPage`, "L "+g.name),
			cardIs("L", func(d *deck.Deck) string { return d.L }, g.name)))
	}
	return cases
}

// fileNames checks the effective F card list after any reserved-name drop.
func fileNames(names ...string) func(*deck.Deck) error {
	return func(d *deck.Deck) error {
		var got []string
		for _, f := range d.F {
			got = append(got, f.Name)
		}
		return want("F names", got, names)
	}
}

var reservedCases = func() []artifactCase {
	var cases []artifactCase
	// Every name sorts before b/c.txt, so it can replace a.txt in place.
	for _, name := range []string{"_FOSSIL_", ".fslckout", "a/_FOSSIL_", "a_FOSSIL_-wal",
		"a/.fslckout-journal", "_fossil_", ".FSLCKOUT-SHM"} {
		cases = append(cases, loose("artifact.4.6.3.drop-"+name, "AF-4.6.3-1",
			edit(baseManifest, "F a.txt ", "F "+name+" "), fileNames("b/c.txt", "d.txt")))
	}
	for _, name := range []string{"a_FOSSIL_", "a.fslckout", "_FOSSIL_-wal-wal"} {
		cases = append(cases, accepted("artifact.4.6.3.keep-"+name, "AF-4.6.3-2",
			edit(baseManifest, "F a.txt ", "F "+name+" "), fileNames(name, "b/c.txt", "d.txt")))
	}
	return append(cases,
		accepted("artifact.4.6.3.old-name-not-tested", "AF-4.6.3-2",
			edit(baseManifest, " w old.txt", " w _FOSSIL_"),
			cardIs("F[2].OldName", func(d *deck.Deck) string { return d.F[2].OldName },
				"_FOSSIL_")),
		rejected("artifact.4.6.3.order-before-drop", "AF-4.6.3-3", edit(baseManifest,
			"F d.txt "+h3+" w old.txt\n", "F d.txt "+h3+" w old.txt\nF _FOSSIL_ "+h8+"\n")),
		// c/_FOSSIL_ is dropped, so b/c.txt is compared with a.txt, not
		// with the dropped name it sorts below.
		loose("artifact.4.6.3.drop-not-anchor", "AF-4.6.3-3",
			edit(baseManifest, "F b/c.txt", "F c/_FOSSIL_ "+h8+"\nF b/c.txt"),
			fileNames("a.txt", "b/c.txt", "d.txt")),
		// The C card makes it a manifest; the dropped F card adds no file.
		loose("artifact.4.6.3.drop-only-file", "AF-4.6.3-4",
			"C only\\sreserved\nD "+ts+"\nF _FOSSIL_ "+h1+"\nU alice\n",
			func(d *deck.Deck) error {
				if err := wantType(deck.Checkin)(d); err != nil {
					return err
				}
				return want("F count", len(d.F), 0)
			}),
		// No C or P card: the dropped F card sets no type, so the artifact
		// is a control artifact, which may not carry an F card.
		rejected("artifact.5.3.dropped-f-control", "AF-5.3-3",
			"D "+ts+"\nF _FOSSIL_ "+h1+"\nU alice\n"),
	)
}()

// timestampCases puts each token in the manifest base's D card. An
// accepted token is checked as the instant it denotes, in UTC.
func timestampCases() []artifactCase {
	good := []struct{ slug, token, instant string }{
		{"hh-mm", "2026-01-02T03:04", "2026-01-02T03:04:00.000"},
		{"one-digit-fraction", "2026-01-02T03:04:05.1", "2026-01-02T03:04:05.100"},
		{"long-fraction", "2026-01-02T03:04:05.123456", "2026-01-02T03:04:05.123"},
		{"zone-z", "2026-01-02T03:04:05Z", "2026-01-02T03:04:05.000"},
		{"zone-lowercase-z", "2026-01-02T03:04:05z", "2026-01-02T03:04:05.000"},
		{"zone-plus", "2026-01-02T03:04:05+01:00", "2026-01-02T02:04:05.000"},
		{"zone-minus", "2026-01-02T03:04:05-01:30", "2026-01-02T04:34:05.000"},
		{"tab-separator", "2026-01-02\t03:04:05", "2026-01-02T03:04:05.000"},
		{"double-separator", "2026-01-02TT03:04:05", "2026-01-02T03:04:05.000"},
		{"hour-24", "2026-01-02T24:00:00", "2026-01-03T00:00:00.000"},
		{"no-calendar-check", "2024-02-31T03:04:05", "2024-03-02T03:04:05.000"},
		{"after-epoch", "-4713-11-25T00:00:00", "-4713-11-25T00:00:00.000"},
	}
	// Julian day numbers (Section 4.6.4, form 5); 2461042.5 is
	// 2026-01-02T00:00:00.
	julian := []struct{ slug, token, instant string }{
		{"julian-fraction", "2461042.5", "2026-01-02T00:00:00.000"},
		{"julian-plus-sign", "+2461042.5", "2026-01-02T00:00:00.000"},
		{"julian-exponent", "2.4610425e6", "2026-01-02T00:00:00.000"},
		{"julian-exponent-upper", "24610425E-1", "2026-01-02T00:00:00.000"},
		{"julian-leading-point", ".5", "-4713-11-25T00:00:00.000"},
		{"julian-trailing-point", "2461042.", "2026-01-01T12:00:00.000"},
	}
	bad := []struct{ slug, req, token string }{
		{"lowercase-t", "AF-4.6.4-3", "2026-01-02t03:04:05"},
		{"invalid-month", "AF-4.6.4-4", "2026-13-02T03:04:05"},
		{"month-00", "AF-4.6.4-4", "2026-00-02T03:04:05"},
		{"day-32", "AF-4.6.4-4", "2026-01-32T03:04:05"},
		{"day-00", "AF-4.6.4-4", "2026-01-00T03:04:05"},
		{"hour-25", "AF-4.6.4-4", "2026-01-02T25:04:05"},
		{"minute-60", "AF-4.6.4-4", "2026-01-02T03:60:05"},
		{"second-60", "AF-4.6.4-4", "2026-01-02T03:04:60"},
		{"three-digit-year", "AF-4.6.4-4", "202-01-02T03:04:05"},
		{"five-digit-year", "AF-4.6.4-4", "20260-01-02T03:04:05"},
		{"one-digit-month", "AF-4.6.4-4", "2026-1-02T03:04:05"},
		{"epoch", "AF-4.6.4-5", "-4713-11-24T12:00:00"},
		{"zero-julian", "AF-4.6.4-7", "0"},
		{"negative-julian", "AF-4.6.4-7", "-1"},
		{"julian-upper-bound", "AF-4.6.4-7", "5373484.5"},
		{"julian-rounds-up-to-bound", "AF-4.6.4-7", "5373484.4999999999"},
		{"julian-rounds-to-zero", "AF-4.6.4-7", "1e-10"},
		{"julian-hex", "AF-4.6.4-6", "0x258000"},
		{"julian-inf", "AF-4.6.4-6", "inf"},
		{"julian-nan", "AF-4.6.4-6", "nan"},
	}
	var cases []artifactCase
	for _, g := range julian {
		cases = append(cases, loose("artifact.4.6.4."+g.slug, "AF-4.6.4-6",
			edit(baseManifest, "D "+ts, "D "+g.token), func(d *deck.Deck) error {
				return want("D", d.D.UTC().Format("2006-01-02T15:04:05.000"), g.instant)
			}))
	}
	for _, g := range good {
		cases = append(cases, loose("artifact.4.6.4."+g.slug, "AF-4.6.4-2",
			edit(baseManifest, "D "+ts, "D "+g.token), func(d *deck.Deck) error {
				return want("D", d.D.UTC().Format("2006-01-02T15:04:05.000"), g.instant)
			}))
	}
	for _, b := range bad {
		cases = append(cases, rejected("artifact.4.6.4."+b.slug, b.req,
			edit(baseManifest, "D "+ts, "D "+b.token)))
	}
	return append(cases,
		loose("artifact.4.6.4.fraction-round-trip", "AF-4.6.4-2",
			edit(baseManifest, "D "+ts, "D 2026-01-02T03:04:05.123456"), roundTrips),
		loose("artifact.4.6.4.e-fraction-round-trip", "AF-4.6.4-2",
			edit(baseEvent, "E 2026-01-02T03:04:05 ", "E 2026-01-02T03:04:05.5 "), roundTrips),
		rejected("artifact.4.6.4.e-invalid-month", "AF-4.6.4-4",
			edit(baseEvent, "E 2026-01-02T", "E 2026-13-02T")),
		rejected("artifact.4.6.4.e-lowercase-t", "AF-4.6.4-3",
			edit(baseEvent, "E 2026-01-02T", "E 2026-01-02t")),
	)
}

// roundTrips checks that the instant the parser read survives Marshal; the
// case itself is non-canonical, since Marshal may write fewer digits.
func roundTrips(d *deck.Deck) error {
	encoded, err := d.Marshal()
	if err != nil {
		return err
	}
	again, err := deck.Parse(encoded)
	if err != nil {
		return expectf("deck.Parse rejects what Marshal wrote: %v", err)
	}
	if !again.D.Equal(d.D) {
		return expectf("D is %v after a round trip, was %v", again.D, d.D)
	}
	if d.E != nil && !again.E.Date.Equal(d.E.Date) {
		return expectf("E date is %v after a round trip, was %v", again.E.Date, d.E.Date)
	}
	return nil
}

var cardCases = []artifactCase{
	accepted("artifact.4.7.f-sha3", "AF-4.7.1-1", edit(baseManifest, "F a.txt "+h1, "F a.txt "+h64),
		cardIs("F[0].UUID", func(d *deck.Deck) string { return d.F[0].UUID }, h64)),
	rejected("artifact.4.7.k-sha3-reject", "AF-4.7.1-2", edit(baseTicket, "K "+h1, "K "+h64)),
	accepted("artifact.4.7.f-uppercase-hash", "AF-3.3-2",
		edit(baseManifest, "F a.txt "+h1, "F a.txt "+strings.ToUpper(h1)),
		cardIs("F[0].UUID", func(d *deck.Deck) string { return d.F[0].UUID }, strings.ToUpper(h1))),
	loose("artifact.4.7.2.a-tail", "AF-4.7.2-1",
		edit(baseAttachment, "A report.txt", "A dir/report.txt"),
		cardIs("A.Filename", func(d *deck.Deck) string { return d.A.Filename }, "report.txt")),
	accepted("artifact.4.7.2.a-target-wiki", "AF-4.7.2-2",
		edit(baseAttachment, "A report.txt "+h1, "A report.txt HomePage"),
		cardIs("A.Target", func(d *deck.Deck) string { return d.A.Target }, "HomePage")),
	rejected("artifact.4.7.2.a-target-bad", "AF-4.7.2-2",
		edit(baseAttachment, "A report.txt "+h1, `A report.txt Home\s\sPage`)),
	accepted("artifact.4.7.2.a-no-source", "AF-4.7.2-3",
		edit(baseAttachment, "A report.txt "+h1+" "+h2, "A report.txt "+h1),
		cardIs("A.Source", func(d *deck.Deck) string { return d.A.Source }, "")),
	rejected("artifact.4.7.2.a-empty-source", "AF-4.7.2-3",
		edit(baseAttachment, "A report.txt "+h1+" "+h2, "A report.txt "+h1+" ")),
	rejected("artifact.4.7.c-missing", "AF-4.7.4-1",
		edit(baseAttachment, "C attached\\sa\\sreport\n", "C\n")),
	rejected("artifact.4.7.5.e-one-token", "AF-4.7.5-1",
		edit(baseEvent, "E 2026-01-02T03:04:05 "+h1, "E 2026-01-02T03:04:05")),
	rejected("artifact.4.7.f-no-hash-baseline", "AF-4.7.6-1",
		edit(baseManifest, "F a.txt "+h1+"\n", "F a.txt\n")),
	accepted("artifact.4.7.f-deletion-delta", "AF-4.7.6-1", baseDelta,
		cardIs("F[1]", func(d *deck.Deck) string { return d.F[1].Name + "=" + d.F[1].UUID },
			"gone.txt=")),
	accepted("artifact.4.7.f-opaque-permission", "AF-4.7.6-2",
		edit(baseManifest, "F a.txt "+h1+"\n", "F a.txt "+h1+" strange\n"),
		cardIs("F[0].Perm", func(d *deck.Deck) string { return d.F[0].Perm }, "strange")),
	accepted("artifact.4.7.6.third-is-permission", "AF-4.7.6-2",
		edit(baseManifest, "F a.txt "+h1+"\n", "F a.txt "+h1+" old.txt\n"),
		cardIs("F[0]", func(d *deck.Deck) string { return d.F[0].Perm + "|" + d.F[0].OldName },
			"old.txt|")),
	accepted("artifact.4.7.j-absent-value", "AF-4.7.8-1",
		edit(baseTicket, "J status Open", "J status"),
		cardIs("J[1].Value", func(d *deck.Deck) string { return d.J[1].Value }, "")),
	accepted("artifact.4.7.p-empty", "AF-4.7.13-1", edit(baseManifest, "P "+h4+" "+h5+"\n", "P\n"),
		func(d *deck.Deck) error { return want("len(P)", len(d.P), 0) }),
	rejected("artifact.4.7.q-invalid-sign", "AF-4.7.14-1", edit(baseManifest, "Q +", "Q *")),
	accepted("artifact.4.7.14.q-no-baseline", "AF-4.7.14-1",
		edit(baseManifest, "Q +"+h6+" "+h7, "Q +"+h6),
		func(d *deck.Deck) error { return want("Q[0]", d.Q[0], deck.CherryPick{Target: h6}) }),
	accepted("artifact.4.7.14.q-backout", "AF-4.7.14-1", edit(baseManifest, "Q +", "Q -"),
		func(d *deck.Deck) error { return want("Q[0].IsBackout", d.Q[0].IsBackout, true) }),
	rejected("artifact.4.7.16.t-bad-sign", "AF-4.7.16-1",
		edit(baseControl, "T +bgcolor", "T !bgcolor")),
	rejected("artifact.4.7.t-hex-name", "AF-4.7.16-2", edit(baseControl, "T +bgcolor", "T +abc")),
	rejected("artifact.4.7.16.t-hex-name-uppercase", "AF-4.7.16-2",
		edit(baseControl, "T +bgcolor", "T +ABC")),
	rejected("artifact.4.7.16.t-hex-name-long", "AF-4.7.16-2",
		edit(baseControl, "T +bgcolor", "T +"+h1+"0")),
	rejected("artifact.4.7.16.t-bad-target", "AF-4.7.16-3",
		edit(baseControl, "T +bgcolor "+h1, "T +bgcolor **")),
	loose("artifact.4.7.u-empty", "AF-4.7.17-1", edit(baseManifest, "U alice\n", "U\n"),
		cardIs("U", func(d *deck.Deck) string { return *d.U }, "anonymous")),
	loose("artifact.4.7.u-space-empty", "AF-4.7.17-1", edit(baseManifest, "U alice\n", "U \n"),
		cardIs("U", func(d *deck.Deck) string { return *d.U }, "anonymous")),
	accepted("artifact.4.7.w-opaque", "AF-4.7.18-1",
		edit(baseWiki, "W 12\nhello\nworld\n\n", "W 5\na\n\x00b!\n"),
		cardIs("W", func(d *deck.Deck) string { return string(d.W) }, "a\n\x00b!")),
	accepted("artifact.4.7.18.w-body-cards", "AF-4.7.18-1",
		edit(baseWiki, "W 12\nhello\nworld\n\n", "W 6\nU bob\n\n"),
		cardIs("W", func(d *deck.Deck) string { return string(d.W) }, "U bob\n")),
	accepted("artifact.4.7.18.w-empty", "AF-4.7.18-1",
		edit(baseWiki, "W 12\nhello\nworld\n\n", "W 0\n\n"),
		func(d *deck.Deck) error { return want("len(W)", len(d.W), 0) }),
	// The body is followed by "!" where LF belongs, then the Z card.
	raw("artifact.4.7.w-missing-lf", "AF-4.7.18-2", edit(baseWiki, "world\n\n", "world\n!")+
		zCard(edit(baseWiki, "world\n\n", "world\n!")), false),
	loose("artifact.4.7.w-count-suffix", "AF-4.7.18-3", edit(baseWiki, "W 12\n", "W 12x\n"),
		cardIs("W", func(d *deck.Deck) string { return string(d.W) }, "hello\nworld\n")),
	loose("artifact.4.7.w-nondigit-zero", "AF-4.7.18-3",
		edit(baseWiki, "W 12\nhello\nworld\n\n", "W abc\n\n"),
		func(d *deck.Deck) error { return want("len(W)", len(d.W), 0) }),
	loose("artifact.4.7.18.w-leading-zero", "AF-4.7.18-3", edit(baseWiki, "W 12\n", "W 012\n"),
		cardIs("W", func(d *deck.Deck) string { return string(d.W) }, "hello\nworld\n")),
	// "+12" has no leading digit, so its size is 0 and the byte after the
	// empty body is "h", not LF.
	rejected("artifact.4.7.18.w-plus-sign", "AF-4.7.18-3", edit(baseWiki, "W 12\n", "W +12\n")),
	rejected("artifact.4.7.w-short", "AF-4.7.18-4", edit(baseWiki, "W 12\n", "W 999\n")),
	rejected("artifact.4.7.w-overflow", "AF-4.7.18-4",
		edit(baseWiki, "W 12\nhello\nworld\n\n", "W 184467440737095516160\n\n")),
	// 2^64 + 12: an accumulator that wraps instead of failing reads 12.
	rejected("artifact.4.7.18.w-overflow-wrap", "AF-4.7.18-4",
		edit(baseWiki, "W 12\n", "W 18446744073709551628\n")),
	raw("artifact.4.7.z-extra-token", "AF-4.2.1-1",
		baseControl+strings.TrimSuffix(zCard(baseControl), "\n")+" extra\n", false),
}

// typeCases checks each base's type, then the type rules of Section 5.
func typeCases() []artifactCase {
	var cases []artifactCase
	for _, b := range artifactBases {
		if b.name == "delta" {
			continue
		}
		cases = append(cases, accepted("artifact.5.1."+b.name, "AF-5.1-1",
			strings.Join(b.cards, ""), wantType(b.goType)))
	}
	return append(cases,
		rejected("artifact.5.3.latest-setter-illegal", "AF-5.3-1",
			edit(baseWiki, "C edit", "A x.txt "+h8+" "+h9+"\nC edit")),
		accepted("artifact.5.3.parent-fallback", "AF-5.3-2", edit(baseControl,
			"T +bgcolor "+h1+" blue\nT -sym-old "+h2+"\n", "P "+h1+"\n"), wantType(deck.Checkin)),
		accepted("artifact.5.3.comment-fallback", "AF-5.3-2",
			edit(baseControl, "D ", "C note\nD "), wantType(deck.Checkin)),
		// §5.3 / fossil manifest.c:1076: the fallback keys on C-card presence
		// (zComment != 0), not a non-empty comment, so a present-but-empty C
		// still makes a tagless D/T/U artifact a check-in, not a control.
		accepted("artifact.5.3.comment-fallback-empty", "AF-5.3-2",
			edit(baseControl, "D ", "C \nD "), wantType(deck.Checkin)),
		accepted("artifact.5.3.control-fallback", "AF-5.3-2", baseControl, wantType(deck.Control)),
		rejected("artifact.5.4.control-self", "AF-5.4-1",
			edit(baseControl, "T -sym-old "+h2, "T -sym-old *")),
		rejected("artifact.5.4.event-external-tag", "AF-5.4-2",
			edit(baseEvent, "T +bgcolor *", "T +bgcolor "+h3)),
		rejected("artifact.5.4.event-propagating", "AF-5.4-2",
			edit(baseEvent, "T +bgcolor", "T *bgcolor")),
		rejected("artifact.5.4.event-cancel", "AF-5.4-2",
			edit(baseEvent, "T +bgcolor", "T -bgcolor")),
		rejected("artifact.5.4.forum-title-reply", "AF-5.4-3",
			edit(baseForum, "I "+h2+"\n", "H title\nI "+h2+"\n")),
		rejected("artifact.5.4.forum-many-parents", "AF-5.4-4",
			edit(baseForum, "P "+h3, "P "+h3+" "+h4)),
	)
}

var checksumCases = []artifactCase{
	rejected("artifact.6.1.r-nonhex", "AF-6.1-1",
		edit(baseManifest, "R "+md5Token, "R "+strings.Repeat("g", 32))),
	rejected("artifact.6.1.r-short", "AF-6.1-1",
		edit(baseManifest, "R "+md5Token, "R "+md5Token[:31])),
	accepted("artifact.6.1.r-uppercase", "AF-6.1-1",
		edit(baseManifest, "R "+md5Token, "R "+strings.ToUpper(md5Token)),
		cardIs("R", func(d *deck.Deck) string { return d.R }, strings.ToUpper(md5Token))),
	zRejected("artifact.6.1.z-short", "AF-6.1-1", baseControl+zCard(baseControl)[:33]+"\n"),
	zRejected("artifact.6.2.mismatch", "AF-6.2-1", baseControl+"Z "+strings.Repeat("0", 32)+"\n"),
	zRejected("artifact.6.2.uppercase", "AF-6.2-2",
		baseControl+strings.ToUpper(zCard(baseControl))),
	zRejected("artifact.6.2.missing", "AF-6.2-3", baseControl),
	// The Z card covers the escaped wire bytes, not the decoded comment.
	accepted("artifact.6.2.raw-before-decode", "AF-6.2-2", baseManifest,
		cardIs("C", func(d *deck.Deck) string { return d.C }, "base manifest")),
	accepted("artifact.6.3.1.r-unchecked", "AF-6.3-1", baseManifest,
		cardIs("R", func(d *deck.Deck) string { return d.R }, md5Token)),
}

// Generated cases: one per card letter or hash position, each a single
// edit of a base.

// cardLine names one card line of a base.
type cardLine struct {
	letter byte
	base   string
	line   string
}

func duplicateCases() []artifactCase {
	lines := []cardLine{
		{'A', baseAttachment, attachmentCards[0]}, {'B', baseDelta, deltaCards[0]},
		{'C', baseManifest, manifestCards[0]}, {'D', baseManifest, manifestCards[1]},
		{'E', baseEvent, eventCards[2]}, {'G', baseForum, forumCards[1]},
		{'I', baseForum, forumCards[2]}, {'K', baseTicket, ticketCards[3]},
		{'L', baseWiki, wikiCards[2]}, {'N', baseWiki, wikiCards[3]},
		{'R', baseManifest, manifestCards[7]}, {'U', baseManifest, manifestCards[10]},
		{'W', baseWiki, wikiCards[6]},
	}
	var cases []artifactCase
	for _, l := range lines {
		c := rejected(fmt.Sprintf("artifact.4.5.1.duplicate-%c", l.letter),
			"AF-4.5.1-1", edit(l.base, l.line, l.line+l.line))
		c.fossilSays = "more than one " + string(l.letter) + "-card"
		if l.letter == 'A' {
			// fossil 2.28 rejects a second A card with no message of its own.
			c.fossilSays = "unknown error on line 2"
		}
		cases = append(cases, c)
	}
	// No base has an H card: a reply post has I, which H may not join.
	return append(cases, rejected("artifact.4.5.1.duplicate-H", "AF-4.5.1-1",
		edit(baseForum, "I "+h2+"\n", "H title\nH title\n")))
}

// arityCases adds one token to a card already at its full arity.
func arityCases() []artifactCase {
	lines := []cardLine{
		{'A', baseAttachment, attachmentCards[0]}, {'B', baseDelta, deltaCards[0]},
		{'C', baseManifest, manifestCards[0]}, {'D', baseManifest, manifestCards[1]},
		{'E', baseEvent, eventCards[2]}, {'F', baseManifest, manifestCards[4]},
		{'G', baseForum, forumCards[1]}, {'I', baseForum, forumCards[2]},
		{'J', baseTicket, ticketCards[2]}, {'K', baseTicket, ticketCards[3]},
		{'L', baseWiki, wikiCards[2]}, {'M', baseCluster, clusterCards[0]},
		{'N', baseWiki, wikiCards[3]}, {'Q', baseManifest, manifestCards[6]},
		{'R', baseManifest, manifestCards[7]}, {'T', baseControl, controlCards[1]},
		{'U', baseManifest, manifestCards[10]}, {'W', baseWiki, wikiCards[6]},
	}
	var cases []artifactCase
	for _, l := range lines {
		first := l.line[:strings.IndexByte(l.line, '\n')+1]
		c := rejected(fmt.Sprintf("artifact.4.2.1.extra-%c", l.letter),
			"AF-4.2.1-1", edit(l.base, first, strings.TrimSuffix(first, "\n")+" extra\n"))
		c.fossilSays = "extra characters at end of card"
		if l.letter == 'W' {
			// fossil reads the size, then finds no body where it expects one.
			c.fossilSays = "no content after W-card"
		}
		cases = append(cases, c)
	}
	return append(cases, rejected("artifact.4.2.1.extra-H", "AF-4.2.1-1",
		edit(baseForum, "I "+h2+"\n", "H title extra\n")))
}

// hashCases breaks the hash at each hash-bearing position three ways. The
// A target is left out: a broken hash there is still a valid wiki name.
func hashCases() []artifactCase {
	positions := []struct {
		slug, base, line, hash string
	}{
		{"a-source", baseAttachment, attachmentCards[0], h2},
		{"b", baseDelta, deltaCards[0], h1},
		{"e-id", baseEvent, eventCards[2], h1},
		{"f", baseManifest, manifestCards[2], h1},
		{"g", baseForum, forumCards[1], h1},
		{"i", baseForum, forumCards[2], h2},
		{"k", baseTicket, ticketCards[3], h1},
		{"m", baseCluster, clusterCards[0], h1},
		{"p", baseManifest, manifestCards[5], h4},
		{"q-target", baseManifest, manifestCards[6], h6},
		{"q-baseline", baseManifest, manifestCards[6], h7},
		{"t-target", baseControl, controlCards[1], h1},
	}
	var cases []artifactCase
	for _, p := range positions {
		req := "AF-4.7.1-1"
		if p.slug == "k" {
			req = "AF-4.7.1-2"
		}
		for _, bad := range []struct{ slug, hash string }{
			{"short", p.hash[:39]}, {"long", p.hash + "0"}, {"nonhex", p.hash[:39] + "g"},
		} {
			line := strings.Replace(p.line, p.hash, bad.hash, 1)
			cases = append(cases, rejected("artifact.4.7.1."+p.slug+"-"+bad.slug, req,
				edit(p.base, p.line, line)))
		}
	}
	return cases
}

// legalitySamples is a well-formed card of each letter, for insertion into
// a base whose type does not allow it.
var legalitySamples = map[byte]string{
	'A': "A x.txt " + h8 + " " + h9 + "\n", 'B': "B " + h8 + "\n", 'C': "C inserted\n",
	'D': "D " + ts + "\n", 'E': "E 2026-01-02T03:04:05 " + h8 + "\n", 'F': "F x.txt " + h8 + "\n",
	'G': "G " + h8 + "\n", 'H': "H title\n", 'I': "I " + h8 + "\n", 'J': "J field value\n",
	'K': "K " + h8 + "\n", 'L': "L Page\n", 'M': "M " + h8 + "\n", 'N': "N text/plain\n",
	'P': "P " + h8 + "\n", 'Q': "Q +" + h8 + "\n", 'R': "R " + md5Token + "\n",
	'T': "T +tag " + h8 + "\n", 'U': "U bob\n", 'W': "W 1\nx\n",
}

// legalityCases inserts each disallowed letter into each base, and removes
// each required one. A change the draft's own tables would accept, because
// it turns the artifact into another valid type, is left out: the cases are
// about rejection.
func legalityCases() []artifactCase {
	var cases []artifactCase
	for _, b := range artifactBases {
		if b.name == "delta" {
			continue
		}
		for letter := byte('A'); letter <= 'W'; letter++ {
			sample, ok := legalitySamples[letter]
			if !ok || strings.IndexByte(b.allowed, letter) >= 0 {
				continue
			}
			id := fmt.Sprintf("artifact.5.2.%s-illegal-%c", b.name, letter)
			cases = appendIllegal(cases, id, insertCard(b.cards, sample))
		}
		for _, letter := range []byte(strings.TrimSuffix(b.required, "Z")) {
			id := fmt.Sprintf("artifact.5.2.%s-no-%c", b.name, letter)
			cases = appendIllegal(cases, id, removeCards(b.cards, letter))
		}
	}
	return cases
}

// appendIllegal adds a rejection case for cards when the draft's tables
// reject them. The requirement is the one the tables break, which is not
// always the one the edit aimed at: removing a wiki's L card leaves no
// type-setting card, so the artifact falls back to a manifest and its W
// card is what is illegal.
func appendIllegal(cases []artifactCase, id string, cards []string) []artifactCase {
	req := draftRule(cards)
	if req == "" {
		return cases
	}
	c := rejected(id, req, strings.Join(cards, ""))
	// fossil names the first card it finds out of place, or missing.
	c.fossilSays = "-card in "
	if req == "AF-5.2-2" {
		c.fossilSays = "-card missing in "
	}
	return append(cases, c)
}

// insertCard puts card before the first card whose letter sorts after it.
func insertCard(cards []string, card string) []string {
	at := len(cards)
	for i, c := range cards {
		if c[0] > card[0] {
			at = i
			break
		}
	}
	out := append([]string{}, cards[:at]...)
	out = append(out, card)
	return append(out, cards[at:]...)
}

func removeCards(cards []string, letter byte) []string {
	var out []string
	for _, c := range cards {
		if c[0] != letter {
			out = append(out, c)
		}
	}
	return out
}

// draftRule applies the draft's type determination (Section 5.3) and card
// table (Section 5.2) to cards, which carry no Z card yet. It returns the
// requirement they break, an illegal card before a missing one as fossil
// checks them, or "" when the tables accept them.
func draftRule(cards []string) string {
	setters := map[byte]string{'A': "attachment", 'B': "manifest", 'F': "manifest",
		'Q': "manifest", 'R': "manifest", 'E': "event", 'G': "forum", 'H': "forum",
		'I': "forum", 'J': "ticket", 'K': "ticket", 'L': "wiki", 'M': "cluster"}
	kind, present := "", "Z"
	for _, c := range cards {
		if s, ok := setters[c[0]]; ok {
			kind = s
		}
		present += string(c[0])
	}
	if kind == "" {
		kind = "control"
		if strings.ContainsAny(present, "CP") {
			kind = "manifest"
		}
	}
	for _, b := range artifactBases {
		if b.name != kind {
			continue
		}
		for _, letter := range []byte(present) {
			if strings.IndexByte(b.allowed, letter) < 0 {
				return "AF-5.2-1"
			}
		}
		for _, letter := range []byte(b.required) {
			if strings.IndexByte(present, letter) < 0 {
				return "AF-5.2-2"
			}
		}
		return ""
	}
	panic("conformance: draftRule: no base for type " + kind)
}
