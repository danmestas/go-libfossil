package deck

import (
	"fmt"
	"strings"
)

// isHexToken reports whether s is non-empty and consists solely of hex
// digits of either case. §4.7.16 rejects a T-card name that is entirely hex
// because it is indistinguishable from a hash target; fossil's check
// (validate16 on the name after its sign) is case-insensitive, so an
// upper-case all-hex name such as "ABC" is rejected the same as "abc".
func isHexToken(s string) bool {
	return s != "" && isHexAnyCase(s)
}

// isHexAnyCase reports whether every byte of s is a base-16 digit in either
// case. §3.5 / §4.7.1: artifact-hash tokens are case-insensitive hexadecimal.
func isHexAnyCase(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// validArtifactHash reports whether s is a syntactically valid artifact-hash
// token per §4.7.1: exactly 40 or 64 case-insensitive hexadecimal characters.
// Unlike hash.IsValidHash it never panics, so it is safe on an empty or
// mangled token straight off the wire.
func validArtifactHash(s string) bool {
	return (len(s) == 40 || len(s) == 64) && isHexAnyCase(s)
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
	seenMask, err := scanCards(content, d)
	if err != nil {
		return nil, fmt.Errorf("deck.Parse: %w", err)
	}
	// §5.3: determine the artifact type, then apply §5.2 card legality (the
	// cards this type allows and requires, which includes the universal Z
	// card) and the §5.4 per-type semantic checks.
	d.Type = inferType(d)
	if err := checkType(d, seenMask); err != nil {
		return nil, fmt.Errorf("deck.Parse: %w", err)
	}
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

// scanCards runs the §4.2 card loop over content, filling d, and returns the
// mask of card types it saw (bit letter-'A' per type) for the §5.2 legality
// check Parse applies next -- which, since every type requires a Z card,
// is also what rejects an artifact missing one.
func scanCards(content []byte, d *Deck) (uint32, error) {
	s := &manifestScanner{buf: content, atEol: true}
	var lastCard byte
	var seen seenCards
	var seenMask uint32 // bit (letter-'A') set for every card type seen

	for {
		card := s.nextCard()
		if card == 0 {
			break
		}
		if card >= 'A' && card <= 'Z' {
			seenMask |= 1 << (card - 'A')
		}

		// §4.5.1 single-occurrence guard, kept separate from the ordering
		// check below because that one permits equal consecutive letters.
		if err := seen.claim(card); err != nil {
			return 0, err
		}

		// §4.4: cards ascend, with the one legacy technote N-after-P exception.
		if card < lastCard && !isLegacyTechnoteNAfterP(d, card, lastCard) {
			return 0, fmt.Errorf("card '%c' out of order (after '%c')", card, lastCard)
		}
		lastCard = card

		if card == 'W' {
			if err := s.parseW(d); err != nil {
				return 0, err
			}
			continue
		}

		tokens, err := s.cardTokens()
		if err != nil {
			return 0, err
		}
		if err := parseCard(d, card, tokens); err != nil {
			return 0, err
		}
	}

	// §4.2: any byte left once no card can be read -- a blank line, trailing
	// residue, or an extra token that stopped the scan early -- is an error
	// (fossil's "extra characters at end of card").
	if s.pos < len(s.buf) {
		return 0, fmt.Errorf("extra characters at end of card")
	}
	return seenMask, nil
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
		return setHashCard(tokens, &d.B, "B-card", false)
	case 'C':
		d.cPresent = true
		return setStringCard(tokens, &d.C, "comment on C-card", true)
	case 'D':
		return parseDCard(d, tokens)
	case 'E':
		return parseECard(d, tokens)
	case 'F':
		return parseFCard(d, tokens)
	case 'G':
		return setHashCard(tokens, &d.G, "G-card", false)
	case 'H':
		return setStringCard(tokens, &d.H, "title on H-card", true)
	case 'I':
		return setHashCard(tokens, &d.I, "I-card", false)
	case 'J':
		return parseJCard(d, tokens)
	case 'K':
		// §4.7.1: the K-card ticket id is pinned to the 40-character SHA-1
		// length and never 64, regardless of hash policy.
		return setHashCard(tokens, &d.K, "K-card", true)
	case 'L':
		return parseLCard(d, tokens)
	case 'M':
		return parseMCard(d, tokens)
	case 'N':
		// §4.3.1: the N-card mimetype is escape-decoded.
		d.nPresent = true
		return setStringCard(tokens, &d.N, "mimetype on N-card", true)
	case 'P':
		return parsePCard(d, tokens)
	case 'Q':
		return parseQCard(d, tokens)
	case 'R':
		return setChecksumCard(tokens, &d.R, "R-card")
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

// setHashCard stores a single-token artifact-hash card (B, G, I, K), checking
// the token against the §4.7.1 map: exactly 40 or 64 case-insensitive hex, or
// pinned to 40 for the K-card ticket id. The token is never escape-decoded.
func setHashCard(tokens []string, dst *string, card string, sha1Only bool) error {
	if len(tokens) != 1 {
		return fmt.Errorf("%s takes exactly one hash", card)
	}
	h := tokens[0]
	ok := validArtifactHash(h)
	if sha1Only {
		ok = len(h) == 40 && isHexAnyCase(h)
	}
	if !ok {
		return fmt.Errorf("invalid hash on %s: %q", card, h)
	}
	*dst = h
	return nil
}

// setChecksumCard stores a single-token MD5 checksum card (R). §4.7.1 / §6.1:
// exactly 32 case-insensitive hex, never decoded.
func setChecksumCard(tokens []string, dst *string, card string) error {
	if len(tokens) != 1 {
		return fmt.Errorf("%s takes exactly one checksum", card)
	}
	if token := tokens[0]; len(token) != 32 || !isHexAnyCase(token) {
		return fmt.Errorf("invalid checksum on %s: %q", card, token)
	}
	*dst = tokens[0]
	return nil
}

func parseACard(d *Deck, tokens []string) error {
	// §4.2.1 / Table 2: filename, target, optional source -- three tokens at
	// most. An extra token is "extra characters at end of card".
	if len(tokens) < 2 || len(tokens) > 3 {
		return fmt.Errorf("A-card takes a filename, a target and an optional source")
	}
	// §4.7.2: the filename is escape-decoded, MUST be a simple pathname
	// (§4.6.1), and only its final path component is kept (fossil's file_tail);
	// the target is escape-decoded -- the one §4.3.1 hash carve-out -- and MUST
	// be either an artifact hash or a well-formed wiki name (§4.6.2); the
	// source, when present, is a raw artifact hash.
	name := FossilDecode(tokens[0])
	if !simplePathname(name) {
		return fmt.Errorf("invalid filename on A-card: %q", name)
	}
	target := FossilDecode(tokens[1])
	if !validArtifactHash(target) && !wikiNameWellformed(target) {
		return fmt.Errorf("invalid target on A-card: %q", target)
	}
	ac := &AttachmentCard{Filename: fileTail(name), Target: target}
	if len(tokens) >= 3 {
		ac.Source = tokens[2]
		if !validArtifactHash(ac.Source) {
			return fmt.Errorf("invalid source hash on A-card: %q", ac.Source)
		}
	}
	d.A = ac
	return nil
}

// fileTail returns the final path component of name, the part after the last
// '/'. It mirrors fossil's file_tail for the A-card attachment name.
func fileTail(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
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
	// §4.7.1: the event identifier is an artifact hash.
	if !validArtifactHash(tokens[1]) {
		return fmt.Errorf("invalid hash on E-card: %q", tokens[1])
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
	// §4.6.1: the decoded filename MUST be a simple pathname.
	if !simplePathname(fc.Name) {
		return fmt.Errorf("invalid filename on F-card: %q", fc.Name)
	}
	if len(tokens) >= 2 {
		fc.UUID = tokens[1]
	}
	if len(tokens) >= 3 {
		fc.Perm = tokens[2]
	}
	if len(tokens) >= 4 {
		fc.OldName = FossilDecode(tokens[3])
		// §4.6.1: the decoded old-name MUST be a simple pathname too.
		if !simplePathname(fc.OldName) {
			return fmt.Errorf("invalid old-name on F-card: %q", fc.OldName)
		}
	}
	// §4.7.6 / §4.7.1: a hash is required on every F card of a baseline
	// manifest and must be a valid artifact hash (raw, never decoded). Only a
	// delta manifest -- one with a B card, which sorts before F and so is
	// already seen -- may carry a hashless F card, a file deletion.
	if fc.UUID == "" {
		if d.B == "" {
			return fmt.Errorf("missing hash on F-card %q", fc.Name)
		}
	} else if !validArtifactHash(fc.UUID) {
		return fmt.Errorf("invalid hash on F-card %q: %q", fc.Name, fc.UUID)
	}
	// §4.5.2: strictly ascending by decoded filename. Decoding has already
	// happened above, so the check sees the post-decode name the merge walk
	// and baseline binary search of §8.2 will later rely on.
	if n := len(d.F); n > 0 {
		if err := requireAscending('F', d.F[n-1].Name, fc.Name); err != nil {
			return err
		}
	}
	// §4.6.3: a reserved repository filename is silently dropped, AFTER the
	// sort-order check above, as fossil does (src/manifest.c, nFile--). The
	// dropped card is never stored, so the next card's sort check compares
	// against the previous retained file.
	if IsReservedName(fc.Name) {
		return nil
	}
	d.F = append(d.F, fc)
	return nil
}

func parseLCard(d *Deck, tokens []string) error {
	if len(tokens) != 1 {
		return fmt.Errorf("L-card takes exactly one wiki title")
	}
	// §4.6.2: the decoded wiki title MUST be a well-formed wiki name.
	title := FossilDecode(tokens[0])
	if !wikiNameWellformed(title) {
		return fmt.Errorf("malformed wiki name on L-card: %q", title)
	}
	d.L = title
	return nil
}

func parseMCard(d *Deck, tokens []string) error {
	if len(tokens) != 1 {
		return fmt.Errorf("M-card takes exactly one hash")
	}
	m := tokens[0]
	// §4.7.1: each cluster member is an artifact hash.
	if !validArtifactHash(m) {
		return fmt.Errorf("invalid hash on M-card: %q", m)
	}
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
	// pPresent lets Marshal re-emit that bare "P" even with no parents.
	d.pPresent = true
	// §4.7.1: every present parent token is an artifact hash.
	for _, parent := range tokens {
		if !validArtifactHash(parent) {
			return fmt.Errorf("invalid hash on P-card: %q", parent)
		}
	}
	d.P = append(d.P, tokens...)
	return nil
}

func parseQCard(d *Deck, tokens []string) error {
	// §4.2.1: a signed target and an optional baseline -- two tokens at most.
	if len(tokens) == 0 || len(tokens) > 2 || len(tokens[0]) < 1 {
		return fmt.Errorf("Q-card takes a signed target and an optional baseline")
	}
	// §4.7.14: the first token begins with '+' (cherrypick) or '-' (backout);
	// §4.7.1: the rest of that token, and the optional baseline, are hashes.
	sign := tokens[0][0]
	if sign != '+' && sign != '-' {
		return fmt.Errorf("Q-card must begin with '+' or '-', got %q", tokens[0][:1])
	}
	cp := CherryPick{IsBackout: sign == '-', Target: tokens[0][1:]}
	if !validArtifactHash(cp.Target) {
		return fmt.Errorf("invalid target hash on Q-card: %q", cp.Target)
	}
	if len(tokens) >= 2 {
		cp.Baseline = tokens[1]
		if !validArtifactHash(cp.Baseline) {
			return fmt.Errorf("invalid baseline hash on Q-card: %q", cp.Baseline)
		}
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
		tc.valuePresent = true
	}
	// §4.7.16: a name that after the sign is entirely hex digits is ambiguous
	// with a hash, so reject it. The escape-decoded name is used; an all-hex
	// token carries no escapes, so decoding leaves it unchanged.
	if isHexToken(tc.Name) {
		return fmt.Errorf("T-card name %q must not be entirely hexadecimal", tc.Name)
	}
	// §4.7.16 / §4.7.1: the target must be a valid artifact hash (40 or 64
	// case-insensitive hex) or the literal '*' for a tag on this manifest.
	if tc.UUID != "*" && !validArtifactHash(tc.UUID) {
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

func inferType(d *Deck) ArtifactType {
	switch {
	// The type-setting cards of §5.3, matching fossil's per-card `p->type`
	// assignments (src/manifest.c): M sets cluster, G/H/I forum, A attachment,
	// J/K ticket, L wiki, E event, and B/F/Q/R manifest.
	case len(d.M) > 0:
		return Cluster
	case d.G != "" || d.H != "" || d.I != "":
		return ForumPost
	case d.A != nil:
		return Attachment
	case len(d.J) > 0 || d.K != "":
		return Ticket
	case d.L != "":
		return Wiki
	case d.E != nil:
		return Event
	case d.B != "" || len(d.F) > 0 || len(d.Q) > 0 || d.R != "":
		return Checkin
	// No type-setting card: fossil then settles check-in versus control, both
	// D/T/U/Z-shaped, on whether the artifact describes a tree position -- a
	// comment or a parent (src/manifest.c:1076, "If the artifact type has not
	// yet been determined"). fossil keys on card presence (p->zComment != 0,
	// p->nParent > 0), not a non-empty value, so a present-but-empty C card
	// still makes it a check-in -- hence d.cPresent, not d.C != "". The C-card
	// is the easy one to miss, and it matters: `fossil merge --integrate`
	// writes a check-in with a comment and a `T +closed` card but no F or P
	// card, and reading that as a control artifact lost its tags (issue #193).
	// The parent test stays count-based (len(d.P) > 0), matching p->nParent:
	// a bare P card with no parents does not make a control a check-in.
	case d.cPresent || len(d.P) > 0:
		return Checkin
	// Otherwise it is a control artifact: a D/T/U/Z artifact with no comment or
	// parent -- including one with only tags -- is a control artifact, not an
	// empty check-in.
	default:
		return Control
	}
}
