package deck

import (
	"fmt"
	"strings"
)

// Compare is the canonical comparator of draft-fossil-artifact-format-00
// §4.5.3: a raw-byte comparison in which octets are compared as unsigned
// values, locale-independent, with no Unicode normalization and no case
// folding. A shorter string sorts before any extension of itself, an
// absent value (the empty string) sorts before any present value, and the
// directory separator '/' (0x2F) receives no special treatment. It returns
// a negative number, zero, or a positive number as a sorts before, equal
// to, or after b.
//
// §4.5.3 defines one comparator shared by the F, J, M and T sort checks,
// the delta-baseline merge walk (§8.2) and single-name search (§8.4). A
// second copy anywhere is a correctness hazard, not a style issue: F
// ordering is what the baseline binary search and the delta-manifest merge
// walk rely on, so two comparators that disagree resolve a delta manifest
// wrongly rather than merely failing validation. Route every such
// comparison through here.
func Compare(a, b string) int {
	return strings.Compare(a, b)
}

// singleOccurrence marks, by letter offset from 'A', the 14 card types of
// §4.5.1 whose duplicate MUST be rejected. F, J, M, T, P and Q are
// repeatable and each occurrence appends. The Z card belongs to neither
// class: it has no duplicate guard, because §4.5.1 derives "exactly one Z,
// last" from the checksum framing rather than from a card rule, and an
// earlier Z line is simply content covered by the final checksum.
var singleOccurrence = func() [26]bool {
	var s [26]bool
	for _, c := range "ABCDEGHIKLNRUW" {
		s[c-'A'] = true
	}
	return s
}()

// seenCards tracks which single-occurrence card types a parse has already
// consumed. The zero value is ready to use.
type seenCards [26]bool

// claim records one occurrence of card and reports an error if the type is
// single-occurrence and has been seen before (§4.5.1). Card letters outside
// A-Z are left to the parser's unknown-card path.
func (s *seenCards) claim(card byte) error {
	if card < 'A' || card > 'Z' {
		return nil
	}
	i := card - 'A'
	if !singleOccurrence[i] {
		return nil
	}
	if s[i] {
		return fmt.Errorf("duplicate '%c' card", card)
	}
	s[i] = true
	return nil
}

// tagOrderKey returns the primary ordering key for a T card: the full name
// token including its leading sign character, escape-decoded (§4.7.16).
// The decode is the same post-decode rule §4.5.2 spells out for F — a
// name reaches the wire as "sym-my\sbranch" and orders as "sym-my branch",
// so comparing raw tokens rejects runs canonical fossil accepts.
//
// TagCard splits the sign into Type and stores Name in raw wire form, so
// the key is reassembled here rather than read off a single field. Both
// the parser's ordering check and the marshaller's sort call this, so
// neither can drift from the other.
func tagOrderKey(t TagCard) string {
	return FossilDecode(string(t.Type) + t.Name)
}

// requireAscending enforces the single-key strictly-ascending intra-run
// rule of §4.5.2 for the F, J and M card lists: equal or descending
// neighbours are rejected. The key is always the parser's decoded form,
// which for F is the post-decode filename rather than the raw wire token.
func requireAscending(card byte, previous, current string) error {
	if Compare(current, previous) > 0 {
		return nil
	}
	return fmt.Errorf("%c-card %q out of order after %q", card, current, previous)
}
