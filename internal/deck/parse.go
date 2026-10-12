package deck

import (
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/internal/hash"
)

// isHexToken reports whether s is non-empty and consists solely of the
// lowercase hex digits an artifact hash is built from. §4.7.16 rejects a
// T-card name that is entirely hex because it is indistinguishable from a
// hash target.
func isHexToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHexDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHexDigit {
			return false
		}
	}
	return true
}

// manifestScanner tokenizes clearsign-stripped artifact content card by card,
// following fossil's next_token/next_card (src/manifest.c). Tokens on a card
// are separated by a single space; a card ends at LF. Tab and CR are ordinary
// token bytes, never separators, so a card value may carry them -- the only
// thing a token cannot contain is a space or an LF. The zero value is not
// usable: start a scan with atEol true at buf[0].
type manifestScanner struct {
	buf   []byte
	pos   int  // index of the next unread byte
	atEol bool // true when positioned at the first byte of a card
}

// nextCard returns the next card's type letter and advances past the letter
// and the single byte that follows it. A space after the letter means
// arguments follow (atEol cleared); an LF means a bare card with none (atEol
// kept). Any other following byte -- or no more cards -- yields 0, leaving pos
// where the caller's §4.2 residue check will see the unconsumed bytes.
func (s *manifestScanner) nextCard() byte {
	if !s.atEol || s.pos >= len(s.buf) {
		return 0
	}
	c := s.buf[s.pos]
	switch {
	case s.pos+1 < len(s.buf) && s.buf[s.pos+1] == ' ':
		s.pos += 2
		s.atEol = false
	case s.pos+1 < len(s.buf) && s.buf[s.pos+1] == '\n':
		s.pos += 2
		s.atEol = true
	default:
		return 0
	}
	return c
}

// nextToken returns the next token on the current card and whether one was
// present. A token runs to the next space or LF; the delimiter is consumed,
// and an LF ends the card (atEol set). A card with no arguments, or one whose
// tokens are exhausted, yields ok=false (fossil's next_token returning NULL).
func (s *manifestScanner) nextToken() (token string, ok bool) {
	if s.atEol {
		return "", false
	}
	start := s.pos
	for s.pos < len(s.buf) && s.buf[s.pos] != ' ' && s.buf[s.pos] != '\n' {
		s.pos++
	}
	token = string(s.buf[start:s.pos])
	if s.pos < len(s.buf) && s.buf[s.pos] == '\n' {
		s.atEol = true
	}
	s.pos++ // consume the delimiter (space or LF)
	return token, true
}

// cardTokens drains and returns every token of the current card. §4.2 with
// its empty-trailing-token erratum: a token between two others may not be
// empty -- two spaces in a row -- but the last token may be, carrying a
// present-but-empty value (an empty comment, J value, N mimetype or T value).
// fossil has no such explicit rule; its per-card validators reject an interior
// empty token at whichever field it lands on, and this is their shared form.
func (s *manifestScanner) cardTokens() ([]string, error) {
	var tokens []string
	for {
		tok, ok := s.nextToken()
		if !ok {
			break
		}
		tokens = append(tokens, tok)
	}
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] == "" {
			return nil, fmt.Errorf("empty token between two others")
		}
	}
	return tokens, nil
}

// parseW reads a W card's body. The size is a token fossil parses as its
// leading decimal digits (W 12x is 12, W abc is 0), which must be the card's
// only token; then exactly that many raw bytes, which are never decoded,
// followed by a required LF. Mirrors the W case of manifest_parse.
func (s *manifestScanner) parseW(d *Deck) error {
	if d.W != nil {
		return fmt.Errorf("more than one W-card")
	}
	sizeTok, ok := s.nextToken()
	if !ok {
		return fmt.Errorf("missing size on W-card")
	}
	if !s.atEol {
		return fmt.Errorf("no content after W-card")
	}
	var size uint64
	for i := 0; i < len(sizeTok) && sizeTok[i] >= '0' && sizeTok[i] <= '9'; i++ {
		next := size*10 + uint64(sizeTok[i]-'0')
		if next < size {
			return fmt.Errorf("size overflow on W-card")
		}
		size = next
	}
	// fossil requires the body and its terminating LF to leave at least one
	// more byte before the end, so a W card is never the artifact's last card.
	if size+1 >= uint64(len(s.buf)-s.pos) {
		return fmt.Errorf("not enough content after W-card")
	}
	n := int(size)
	if s.buf[s.pos+n] != '\n' {
		return fmt.Errorf("W-card content not newline terminated")
	}
	// Copy so the deck does not alias the input buffer.
	d.W = append([]byte{}, s.buf[s.pos:s.pos+n]...)
	s.pos += n + 1
	s.atEol = true
	return nil
}

// Parse reads an artifact as a pipeline of stages: frame the raw blob (§4.1
// framing, wrapper strip, §4.1.1 first card, §6.2 Z checksum), scan the cards
// (§4.2), then determine the type (§5.3). Each stage is a helper so Parse
// reads as the pipeline and no stage runs before the one it depends on.
func Parse(data []byte) (*Deck, error) {
	if data == nil {
		panic("deck.Parse: data must not be nil")
	}
	content, err := frameArtifact(data)
	if err != nil {
		return nil, fmt.Errorf("deck.Parse: %w", err)
	}
	d := &Deck{}
	if err := scanCards(content, d); err != nil {
		return nil, fmt.Errorf("deck.Parse: %w", err)
	}
	d.Type = inferType(d)
	return d, nil
}

// frameArtifact validates an artifact's framing and returns the inner content
// the card scanner reads. It mirrors the order fossil applies these checks in,
// up front before the card loop (src/manifest.c): §4.1 (non-empty, ends in LF)
// on the raw blob, the PGP/SSH clearsign wrapper stripped, §4.1.1 (the first
// card is an upper-case letter and a space), then the §6.2 Z self-checksum.
// The clearsign strip is a byte-for-byte identity on the common unsigned case,
// and the artifact's content-addressed hash is taken over the raw blob
// elsewhere, so this never changes which UUID an artifact resolves to.
func frameArtifact(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("zero-length artifact")
	}
	if data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("artifact not terminated with a newline")
	}
	content := stripClearsign(data)
	if len(content) < 10 || content[0] < 'A' || content[0] > 'Z' || content[1] != ' ' {
		return nil, fmt.Errorf("line 1 not recognized")
	}
	// verifyZ reports a present-but-wrong checksum; an absent Z card is left to
	// the card loop and the required-card rule. The Z card itself is parsed
	// below as an ordinary card, so the whole content is scanned.
	if err := verifyZ(content); err != nil {
		return nil, err
	}
	return content, nil
}

// scanCards runs the §4.2 card loop over content, filling d. Every structural
// artifact type requires a Z card (§6.2 / §5.2), so a scan reaching the end
// without one is rejected here; the full per-type required-card table is
// applied in Parse's later stages.
func scanCards(content []byte, d *Deck) error {
	s := &manifestScanner{buf: content, atEol: true}
	var lastCard byte
	var seen seenCards
	var sawZ bool

	for {
		card := s.nextCard()
		if card == 0 {
			break
		}
		if card == 'Z' {
			sawZ = true
		}

		// §4.5.1 single-occurrence guard, kept separate from the ordering
		// check below because that one permits equal consecutive letters.
		if err := seen.claim(card); err != nil {
			return err
		}

		// §4.4: cards ascend, with the one legacy technote N-after-P exception.
		if card < lastCard && !isLegacyTechnoteNAfterP(d, card, lastCard) {
			return fmt.Errorf("card '%c' out of order (after '%c')", card, lastCard)
		}
		lastCard = card

		if card == 'W' {
			if err := s.parseW(d); err != nil {
				return err
			}
			continue
		}

		tokens, err := s.cardTokens()
		if err != nil {
			return err
		}
		if err := parseCard(d, card, tokens); err != nil {
			return err
		}
	}

	// §4.2: any byte left once no card can be read -- a blank line, trailing
	// residue, or an extra token that stopped the scan early -- is an error
	// (fossil's "extra characters at end of card").
	if s.pos < len(s.buf) {
		return fmt.Errorf("extra characters at end of card")
	}

	// §6.2 / §5.2: an artifact that reaches here without a Z card is not well
	// formed. verifyZ treated the absent Z card as "continue"; this is where
	// its absence is judged.
	if !sawZ {
		return fmt.Errorf("Z-card missing")
	}
	return nil
}

// isLegacyTechnoteNAfterP reports whether an N-card following a P-card is the
// one out-of-order sequence Fossil accepts.
//
// Cards are otherwise required to be in lexicographical order, but a technote
// generator bug shipped before 2021-02-10 emitted the P-card ahead of the
// N-card, and the ordering check that would have caught it was itself broken
// until the same date. Fossil keeps accepting those artifacts for historical
// compatibility (src/manifest.c, tickets 15d04de574383d61 and 5e67a7f4041a36ad)
// and so must this parser: rejecting them dropped real technote revisions --
// along with their event-<id> and bgcolor tags -- out of crosslink (issue #193).
//
// The exception is narrow on purpose: only N after P, and only once the E-card
// that makes the artifact a technote has already been seen.
func isLegacyTechnoteNAfterP(d *Deck, card, lastCard byte) bool {
	return card == 'N' && lastCard == 'P' && d.E != nil
}

func parseCard(d *Deck, card byte, tokens []string) error {
	if d == nil {
		panic("deck.parseCard: d must not be nil")
	}
	switch card {
	case 'A':
		return parseACard(d, tokens)
	case 'B':
		return setStringCard(tokens, &d.B, "hash on B-card", false)
	case 'C':
		return setStringCard(tokens, &d.C, "comment on C-card", true)
	case 'D':
		return parseDCard(d, tokens)
	case 'E':
		return parseECard(d, tokens)
	case 'F':
		return parseFCard(d, tokens)
	case 'G':
		return setStringCard(tokens, &d.G, "hash on G-card", false)
	case 'H':
		return setStringCard(tokens, &d.H, "title on H-card", true)
	case 'I':
		return setStringCard(tokens, &d.I, "hash on I-card", false)
	case 'J':
		return parseJCard(d, tokens)
	case 'K':
		return setStringCard(tokens, &d.K, "hash on K-card", false)
	case 'L':
		return setStringCard(tokens, &d.L, "title on L-card", true)
	case 'M':
		return parseMCard(d, tokens)
	case 'N':
		return setStringCard(tokens, &d.N, "mimetype on N-card", false)
	case 'P':
		return parsePCard(d, tokens)
	case 'Q':
		return parseQCard(d, tokens)
	case 'R':
		return setStringCard(tokens, &d.R, "checksum on R-card", false)
	case 'T':
		return parseTCard(d, tokens)
	case 'U':
		return parseUCard(d, tokens)
	case 'Z':
		return parseZCard(tokens)
	default:
		return fmt.Errorf("unknown card '%c'", card)
	}
}

// setStringCard stores a single-token card value. A bare card with no token
// at all is "missing"; a present-but-empty token -- a card line ending in a
// space, per the §4.2 erratum -- is kept distinct from that. decode applies
// §4.3 fossil-decoding to the fields §4.3.1 lists as decoded (C, H, L).
func setStringCard(tokens []string, dst *string, what string, decode bool) error {
	if len(tokens) == 0 {
		return fmt.Errorf("missing %s", what)
	}
	if len(tokens) > 1 {
		return fmt.Errorf("extra characters after %s", what)
	}
	value := tokens[0]
	if decode {
		value = FossilDecode(value)
	}
	*dst = value
	return nil
}

func parseACard(d *Deck, tokens []string) error {
	// §4.2.1 / Table 2: filename, target, optional source -- three tokens at
	// most. An extra token is "extra characters at end of card".
	if len(tokens) < 2 || len(tokens) > 3 {
		return fmt.Errorf("A-card takes a filename, a target and an optional source")
	}
	ac := &AttachmentCard{Filename: FossilDecode(tokens[0]), Target: tokens[1]}
	if len(tokens) >= 3 {
		ac.Source = tokens[2]
	}
	d.A = ac
	return nil
}

func parseDCard(d *Deck, tokens []string) error {
	if len(tokens) != 1 {
		return fmt.Errorf("D-card takes exactly one timestamp")
	}
	t, err := parseTimestamp(tokens[0])
	if err != nil {
		return fmt.Errorf("D-card: %w", err)
	}
	d.D = t
	return nil
}

func parseECard(d *Deck, tokens []string) error {
	// §4.2.1: exactly a timestamp and an event id.
	if len(tokens) != 2 {
		return fmt.Errorf("E-card takes a timestamp and a uuid")
	}
	t, err := parseTimestamp(tokens[0])
	if err != nil {
		return fmt.Errorf("E-card: %w", err)
	}
	d.E = &EventCard{Date: t, UUID: tokens[1]}
	return nil
}

func parseFCard(d *Deck, tokens []string) error {
	// §4.2.1 / Table 2: filename, optional hash, permission and old-name --
	// four tokens at most.
	if len(tokens) == 0 || len(tokens) > 4 {
		return fmt.Errorf("F-card takes a filename and up to three more fields")
	}
	fc := FileCard{Name: FossilDecode(tokens[0])}
	if len(tokens) >= 2 {
		fc.UUID = tokens[1]
	}
	if len(tokens) >= 3 {
		fc.Perm = tokens[2]
	}
	if len(tokens) >= 4 {
		fc.OldName = FossilDecode(tokens[3])
	}
	// §4.5.2: strictly ascending by decoded filename. Decoding has already
	// happened above, so the check sees the post-decode name the merge walk
	// and baseline binary search of §8.2 will later rely on.
	if n := len(d.F); n > 0 {
		if err := requireAscending('F', d.F[n-1].Name, fc.Name); err != nil {
			return err
		}
	}
	d.F = append(d.F, fc)
	return nil
}

func parseMCard(d *Deck, tokens []string) error {
	if len(tokens) != 1 {
		return fmt.Errorf("M-card takes exactly one hash")
	}
	m := tokens[0]
	// §4.5.2: strictly ascending by hash.
	if n := len(d.M); n > 0 {
		if err := requireAscending('M', d.M[n-1], m); err != nil {
			return err
		}
	}
	d.M = append(d.M, m)
	return nil
}

func parseJCard(d *Deck, tokens []string) error {
	// §4.2.1: a field name and an optional value -- two tokens at most.
	if len(tokens) == 0 || len(tokens) > 2 {
		return fmt.Errorf("J-card takes a name and an optional value")
	}
	// §4.7.8: the value is escape-decoded, the field name is stored verbatim
	// (canonical manifest.c defossilizes only zValue and compares the raw
	// zName).
	jf := TicketField{Name: tokens[0]}
	if len(tokens) >= 2 {
		jf.Value = FossilDecode(tokens[1])
	}
	// §4.5.2: strictly ascending by field name.
	if n := len(d.J); n > 0 {
		if err := requireAscending('J', d.J[n-1].Name, jf.Name); err != nil {
			return err
		}
	}
	d.J = append(d.J, jf)
	return nil
}

func parsePCard(d *Deck, tokens []string) error {
	// Repeatable, and neither sorted nor deduplicated (§4.5.2): P order is
	// semantic, primary parent first. Appending -- not assigning -- is what
	// makes a second P card add parents rather than replace them (§4.5.1). A
	// bare P card carries no parents, which is the initial empty baseline.
	d.P = append(d.P, tokens...)
	return nil
}

func parseQCard(d *Deck, tokens []string) error {
	// §4.2.1: a signed target and an optional baseline -- two tokens at most.
	if len(tokens) == 0 || len(tokens) > 2 || len(tokens[0]) < 1 {
		return fmt.Errorf("Q-card takes a signed target and an optional baseline")
	}
	cp := CherryPick{IsBackout: tokens[0][0] == '-'}
	cp.Target = tokens[0][1:]
	if len(tokens) >= 2 {
		cp.Baseline = tokens[1]
	}
	d.Q = append(d.Q, cp)
	return nil
}

func parseTCard(d *Deck, tokens []string) error {
	if len(tokens) == 0 || len(tokens[0]) < 1 {
		return fmt.Errorf("T-card too short")
	}
	nameToken := tokens[0]
	// §4.7.16: the first character of the name token MUST be +, - or *.
	// Rejecting anything else also keeps the sign a single byte, so the
	// ordering key assembled from it cannot silently gain a second byte
	// through UTF-8 expansion.
	switch TagType(nameToken[0]) {
	case TagSingleton, TagPropagating, TagCancel:
	default:
		return fmt.Errorf("T-card name must begin with '+', '-' or '*', got %q", nameToken[:1])
	}
	// §4.2.1: a signed name, a target and an optional value -- three tokens.
	if len(tokens) < 2 || len(tokens) > 3 {
		return fmt.Errorf("T-card takes a name, a target and an optional value")
	}
	tc := TagCard{Type: TagType(nameToken[0])}
	// §4.7.16: the name and value tokens are escape-decoded (canonical
	// defossilizes both), while the target token is stored verbatim -- never
	// decoded on the way in, so it is never encoded on the way out. Decoding
	// the value matters on ordinary input: `fossil branch new 'my branch'`
	// writes `T *branch * my\sbranch`, and leaving that raw names the branch
	// literally "my\sbranch" all the way through crosslink.
	tc.Name = FossilDecode(nameToken[1:])
	tc.UUID = tokens[1]
	if len(tokens) >= 3 {
		tc.Value = FossilDecode(tokens[2])
	}
	// §4.7.16: a name that after the sign is entirely hex digits is ambiguous
	// with a hash, so reject it. The escape-decoded name is used; an all-hex
	// token carries no escapes, so decoding leaves it unchanged.
	if isHexToken(tc.Name) {
		return fmt.Errorf("T-card name %q must not be entirely hexadecimal", tc.Name)
	}
	// §4.7.16: the target must be a valid artifact hash or the literal '*'.
	// hash.IsValidHash panics on an empty string, so the '*' and non-empty
	// checks come first.
	validTarget := tc.UUID == "*"
	if !validTarget && tc.UUID != "" {
		validTarget = hash.IsValidHash(tc.UUID)
	}
	if !validTarget {
		return fmt.Errorf("T-card target %q is neither a valid artifact hash nor '*'", tc.UUID)
	}
	if n := len(d.T); n > 0 {
		if err := requireTagAscending(d.T[n-1], tc); err != nil {
			return err
		}
	}
	d.T = append(d.T, tc)
	return nil
}

// requireTagAscending enforces the two-level T rule of §4.5.2: ascending by
// tag name, then by target hash, with equal names requiring a strictly
// increasing hash. Per §4.7.16 the primary key is the decoded full name
// token including its leading sign character; tagOrderKey builds it.
func requireTagAscending(previous, current TagCard) error {
	previousName := tagOrderKey(previous)
	currentName := tagOrderKey(current)
	switch cmp := Compare(currentName, previousName); {
	case cmp > 0:
		return nil
	case cmp < 0:
		return fmt.Errorf("T-card %q out of order after %q", currentName, previousName)
	default:
		if Compare(current.UUID, previous.UUID) > 0 {
			return nil
		}
		return fmt.Errorf("T-card %q target %q out of order after %q",
			currentName, current.UUID, previous.UUID)
	}
}

// parseUCard resolves the U-card's decoded value the same way fossil's
// own parser does (src/manifest.c:1008-1016): a present-but-empty U-card
// becomes the literal "anonymous". A wholly absent U-card never calls
// this function at all, so d.U stays nil — the third state a bare string
// field cannot represent. Resolving here, once, means every downstream
// crosslink call site can bind d.U directly and get the right SQL value
// (NULL, "anonymous", or the login) without repeating this logic.
func parseUCard(d *Deck, tokens []string) error {
	// §4.2.1: an optional login -- one token at most.
	if len(tokens) > 1 {
		return fmt.Errorf("U-card takes an optional login")
	}
	var user string
	if len(tokens) > 0 {
		user = FossilDecode(tokens[0])
	}
	if user == "" {
		user = "anonymous"
	}
	d.U = &user
	return nil
}

// parseZCard validates the trailing Z card as an ordinary card. §4.7.19 fixes
// its payload at exactly 32 hexadecimal characters (never decoded) and it
// carries exactly one token; an extra token is "extra characters" the same as
// on any other card. The Z line's integrity is checked separately by verifyZ.
func parseZCard(tokens []string) error {
	if len(tokens) != 1 {
		return fmt.Errorf("Z-card takes exactly one token")
	}
	token := tokens[0]
	if len(token) != 32 {
		return fmt.Errorf("Z-card checksum must be 32 characters, got %d", len(token))
	}
	for i := 0; i < len(token); i++ {
		if !isHexDigit(token[i]) {
			return fmt.Errorf("Z-card checksum is not hexadecimal: %q", token)
		}
	}
	return nil
}

func parseTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	s = strings.Replace(s, "t", "T", 1)
	for _, layout := range []string{
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse timestamp %q", s)
}

func inferType(d *Deck) ArtifactType {
	switch {
	case len(d.M) > 0:
		return Cluster
	case d.G != "" || d.H != "" || d.I != "":
		return ForumPost
	case d.A != nil:
		return Attachment
	case d.K != "":
		return Ticket
	case d.L != "":
		return Wiki
	case d.E != nil:
		return Event
	// Canonical Fossil settles the remaining ambiguity -- check-in versus
	// control artifact, both of which are D/T/U/Z-shaped -- on whether the
	// artifact describes a tree position: a comment, files, or parents
	// (src/manifest.c, "If the artifact type has not yet been determined").
	// The C-card is the part that is easy to miss, and it matters: `fossil
	// merge --integrate` writes a check-in that has a comment and a `T +closed`
	// card but no F-cards and no P-card, and reading those as control
	// artifacts lost their tags (issue #193). The R-card is not part of
	// canonical's test but implies a file tree, so it stays here.
	case d.C != "" || len(d.F) > 0 || len(d.P) > 0 || d.R != "":
		return Checkin
	case len(d.T) > 0:
		return Control
	default:
		return Checkin
	}
}
