package conformance

import "testing"

// artifactGaps marks the artifact cases go-libfossil is known to fail, by
// case ID, grouped by the code at fault. "#0" stands in for the issue until
// one is filed.
var artifactGaps = map[string]string{
	// deck.VerifyZ rejects an artifact too short to hold a Z card, and one
	// whose last 35 bytes are not a Z card, where Section 6.2 says it reports
	// the Z card absent; and it requires LF at the last byte, which it must
	// not read (deck/zcard.go:23, :26). With no framing gates or residue check
	// of its own, deck.Parse leaves these inputs to that check, so the rule
	// each case names never runs.
	"artifact.6.2.verify-missing-z": "#0",
	"artifact.6.2.verify-too-short": "#0",
	"artifact.6.2.verify-last-byte": "#0",
	"artifact.4.1.empty":            "#0",
	"artifact.4.1.no-final-lf":      "#0",
	"artifact.4.1.short":            "#0",
	"artifact.4.2.residual":         "#0",
	"artifact.4.1.2.sterilized":     "#0",
	"artifact.4.1.2.signed-guard":   "#0",
	"artifact.4.1.1.leading-space":  "#0",
	"artifact.4.1.1.header-short":   "#0",
	"artifact.4.1.1.missing-blank":  "#0",
	"artifact.4.7.z-extra-token":    "#0",
	// Framing: no check that the raw input ends in LF before the wrapper is
	// stripped (deck/parse.go:40), and a bare C line passes as an empty comment
	// (parse.go:103, :174).
	"artifact.4.1.1.outer-no-lf": "#0",
	"artifact.4.1.first-bare":    "#0",
	"artifact.4.7.c-missing":     "#0",
	// The armor match compares all 30 bytes, not 29 (deck/clearsign.go:16, :65).
	"artifact.4.1.1.pgp-short": "#0",
	"artifact.4.1.1.ssh-short": "#0",
	// Tokens: strings.Fields and strings.TrimSpace split or trim on tab and CR,
	// collapse an empty token, and readLine's caller skips empty lines
	// (deck/parse.go:54, :171-204, :251).
	"artifact.4.2.2.blank-line": "#0",
	"artifact.4.2.double-space": "#0",
	"artifact.4.2.file-tab":     "#0",
	"artifact.4.2.hash-cr":      "#0",
	"artifact.4.2.login-cr":     "#0",
	// Marshal drops a present but empty C, N or T value, an empty P card and an
	// empty W body, so the artifact does not re-encode (deck/marshal.go:58, :149,
	// :153, :194, :205).
	"artifact.4.7.16.t-empty-value": "#0",
	"artifact.4.7.18.w-empty":       "#0",
	"artifact.4.7.c-empty":          "#0",
	"artifact.4.7.n-empty":          "#0",
	"artifact.4.7.p-empty":          "#0",
	// No maximum arity: cards are split with SplitN or taken whole, so an extra
	// token is absorbed (deck/parse.go:171-204, :215, :250, :289, :308, :323).
	"artifact.4.2.1.extra-A": "#0",
	"artifact.4.2.1.extra-B": "#0",
	"artifact.4.2.1.extra-C": "#0",
	"artifact.4.2.1.extra-E": "#0",
	"artifact.4.2.1.extra-F": "#0",
	"artifact.4.2.1.extra-G": "#0",
	"artifact.4.2.1.extra-H": "#0",
	"artifact.4.2.1.extra-I": "#0",
	"artifact.4.2.1.extra-J": "#0",
	"artifact.4.2.1.extra-K": "#0",
	"artifact.4.2.1.extra-L": "#0",
	"artifact.4.2.1.extra-M": "#0",
	"artifact.4.2.1.extra-N": "#0",
	"artifact.4.2.1.extra-Q": "#0",
	"artifact.4.2.1.extra-R": "#0",
	"artifact.4.2.1.extra-T": "#0",
	"artifact.4.2.1.extra-U": "#0",
	// Escape decoding: the A target and N mimetype are kept raw (deck/parse.go:194,
	// :220), and an F hash is never validated, so an escaped one passes.
	"artifact.4.3.1.a-target":              "#0",
	"artifact.4.3.1.a-target-escaped-hash": "#0",
	"artifact.4.3.1.hash-raw":              "#0",
	"artifact.4.3.1.n-mimetype":            "#0",
	// No pathname predicate on the A filename or the F filename and old-name
	// (deck/parse.go:215, :250).
	"artifact.4.6.1.a-double-slash":         "#0",
	"artifact.4.6.1.a-leading-slash":        "#0",
	"artifact.4.6.1.f-dot":                  "#0",
	"artifact.4.6.1.f-dot-element":          "#0",
	"artifact.4.6.1.f-dot-prefix":           "#0",
	"artifact.4.6.1.f-dot-tail":             "#0",
	"artifact.4.6.1.f-dotdot":               "#0",
	"artifact.4.6.1.f-dotdot-element":       "#0",
	"artifact.4.6.1.f-dotdot-prefix":        "#0",
	"artifact.4.6.1.f-dotdot-tail":          "#0",
	"artifact.4.6.1.f-double-slash":         "#0",
	"artifact.4.6.1.f-leading-slash":        "#0",
	"artifact.4.6.1.f-trailing-slash":       "#0",
	"artifact.4.6.1.old-name-dotdot-prefix": "#0",
	"artifact.4.6.1.old-name-leading-slash": "#0",
	// Check-in writes any filename it is given (manifest/manifest.go:180).
	"artifact.4.6.1.writer-backslash":    "#0",
	"artifact.4.6.1.writer-invalid-utf8": "#0",
	"artifact.4.6.1.writer-surrogate":    "#0",
	// No wiki-name predicate on the L card or the A target (deck/parse.go:189, :220).
	"artifact.4.6.2.l-101-bytes":      "#0",
	"artifact.4.6.2.l-double-space":   "#0",
	"artifact.4.6.2.l-inner-lf":       "#0",
	"artifact.4.6.2.l-inner-tab":      "#0",
	"artifact.4.6.2.l-leading-space":  "#0",
	"artifact.4.6.2.l-leading-tab":    "#0",
	"artifact.4.6.2.l-trailing-space": "#0",
	"artifact.4.7.2.a-target-bad":     "#0",
	// No reserved-filename drop (deck/parse.go:250).
	"artifact.4.6.3.drop-_fossil_":            "#0",
	"artifact.4.6.3.drop-_FOSSIL_":            "#0",
	"artifact.4.6.3.drop-.fslckout":           "#0",
	"artifact.4.6.3.drop-.FSLCKOUT-SHM":       "#0",
	"artifact.4.6.3.drop-a_FOSSIL_-wal":       "#0",
	"artifact.4.6.3.drop-a/_FOSSIL_":          "#0",
	"artifact.4.6.3.drop-a/.fslckout-journal": "#0",
	"artifact.4.6.3.drop-not-anchor":          "#0",
	// parseTimestamp accepts only two time.Parse layouts, so the wider accepted
	// grammar is rejected, and it rewrites a lowercase t to T (deck/parse.go:420).
	"artifact.4.6.4.after-epoch":       "#0",
	"artifact.4.6.4.double-separator":  "#0",
	"artifact.4.6.4.e-lowercase-t":     "#0",
	"artifact.4.6.4.hh-mm":             "#0",
	"artifact.4.6.4.hour-24":           "#0",
	"artifact.4.6.4.lowercase-t":       "#0",
	"artifact.4.6.4.no-calendar-check": "#0",
	"artifact.4.6.4.tab-separator":     "#0",
	"artifact.4.6.4.zone-lowercase-z":  "#0",
	"artifact.4.6.4.zone-minus":        "#0",
	"artifact.4.6.4.zone-plus":         "#0",
	"artifact.4.6.4.zone-z":            "#0",
	// Marshal writes D to the millisecond and E to the second, so a longer
	// fraction the parser accepted is lost (deck/marshal.go:63, :67). Found
	// by FuzzArtifactRoundTrip.
	"artifact.4.6.4.fraction-round-trip":   "#0",
	"artifact.4.6.4.e-fraction-round-trip": "#0",
	// No hash validation at the A source, B, E, F, G, I, K, M, P or Q positions
	// (deck/parse.go:171-204, :215, :250, :277, :308); K is not held to 40 digits.
	"artifact.4.7.1.a-source-long":     "#0",
	"artifact.4.7.1.a-source-nonhex":   "#0",
	"artifact.4.7.1.a-source-short":    "#0",
	"artifact.4.7.1.b-long":            "#0",
	"artifact.4.7.1.b-nonhex":          "#0",
	"artifact.4.7.1.b-short":           "#0",
	"artifact.4.7.1.e-id-long":         "#0",
	"artifact.4.7.1.e-id-nonhex":       "#0",
	"artifact.4.7.1.e-id-short":        "#0",
	"artifact.4.7.1.f-long":            "#0",
	"artifact.4.7.1.f-nonhex":          "#0",
	"artifact.4.7.1.f-short":           "#0",
	"artifact.4.7.1.g-long":            "#0",
	"artifact.4.7.1.g-nonhex":          "#0",
	"artifact.4.7.1.g-short":           "#0",
	"artifact.4.7.1.i-long":            "#0",
	"artifact.4.7.1.i-nonhex":          "#0",
	"artifact.4.7.1.i-short":           "#0",
	"artifact.4.7.1.k-long":            "#0",
	"artifact.4.7.1.k-nonhex":          "#0",
	"artifact.4.7.1.k-short":           "#0",
	"artifact.4.7.1.m-long":            "#0",
	"artifact.4.7.1.m-nonhex":          "#0",
	"artifact.4.7.1.m-short":           "#0",
	"artifact.4.7.1.p-long":            "#0",
	"artifact.4.7.1.p-nonhex":          "#0",
	"artifact.4.7.1.p-short":           "#0",
	"artifact.4.7.1.q-baseline-long":   "#0",
	"artifact.4.7.1.q-baseline-nonhex": "#0",
	"artifact.4.7.1.q-baseline-short":  "#0",
	"artifact.4.7.1.q-target-long":     "#0",
	"artifact.4.7.1.q-target-nonhex":   "#0",
	"artifact.4.7.1.q-target-short":    "#0",
	"artifact.4.7.k-sha3-reject":       "#0",
	// Single cards: the A filename keeps its directory (deck/parse.go:220), a
	// baseline F card may lack a hash (:250), any Q sign but - is read as +
	// (:312), the hex-name check is lowercase only (:17), and an R token is
	// stored unchecked (:204).
	"artifact.4.7.16.t-hex-name-uppercase": "#0",
	"artifact.4.7.2.a-tail":                "#0",
	"artifact.4.7.f-no-hash-baseline":      "#0",
	"artifact.4.7.q-invalid-sign":          "#0",
	"artifact.6.1.r-nonhex":                "#0",
	"artifact.6.1.r-short":                 "#0",
	// W size: strconv.Atoi rejects 12x and abc but takes +12, and the byte after
	// the body is consumed without checking it is LF (deck/parse.go:73-86).
	"artifact.4.7.18.w-plus-sign":  "#0",
	"artifact.4.7.w-count-suffix":  "#0",
	"artifact.4.7.w-missing-lf":    "#0",
	"artifact.4.7.w-nondigit-zero": "#0",
	// No card-legality table, required cards or per-type semantic checks:
	// inferType only picks a type (deck/parse.go:442).
	"artifact.5.2.attachment-illegal-B":  "#0",
	"artifact.5.2.attachment-illegal-E":  "#0",
	"artifact.5.2.attachment-illegal-F":  "#0",
	"artifact.5.2.attachment-illegal-G":  "#0",
	"artifact.5.2.attachment-illegal-H":  "#0",
	"artifact.5.2.attachment-illegal-I":  "#0",
	"artifact.5.2.attachment-illegal-J":  "#0",
	"artifact.5.2.attachment-illegal-K":  "#0",
	"artifact.5.2.attachment-illegal-L":  "#0",
	"artifact.5.2.attachment-illegal-M":  "#0",
	"artifact.5.2.attachment-illegal-P":  "#0",
	"artifact.5.2.attachment-illegal-Q":  "#0",
	"artifact.5.2.attachment-illegal-R":  "#0",
	"artifact.5.2.attachment-illegal-T":  "#0",
	"artifact.5.2.attachment-illegal-W":  "#0",
	"artifact.5.2.attachment-no-D":       "#0",
	"artifact.5.2.cluster-illegal-A":     "#0",
	"artifact.5.2.cluster-illegal-B":     "#0",
	"artifact.5.2.cluster-illegal-C":     "#0",
	"artifact.5.2.cluster-illegal-D":     "#0",
	"artifact.5.2.cluster-illegal-E":     "#0",
	"artifact.5.2.cluster-illegal-F":     "#0",
	"artifact.5.2.cluster-illegal-G":     "#0",
	"artifact.5.2.cluster-illegal-H":     "#0",
	"artifact.5.2.cluster-illegal-I":     "#0",
	"artifact.5.2.cluster-illegal-J":     "#0",
	"artifact.5.2.cluster-illegal-K":     "#0",
	"artifact.5.2.cluster-illegal-L":     "#0",
	"artifact.5.2.cluster-illegal-N":     "#0",
	"artifact.5.2.cluster-illegal-P":     "#0",
	"artifact.5.2.cluster-illegal-Q":     "#0",
	"artifact.5.2.cluster-illegal-R":     "#0",
	"artifact.5.2.cluster-illegal-T":     "#0",
	"artifact.5.2.cluster-illegal-U":     "#0",
	"artifact.5.2.cluster-illegal-W":     "#0",
	"artifact.5.2.cluster-no-M":          "#0",
	"artifact.5.2.control-illegal-A":     "#0",
	"artifact.5.2.control-illegal-E":     "#0",
	"artifact.5.2.control-illegal-G":     "#0",
	"artifact.5.2.control-illegal-H":     "#0",
	"artifact.5.2.control-illegal-I":     "#0",
	"artifact.5.2.control-illegal-J":     "#0",
	"artifact.5.2.control-illegal-K":     "#0",
	"artifact.5.2.control-illegal-L":     "#0",
	"artifact.5.2.control-illegal-M":     "#0",
	"artifact.5.2.control-illegal-N":     "#0",
	"artifact.5.2.control-illegal-W":     "#0",
	"artifact.5.2.control-no-D":          "#0",
	"artifact.5.2.control-no-T":          "#0",
	"artifact.5.2.control-no-U":          "#0",
	"artifact.5.2.event-illegal-A":       "#0",
	"artifact.5.2.event-illegal-B":       "#0",
	"artifact.5.2.event-illegal-F":       "#0",
	"artifact.5.2.event-illegal-G":       "#0",
	"artifact.5.2.event-illegal-H":       "#0",
	"artifact.5.2.event-illegal-I":       "#0",
	"artifact.5.2.event-illegal-J":       "#0",
	"artifact.5.2.event-illegal-K":       "#0",
	"artifact.5.2.event-illegal-L":       "#0",
	"artifact.5.2.event-illegal-M":       "#0",
	"artifact.5.2.event-illegal-Q":       "#0",
	"artifact.5.2.event-illegal-R":       "#0",
	"artifact.5.2.event-no-D":            "#0",
	"artifact.5.2.event-no-E":            "#0",
	"artifact.5.2.event-no-W":            "#0",
	"artifact.5.2.forum-illegal-A":       "#0",
	"artifact.5.2.forum-illegal-B":       "#0",
	"artifact.5.2.forum-illegal-C":       "#0",
	"artifact.5.2.forum-illegal-E":       "#0",
	"artifact.5.2.forum-illegal-F":       "#0",
	"artifact.5.2.forum-illegal-J":       "#0",
	"artifact.5.2.forum-illegal-K":       "#0",
	"artifact.5.2.forum-illegal-L":       "#0",
	"artifact.5.2.forum-illegal-M":       "#0",
	"artifact.5.2.forum-illegal-Q":       "#0",
	"artifact.5.2.forum-illegal-R":       "#0",
	"artifact.5.2.forum-illegal-T":       "#0",
	"artifact.5.2.forum-no-D":            "#0",
	"artifact.5.2.forum-no-U":            "#0",
	"artifact.5.2.forum-no-W":            "#0",
	"artifact.5.2.manifest-illegal-A":    "#0",
	"artifact.5.2.manifest-illegal-E":    "#0",
	"artifact.5.2.manifest-illegal-G":    "#0",
	"artifact.5.2.manifest-illegal-H":    "#0",
	"artifact.5.2.manifest-illegal-I":    "#0",
	"artifact.5.2.manifest-illegal-J":    "#0",
	"artifact.5.2.manifest-illegal-K":    "#0",
	"artifact.5.2.manifest-illegal-L":    "#0",
	"artifact.5.2.manifest-illegal-M":    "#0",
	"artifact.5.2.manifest-illegal-W":    "#0",
	"artifact.5.2.manifest-no-D":         "#0",
	"artifact.5.2.ticket-illegal-A":      "#0",
	"artifact.5.2.ticket-illegal-B":      "#0",
	"artifact.5.2.ticket-illegal-C":      "#0",
	"artifact.5.2.ticket-illegal-E":      "#0",
	"artifact.5.2.ticket-illegal-F":      "#0",
	"artifact.5.2.ticket-illegal-G":      "#0",
	"artifact.5.2.ticket-illegal-H":      "#0",
	"artifact.5.2.ticket-illegal-I":      "#0",
	"artifact.5.2.ticket-illegal-L":      "#0",
	"artifact.5.2.ticket-illegal-M":      "#0",
	"artifact.5.2.ticket-illegal-N":      "#0",
	"artifact.5.2.ticket-illegal-P":      "#0",
	"artifact.5.2.ticket-illegal-Q":      "#0",
	"artifact.5.2.ticket-illegal-R":      "#0",
	"artifact.5.2.ticket-illegal-T":      "#0",
	"artifact.5.2.ticket-illegal-W":      "#0",
	"artifact.5.2.ticket-no-D":           "#0",
	"artifact.5.2.ticket-no-J":           "#0",
	"artifact.5.2.ticket-no-K":           "#0",
	"artifact.5.2.ticket-no-U":           "#0",
	"artifact.5.2.wiki-illegal-A":        "#0",
	"artifact.5.2.wiki-illegal-B":        "#0",
	"artifact.5.2.wiki-illegal-E":        "#0",
	"artifact.5.2.wiki-illegal-F":        "#0",
	"artifact.5.2.wiki-illegal-G":        "#0",
	"artifact.5.2.wiki-illegal-H":        "#0",
	"artifact.5.2.wiki-illegal-I":        "#0",
	"artifact.5.2.wiki-illegal-J":        "#0",
	"artifact.5.2.wiki-illegal-K":        "#0",
	"artifact.5.2.wiki-illegal-M":        "#0",
	"artifact.5.2.wiki-illegal-Q":        "#0",
	"artifact.5.2.wiki-illegal-R":        "#0",
	"artifact.5.2.wiki-illegal-T":        "#0",
	"artifact.5.2.wiki-no-D":             "#0",
	"artifact.5.2.wiki-no-L":             "#0",
	"artifact.5.2.wiki-no-U":             "#0",
	"artifact.5.2.wiki-no-W":             "#0",
	"artifact.5.3.latest-setter-illegal": "#0",
	"artifact.5.4.control-self":          "#0",
	"artifact.5.4.event-cancel":          "#0",
	"artifact.5.4.event-external-tag":    "#0",
	"artifact.5.4.event-propagating":     "#0",
	"artifact.5.4.forum-many-parents":    "#0",
	"artifact.5.4.forum-title-reply":     "#0",
	// hash/policy.go:81 derives the auto-to-sha3 promotion from the stored
	// names and never writes the hash-policy setting, so fossil opening the
	// repository still reads auto and keeps naming new content SHA-1.
	"artifact.7.4.ratchet-persisted": "#0",
	// With no hash-policy row an empty repository names new content SHA-1, not
	// SHA3 (hash/policy.go:68).
	"artifact.7.2.default-empty": "#0",
	// ListFiles takes any parseable artifact as the baseline, including a delta
	// manifest or a control artifact (manifest/files.go:44).
	"artifact.4.7.3.delta-baseline-invalid": "#0",
	"artifact.8.3.baseline-not-manifest":    "#0",
}

// TestArtifactGapKeys checks that every marked case exists, and that every
// loss the fuzz test forgives is still a marked gap.
func TestArtifactGapKeys(t *testing.T) {
	ids := map[string]bool{}
	for _, c := range artifactSuiteCases() {
		ids[c.ID] = true
	}
	for id := range artifactGaps {
		if !ids[id] {
			t.Errorf("artifactGaps names %s, which is not a case", id)
		}
	}
	for _, loss := range knownLosses {
		if artifactGaps[loss.caseID] == "" {
			t.Errorf("FuzzArtifactRoundTrip forgives %s, which is no longer a gap; "+
				"remove it from knownLosses", loss.caseID)
		}
	}
}
