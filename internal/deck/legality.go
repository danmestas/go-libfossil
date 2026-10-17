package deck

import "fmt"

// Card legality and per-type semantic checks, §5.2 and §5.4. The allowed and
// required card sets are fossil's manifestCardTypes table (src/manifest.c),
// one row per artifact type; they match the table the conformance suite reads
// from the draft.
type cardTable struct {
	allowed  uint32
	required uint32
}

// cardTables is indexed by ArtifactType. Each entry's allowed and required
// masks are built from the §5.2 Table 4 letter sets.
var cardTables = func() map[ArtifactType]cardTable {
	rows := map[ArtifactType][2]string{
		Checkin:    {"BCDFNPQRSTUZ", "DZ"},
		Wiki:       {"CDLNPUWZ", "DLUWZ"},
		Ticket:     {"DJKUZ", "DJKUZ"},
		Event:      {"CDENPTUWZ", "DEWZ"},
		Cluster:    {"MZ", "MZ"},
		ForumPost:  {"DGHINPUWZ", "DUWZ"},
		Attachment: {"ACDNUZ", "ADZ"},
		Control:    {"DTUZ", "DTUZ"},
		// draft-fossil-submanifests-00 §4.2, Table 1.
		Submanifest: {"FSZ", "Z"},
	}
	tables := make(map[ArtifactType]cardTable, len(rows))
	for t, r := range rows {
		tables[t] = cardTable{allowed: letterMask(r[0]), required: letterMask(r[1])}
	}
	return tables
}()

// letterMask turns a set of upper-case card letters into a bitmask, bit
// (letter-'A') per letter.
func letterMask(letters string) uint32 {
	var m uint32
	for i := 0; i < len(letters); i++ {
		m |= 1 << (letters[i] - 'A')
	}
	return m
}

// checkType applies §5.2 legality (allowed and required cards for the deck's
// type) and the §5.4 per-type semantic checks. seenMask has a bit set per card
// type present in the artifact.
func checkType(d *Deck, seenMask uint32) error {
	table, ok := cardTables[d.Type]
	if !ok {
		panic("deck.checkType: no card table for the artifact type")
	}
	// §5.2: no card outside the type's allowed set, and every required card
	// present. fossil reports the illegal card before the missing one.
	if extra := seenMask &^ table.allowed; extra != 0 {
		return fmt.Errorf("%c-card not allowed in %s", lowestLetter(extra), typeName(d.Type))
	}
	if missing := table.required &^ seenMask; missing != 0 {
		return fmt.Errorf("%c-card missing in %s", lowestLetter(missing), typeName(d.Type))
	}
	return checkSemantics(d)
}

// lowestLetter returns the card letter of the lowest set bit of mask.
func lowestLetter(mask uint32) byte {
	for i := byte(0); i < 26; i++ {
		if mask&(1<<i) != 0 {
			return 'A' + i
		}
	}
	return '?'
}

// checkSemantics applies the §5.4 per-type rules that the card table cannot
// express, ported from the switch at the end of fossil's manifest_parse.
func checkSemantics(d *Deck) error {
	var selfTags, singletonTags int
	for _, t := range d.T {
		if t.UUID == "*" {
			selfTags++
		}
		if t.Type == TagSingleton {
			singletonTags++
		}
	}
	switch d.Type {
	case Checkin:
		return checkTreeManifest(d)
	case Submanifest:
		return checkSubmanifest(d)
	case Control:
		// §5.4: a control artifact's tags target other artifacts, never itself.
		if selfTags > 0 {
			return fmt.Errorf("self-referential T-card in control artifact")
		}
	case Event:
		// §5.4: every tag on a technote is a self-referential singleton.
		if len(d.T) != selfTags {
			return fmt.Errorf("non-self-referential T-card in technote")
		}
		if len(d.T) != singletonTags {
			return fmt.Errorf("T-card with '*' or '-' in technote")
		}
	case ForumPost:
		// §5.4: a post is either a new thread (H) or a reply (I), not both, and
		// a forum post has at most one parent.
		if d.H != "" && d.I != "" {
			return fmt.Errorf("a forum post cannot carry both an H-card and an I-card")
		}
		if len(d.P) > 1 {
			return fmt.Errorf("a forum post has at most one parent")
		}
	}
	return nil
}

// checkTreeManifest applies the two manifest rules of
// draft-fossil-submanifests-00 §4.4: a tree manifest conveys its file set by S
// cards alone, so it carries neither a B card nor an F card.
func checkTreeManifest(d *Deck) error {
	if len(d.S) == 0 {
		return nil
	}
	if d.B != "" {
		return fmt.Errorf("a manifest cannot carry both a B-card and an S-card")
	}
	if len(d.F) > 0 {
		return fmt.Errorf("a manifest cannot carry both F-cards and S-cards")
	}
	return nil
}

// checkSubmanifest applies the sub-manifest rule of
// draft-fossil-submanifests-00 §4.4: F cards or S cards, never both and never
// neither. The rule that every F card carries a hash needs no check here: a
// sub-manifest has no B card, so parseFCard already refuses a hashless one.
func checkSubmanifest(d *Deck) error {
	if len(d.F) > 0 && len(d.S) > 0 {
		return fmt.Errorf("a sub-manifest cannot carry both F-cards and S-cards")
	}
	if len(d.F) == 0 && len(d.S) == 0 {
		return fmt.Errorf("a sub-manifest needs an F-card or an S-card")
	}
	return nil
}

// typeName is the artifact type's name for an error message, matching fossil's
// azNameOfMType.
func typeName(t ArtifactType) string {
	switch t {
	case Checkin:
		return "manifest"
	case Wiki:
		return "wiki"
	case Ticket:
		return "ticket"
	case Event:
		return "technote"
	case Cluster:
		return "cluster"
	case ForumPost:
		return "forum post"
	case Attachment:
		return "attachment"
	case Control:
		return "control artifact"
	case Submanifest:
		return "submanifest"
	default:
		return "artifact"
	}
}
