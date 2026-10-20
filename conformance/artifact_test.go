package conformance

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// The artifact suite checks draft-fossil-artifact-format-01. Most of its
// cases are byte strings given to deck.Parse; each is built from one of the
// base artifacts below by a single edit, so the edit is the only thing that
// can explain a rejection, and every one is also given to fossil(1).

// artifactCase is one input to deck.Parse and what the draft says about it.
type artifactCase struct {
	Case
	// input is the whole artifact. Build it with withZ unless the case is
	// about the Z card itself.
	input  []byte
	accept bool
	// check, when set, inspects an accepted parse.
	check func(*deck.Deck) error
	// fossil is set only when fossil(1) gives the opposite verdict; it
	// cites the draft's note or the reason the case does not follow fossil.
	fossil string
	// nonCanonical marks an accepted input Marshal need not reproduce byte
	// for byte, such as a bare U card that reads back as "U anonymous".
	nonCanonical bool
	// zCase marks a rejection that is the Z card's to make. Every other
	// rejection must pass deck.VerifyZ first, so the rule the case targets,
	// not the checksum, is what rejects it.
	zCase bool
	// fossilSays, when set, is a substring fossil's reason for rejecting
	// the input must contain, for cases where several rules could reject it.
	// fossil is pinned at 2.28 in CI, so its wording is stable.
	fossilSays string
}

// Hash tokens. Their values only need to be well formed and, where a run of
// them must ascend, in order: h1 < h2 < ... < h9.
const (
	h1       = "0123456789abcdef0123456789abcdef01234567"
	h2       = "1123456789abcdef0123456789abcdef01234567"
	h3       = "2123456789abcdef0123456789abcdef01234567"
	h4       = "3123456789abcdef0123456789abcdef01234567"
	h5       = "4123456789abcdef0123456789abcdef01234567"
	h6       = "5123456789abcdef0123456789abcdef01234567"
	h7       = "6123456789abcdef0123456789abcdef01234567"
	h8       = "7123456789abcdef0123456789abcdef01234567"
	h9       = "8123456789abcdef0123456789abcdef01234567"
	h64      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	md5Token = "0123456789abcdef0123456789abcdef"
	ts       = "2026-01-02T03:04:05.000"
)

// One base per artifact type, plus a delta manifest, each as its cards in
// order. Every base uses the cards its type allows in the form Marshal
// writes, so TestArtifactBases can require an exact re-encoding.
var (
	manifestCards = []string{
		"C base\\smanifest\n",
		"D " + ts + "\n",
		"F a.txt " + h1 + "\n",
		"F b/c.txt " + h2 + " x\n",
		"F d.txt " + h3 + " w old.txt\n",
		"P " + h4 + " " + h5 + "\n",
		"Q +" + h6 + " " + h7 + "\n",
		"R " + md5Token + "\n",
		"T *branch * trunk\n",
		"T +sym-trunk *\n",
		"U alice\n",
	}
	deltaCards = []string{
		"B " + h1 + "\n",
		"C base\\sdelta\n",
		"D " + ts + "\n",
		"F a.txt " + h2 + "\n",
		"F gone.txt\n",
		"P " + h1 + "\n",
		"U alice\n",
	}
	clusterCards = []string{"M " + h1 + "\n", "M " + h2 + "\n"}
	controlCards = []string{
		"D " + ts + "\n",
		"T +bgcolor " + h1 + " blue\n",
		"T -sym-old " + h2 + "\n",
		"U alice\n",
	}
	wikiCards = []string{
		"C edit\\scomment\n",
		"D " + ts + "\n",
		"L Home\\sPage\n",
		"N text/x-markdown\n",
		"P " + h1 + "\n",
		"U alice\n",
		"W 12\nhello\nworld\n\n",
	}
	ticketCards = []string{
		"D " + ts + "\n",
		"J +comment more\\stext\n",
		"J status Open\n",
		"K " + h1 + "\n",
		"U alice\n",
	}
	attachmentCards = []string{
		"A report.txt " + h1 + " " + h2 + "\n",
		"C attached\\sa\\sreport\n",
		"D " + ts + "\n",
		"N text/plain\n",
		"U alice\n",
	}
	eventCards = []string{
		"C a\\stechnote\n",
		"D " + ts + "\n",
		"E 2026-01-02T03:04:05 " + h1 + "\n",
		"N text/x-markdown\n",
		"P " + h2 + "\n",
		"T +bgcolor * red\n",
		"U alice\n",
		"W 4\nnote\n",
	}
	forumCards = []string{
		"D " + ts + "\n",
		"G " + h1 + "\n",
		"I " + h2 + "\n",
		"N text/x-markdown\n",
		"P " + h3 + "\n",
		"U alice\n",
		"W 5\nreply\n",
	}
)

// The bases as artifact bodies, without their Z card.
var (
	baseManifest   = strings.Join(manifestCards, "")
	baseDelta      = strings.Join(deltaCards, "")
	baseCluster    = strings.Join(clusterCards, "")
	baseControl    = strings.Join(controlCards, "")
	baseWiki       = strings.Join(wikiCards, "")
	baseTicket     = strings.Join(ticketCards, "")
	baseAttachment = strings.Join(attachmentCards, "")
	baseEvent      = strings.Join(eventCards, "")
	baseForum      = strings.Join(forumCards, "")
)

// artifactBase is one base and the draft's card table for its type
// (Section 5.2, Table 4).
type artifactBase struct {
	name     string
	cards    []string
	goType   deck.ArtifactType
	allowed  string
	required string
}

var artifactBases = []artifactBase{
	{"manifest", manifestCards, deck.Checkin, "BCDFNPQRTUZ", "DZ"},
	{"delta", deltaCards, deck.Checkin, "BCDFNPQRTUZ", "DZ"},
	{"cluster", clusterCards, deck.Cluster, "MZ", "MZ"},
	{"control", controlCards, deck.Control, "DTUZ", "DTUZ"},
	{"wiki", wikiCards, deck.Wiki, "CDLNPUWZ", "DLUWZ"},
	{"ticket", ticketCards, deck.Ticket, "DJKUZ", "DJKUZ"},
	{"attachment", attachmentCards, deck.Attachment, "ACDNUZ", "ADZ"},
	{"event", eventCards, deck.Event, "CDENPTUWZ", "DEWZ"},
	{"forum", forumCards, deck.ForumPost, "DGHINPUWZ", "DUWZ"},
}

// edit replaces the one occurrence of old in base. It panics unless old
// occurs exactly once, so a case cannot silently edit the wrong place.
func edit(base, old, replacement string) string {
	if n := strings.Count(base, old); n != 1 {
		panic(fmt.Sprintf("conformance: edit: %q occurs %d times in the base", old, n))
	}
	return strings.Replace(base, old, replacement, 1)
}

// conforms runs one artifact case through fossil(1), when present, and
// deck.Parse. fossil goes first: a case whose expectation fossil
// contradicts is wrong whatever go-libfossil does.
func (c artifactCase) conforms(ref *fossilReference) error {
	if ref != nil {
		if err := c.fossilAgrees(ref); err != nil {
			return err
		}
	}
	if !c.accept {
		return c.rejectedByRule()
	}
	d, err := deck.Parse(c.input)
	if err != nil {
		return expectf("deck.Parse rejected %q: %v", c.input, err)
	}
	if c.check != nil {
		if err := runCheck(c.check, d); err != nil {
			return err
		}
	}
	if c.nonCanonical {
		return nil
	}
	encoded, err := d.Marshal()
	if err != nil {
		return expectf("Marshal: %v", err)
	}
	if !bytes.Equal(encoded, c.input) {
		return expectf("Marshal(Parse(input)) = %q, want the input %q", encoded, c.input)
	}
	return nil
}

// rejectedByRule checks that deck.Parse rejects the input, and that the Z
// check did not do it for the rule the case is about.
func (c artifactCase) rejectedByRule() error {
	if !c.zCase {
		if err := deck.VerifyZ(c.input); err != nil {
			return expectf("rejected by the Z check, not by the rule: %v", err)
		}
	}
	if _, err := deck.Parse(c.input); err == nil {
		return expectf("deck.Parse accepted %q, want rejection", c.input)
	}
	return nil
}

// fossilAgrees checks fossil's verdict against the case: the same as accept,
// or the opposite when the case says why it departs from fossil.
func (c artifactCase) fossilAgrees(ref *fossilReference) error {
	accepted, reason, err := ref.parseArtifact(c.input)
	if err != nil {
		return err
	}
	expected := c.accept
	if c.fossil != "" {
		expected = !c.accept
	}
	if accepted == expected {
		if !accepted && !strings.Contains(reason, c.fossilSays) {
			return expectf("fossil(1) rejects for %q, not %q", reason, c.fossilSays)
		}
		return nil
	}
	if c.fossil != "" {
		return expectf("fossil(1) now agrees with the case (accepted=%v); remove fossil: %q",
			accepted, c.fossil)
	}
	if accepted {
		return expectf("fossil(1) accepts the input; the case expects rejection")
	}
	return expectf("fossil(1) rejects the input (%s); the case expects acceptance", reason)
}

func TestArtifactBases(t *testing.T) {
	ref := newFossilReference(t)
	for _, b := range artifactBases {
		t.Run(b.name, func(t *testing.T) {
			input := withZ(strings.Join(b.cards, ""))
			if ref != nil {
				accepted, reason, err := ref.parseArtifact(input)
				if err != nil || !accepted {
					t.Fatalf("fossil(1) rejects the %s base: %v %s", b.name, err, reason)
				}
			}
			d, err := deck.Parse(input)
			if err != nil {
				t.Fatalf("deck.Parse rejects the %s base: %v", b.name, err)
			}
			if d.Type != b.goType {
				t.Fatalf("deck.Parse typed the %s base %d, want %d", b.name, d.Type, b.goType)
			}
			encoded, err := d.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, input) {
				t.Fatalf("Marshal(Parse(base)) = %q, want %q", encoded, input)
			}
		})
	}
}

func TestArtifactParse(t *testing.T) {
	ref := newFossilReference(t)
	for _, c := range artifactCases() {
		check(t, c.Case, func() error { return c.conforms(ref) })
	}
}

// artifactCases is every deck.Parse case: the hand-written tables, then the
// ones generated from the bases.
func artifactCases() []artifactCase {
	var all []artifactCase
	for _, table := range [][]artifactCase{
		framingCases, signatureCases, grammarCases, escapeCases, orderCases,
		pathnameCases(), wikiNameCases(), reservedCases, timestampCases(),
		cardCases, typeCases(), checksumCases,
		duplicateCases(), arityCases(), hashCases(), legalityCases(),
	} {
		all = append(all, table...)
	}
	for i := range all {
		all[i].Gap = artifactGaps[all[i].ID]
	}
	return all
}

// artifactSuiteCases is what the coverage test audits for this suite.
func artifactSuiteCases() []Case {
	var all []Case
	for _, c := range artifactCases() {
		all = append(all, c.Case)
	}
	for _, c := range apiCases() {
		all = append(all, c.Case)
	}
	return all
}

// accepted builds an acceptance case.
func accepted(id, req, body string, check func(*deck.Deck) error) artifactCase {
	c := artifactCase{Case: Case{ID: id, Req: req}, input: withZ(body), accept: true}
	c.check = check
	return c
}

// rejected builds a rejection case.
func rejected(id, req, body string) artifactCase {
	return artifactCase{Case: Case{ID: id, Req: req}, input: withZ(body)}
}

// loose is accepted for inputs Marshal writes differently.
func loose(id, req, body string, check func(*deck.Deck) error) artifactCase {
	c := accepted(id, req, body, check)
	c.nonCanonical = true
	return c
}

// want compares one parsed field with the value the draft gives it.
func want(field string, got, expected any) error {
	if fmt.Sprint(got) == fmt.Sprint(expected) {
		return nil
	}
	return expectf("%s = %q, want %q", field, fmt.Sprint(got), fmt.Sprint(expected))
}

// wantType checks the type deck.Parse assigned.
func wantType(expected deck.ArtifactType) func(*deck.Deck) error {
	return func(d *deck.Deck) error { return want("Type", int(d.Type), int(expected)) }
}

// runCheck runs a field check, turning an index out of range on a deck
// with fewer cards than the draft gives it into a reported problem.
func runCheck(check func(*deck.Deck) error, d *deck.Deck) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = expectf("checking the parsed deck: %v", r)
		}
	}()
	return check(d)
}
