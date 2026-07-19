package deck

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func Parse(data []byte) (*Deck, error) {
	if data == nil {
		panic("deck.Parse: data must not be nil")
	}
	if err := VerifyZ(data); err != nil {
		return nil, fmt.Errorf("deck.Parse: %w", err)
	}

	// body is safe: VerifyZ above guarantees len(data) >= 35.
	body := data[:len(data)-35]
	d := &Deck{}
	var lastCard byte
	var seen seenCards
	reader := bytes.NewReader(body)

	for reader.Len() > 0 {
		line, err := readLine(reader)
		if err != nil || len(line) == 0 {
			continue
		}

		card := line[0]

		// §4.5.1 occurrence classes. The card-type ordering check below
		// permits equal consecutive letters, so the duplicate guard for
		// single-occurrence types has to be separate from it. W is
		// single-occurrence too, so the claim precedes the W branch.
		if err := seen.claim(card); err != nil {
			return nil, fmt.Errorf("deck.Parse: %w", err)
		}

		if card == 'W' {
			if card < lastCard {
				return nil, fmt.Errorf("deck.Parse: card 'W' out of order (after '%c')", lastCard)
			}
			lastCard = card
			sizeStr := strings.TrimSpace(line[2:])
			size, err := strconv.Atoi(sizeStr)
			if err != nil {
				return nil, fmt.Errorf("deck.Parse: bad W size: %w", err)
			}
			content := make([]byte, size)
			n, readErr := reader.Read(content)
			if readErr != nil && n != size {
				return nil, fmt.Errorf("deck.Parse: W content read: %w", readErr)
			}
			if n != size {
				return nil, fmt.Errorf("deck.Parse: W content: got %d, want %d", n, size)
			}
			reader.ReadByte() // trailing newline
			d.W = content
			continue
		}

		if card < lastCard {
			return nil, fmt.Errorf("deck.Parse: card '%c' out of order (after '%c')", card, lastCard)
		}
		lastCard = card

		if len(line) < 2 || line[1] != ' ' {
			return nil, fmt.Errorf("deck.Parse: malformed: %q", line)
		}
		args := line[2:]
		if err := parseCard(d, card, args); err != nil {
			return nil, fmt.Errorf("deck.Parse: %w", err)
		}
	}

	d.Type = inferType(d)
	return d, nil
}

func readLine(r *bytes.Reader) (string, error) {
	var b strings.Builder
	for {
		c, err := r.ReadByte()
		if err != nil {
			return b.String(), nil
		}
		if c == '\n' {
			return b.String(), nil
		}
		b.WriteByte(c)
	}
}

func parseCard(d *Deck, card byte, args string) error {
	if d == nil {
		panic("deck.parseCard: d must not be nil")
	}
	switch card {
	case 'A':
		return parseACard(d, args)
	case 'D':
		return parseDCard(d, args)
	case 'E':
		return parseECard(d, args)
	case 'F':
		return parseFCard(d, args)
	case 'J':
		return parseJCard(d, args)
	case 'Q':
		return parseQCard(d, args)
	case 'T':
		return parseTCard(d, args)
	// Simple cards stay inline:
	case 'B':
		d.B = strings.TrimSpace(args)
		return nil
	case 'C':
		d.C = FossilDecode(args)
		return nil
	case 'G':
		d.G = strings.TrimSpace(args)
		return nil
	case 'H':
		d.H = FossilDecode(args)
		return nil
	case 'I':
		d.I = strings.TrimSpace(args)
		return nil
	case 'K':
		d.K = strings.TrimSpace(args)
		return nil
	case 'L':
		d.L = FossilDecode(args)
		return nil
	case 'M':
		return parseMCard(d, args)
	case 'N':
		d.N = strings.TrimSpace(args)
		return nil
	case 'P':
		// Repeatable, and neither sorted nor deduplicated (§4.5.2): P
		// order is semantic, primary parent first. Appending — not
		// assigning — is what makes a second P card add parents rather
		// than replace them (§4.5.1).
		d.P = append(d.P, strings.Fields(args)...)
		return nil
	case 'R':
		d.R = strings.TrimSpace(args)
		return nil
	case 'U':
		return parseUCard(d, args)
	case 'Z':
		return parseZCard(args)
	default:
		return fmt.Errorf("unknown card '%c'", card)
	}
}

func parseACard(d *Deck, args string) error {
	parts := strings.SplitN(args, " ", 3)
	if len(parts) < 2 {
		return fmt.Errorf("A-card needs 2+ fields")
	}
	ac := &AttachmentCard{Filename: FossilDecode(parts[0]), Target: parts[1]}
	if len(parts) == 3 {
		ac.Source = parts[2]
	}
	d.A = ac
	return nil
}

func parseDCard(d *Deck, args string) error {
	t, err := parseTimestamp(args)
	if err != nil {
		return fmt.Errorf("D-card: %w", err)
	}
	d.D = t
	return nil
}

func parseECard(d *Deck, args string) error {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) != 2 {
		return fmt.Errorf("E-card needs datetime and uuid")
	}
	t, err := parseTimestamp(parts[0])
	if err != nil {
		return fmt.Errorf("E-card: %w", err)
	}
	d.E = &EventCard{Date: t, UUID: parts[1]}
	return nil
}

func parseFCard(d *Deck, args string) error {
	parts := strings.Fields(args)
	if len(parts) == 0 {
		return fmt.Errorf("empty F-card")
	}
	fc := FileCard{Name: FossilDecode(parts[0])}
	if len(parts) >= 2 {
		fc.UUID = parts[1]
	}
	if len(parts) >= 3 {
		fc.Perm = parts[2]
	}
	if len(parts) >= 4 {
		fc.OldName = FossilDecode(parts[3])
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

func parseMCard(d *Deck, args string) error {
	hash := strings.TrimSpace(args)
	// §4.5.2: strictly ascending by hash.
	if n := len(d.M); n > 0 {
		if err := requireAscending('M', d.M[n-1], hash); err != nil {
			return err
		}
	}
	d.M = append(d.M, hash)
	return nil
}

func parseJCard(d *Deck, args string) error {
	parts := strings.SplitN(args, " ", 2)
	jf := TicketField{Name: FossilDecode(parts[0])}
	if len(parts) == 2 {
		jf.Value = parts[1]
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

func parseQCard(d *Deck, args string) error {
	if len(args) < 2 {
		return fmt.Errorf("Q-card too short")
	}
	cp := CherryPick{IsBackout: args[0] == '-'}
	rest := args[1:]
	parts := strings.SplitN(rest, " ", 2)
	cp.Target = parts[0]
	if len(parts) == 2 {
		cp.Baseline = parts[1]
	}
	d.Q = append(d.Q, cp)
	return nil
}

func parseTCard(d *Deck, args string) error {
	if len(args) < 2 {
		return fmt.Errorf("T-card too short")
	}
	// §4.7.16: the first character of the name token MUST be +, - or *.
	// Rejecting anything else also keeps the sign a single byte, so the
	// ordering key assembled from it cannot silently gain a second byte
	// through UTF-8 expansion.
	switch TagType(args[0]) {
	case TagSingleton, TagPropagating, TagCancel:
	default:
		return fmt.Errorf("T-card name must begin with '+', '-' or '*', got %q", args[:1])
	}
	tc := TagCard{Type: TagType(args[0])}
	parts := strings.SplitN(args[1:], " ", 3)
	if len(parts) < 2 {
		return fmt.Errorf("T-card needs name and uuid")
	}
	tc.Name = parts[0]
	tc.UUID = parts[1]
	if len(parts) == 3 {
		tc.Value = parts[2]
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
//
// args is trimmed before the emptiness check, matching canonical fossil's
// next_token() (which skips leading whitespace) and every sibling card
// parser in this file (B/G/I/K/M/N/R all TrimSpace); otherwise a
// whitespace-only U-card ("U   \n") would store literal whitespace instead
// of resolving to "anonymous".
func parseUCard(d *Deck, args string) error {
	user := FossilDecode(strings.TrimSpace(args))
	if user == "" {
		user = "anonymous"
	}
	d.U = &user
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
	case len(d.F) > 0 || d.R != "":
		return Checkin
	case len(d.T) > 0:
		return Control
	default:
		return Checkin
	}
}
