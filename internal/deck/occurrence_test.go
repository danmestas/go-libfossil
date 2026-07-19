package deck

import (
	"crypto/md5"
	"fmt"
	"strings"
	"testing"
)

// hashN builds a distinct 40-character lowercase artifact hash whose first
// byte is c, so tests can order hashes by inspection.
func hashN(c byte) string {
	return string(c) + strings.Repeat("0", 39)
}

// parseBody frames body as a complete artifact by appending the trailing Z
// card VerifyZ requires, then parses it. Every test in this file works on
// well-framed input so that any error returned is the occurrence or
// ordering rule under test, never checksum framing.
func parseBody(body string) (*Deck, error) {
	h := md5.Sum([]byte(body))
	return Parse([]byte(fmt.Sprintf("%sZ %x\n", body, h)))
}

// TestSingleOccurrenceCardsRejectDuplicates covers every one of the 14
// single-occurrence types of draft-fossil-artifact-format-00 §4.5.1. Each
// case feeds two syntactically valid cards of one type; the card-type
// ordering rule of §4.4 permits equal consecutive letters, so nothing but
// the duplicate guard can reject these.
func TestSingleOccurrenceCardsRejectDuplicates(t *testing.T) {
	cases := []struct {
		card byte
		body string
	}{
		{'A', "A one.txt " + hashN('a') + "\nA two.txt " + hashN('b') + "\n"},
		{'B', "B " + hashN('a') + "\nB " + hashN('b') + "\n"},
		{'C', "C first\nC second\n"},
		{'D', "D 2024-01-15T10:30:00.000\nD 2024-01-16T10:30:00.000\n"},
		{'E', "E 2024-01-15T10:30:00 " + hashN('a') + "\nE 2024-01-16T10:30:00 " + hashN('b') + "\n"},
		{'G', "G " + hashN('a') + "\nG " + hashN('b') + "\n"},
		{'H', "H first\nH second\n"},
		{'I', "I " + hashN('a') + "\nI " + hashN('b') + "\n"},
		{'K', "K " + hashN('a') + "\nK " + hashN('b') + "\n"},
		{'L', "L PageOne\nL PageTwo\n"},
		{'N', "N text/plain\nN text/x-markdown\n"},
		{'R', "R " + strings.Repeat("0", 32) + "\nR " + strings.Repeat("1", 32) + "\n"},
		{'U', "U alice\nU bob\n"},
		{'W', "W 3\nabc\nW 3\nxyz\n"},
	}
	if len(cases) != 14 {
		t.Fatalf("case count = %d, want 14 single-occurrence types", len(cases))
	}
	for _, tc := range cases {
		if _, err := parseBody(tc.body); err == nil {
			t.Errorf("duplicate %c card: Parse succeeded, want error", tc.card)
		}
		// The single occurrence must still parse, so the guard cannot be a
		// blanket rejection of the letter.
		single := strings.SplitAfter(tc.body, "\n")[0]
		if tc.card == 'W' {
			single = "W 3\nabc\n"
		}
		if _, err := parseBody(single); err != nil {
			t.Errorf("single %c card: Parse: %v", tc.card, err)
		}
	}
}

// TestDuplicateCardIsErrorNotPanic pins that malformed peer input is
// reported through the error return. The assertion idiom in this package
// panics only on programmer error; artifact bytes arrive from the network.
func TestDuplicateCardIsErrorNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Parse panicked on duplicate card: %v", r)
		}
	}()
	if _, err := parseBody("U alice\nU bob\n"); err == nil {
		t.Fatal("duplicate U card: Parse succeeded, want error")
	}
}

// TestFCardOrdering exercises the strict-ascending F rule of §4.5.2.
func TestFCardOrdering(t *testing.T) {
	ascending := "F a.txt " + hashN('a') + "\nF b.txt " + hashN('b') + "\n"
	if _, err := parseBody(ascending); err != nil {
		t.Fatalf("ascending F run: Parse: %v", err)
	}
	descending := "F b.txt " + hashN('a') + "\nF a.txt " + hashN('b') + "\n"
	if _, err := parseBody(descending); err == nil {
		t.Error("descending F run: Parse succeeded, want error")
	}
	equal := "F a.txt " + hashN('a') + "\nF a.txt " + hashN('b') + "\n"
	if _, err := parseBody(equal); err == nil {
		t.Error("duplicate F name: Parse succeeded, want error")
	}
}

// TestFCardOrderingUsesDecodedFilename pins that the F sort check runs on
// the post-decode filename, not the raw wire token (§4.5.2). The two names
// "a b" and "a-b" order oppositely in the two forms: decoded, 0x20 sorts
// before 0x2D; encoded, "a\sb" begins its third byte with 0x5C, which
// sorts after 0x2D. A raw-token comparison would therefore accept the
// descending run and reject the ascending one.
func TestFCardOrderingUsesDecodedFilename(t *testing.T) {
	decodedAscending := `F a\sb ` + hashN('a') + "\nF a-b " + hashN('b') + "\n"
	d, err := parseBody(decodedAscending)
	if err != nil {
		t.Fatalf("decoded-ascending F run: Parse: %v", err)
	}
	if len(d.F) != 2 || d.F[0].Name != "a b" || d.F[1].Name != "a-b" {
		t.Fatalf("F = %+v, want decoded [a b, a-b]", d.F)
	}
	decodedDescending := "F a-b " + hashN('a') + "\nF a\\sb " + hashN('b') + "\n"
	if _, err := parseBody(decodedDescending); err == nil {
		t.Error("decoded-descending F run: Parse succeeded, want error")
	}
}

// TestJCardOrdering exercises the strict-ascending J rule of §4.5.2.
func TestJCardOrdering(t *testing.T) {
	if _, err := parseBody("J alpha 1\nJ beta 2\n"); err != nil {
		t.Fatalf("ascending J run: Parse: %v", err)
	}
	if _, err := parseBody("J beta 1\nJ alpha 2\n"); err == nil {
		t.Error("descending J run: Parse succeeded, want error")
	}
	if _, err := parseBody("J alpha 1\nJ alpha 2\n"); err == nil {
		t.Error("duplicate J field name: Parse succeeded, want error")
	}
}

// TestMCardOrdering exercises the strict-ascending M rule of §4.5.2.
func TestMCardOrdering(t *testing.T) {
	if _, err := parseBody("M " + hashN('a') + "\nM " + hashN('b') + "\n"); err != nil {
		t.Fatalf("ascending M run: Parse: %v", err)
	}
	if _, err := parseBody("M " + hashN('b') + "\nM " + hashN('a') + "\n"); err == nil {
		t.Error("descending M run: Parse succeeded, want error")
	}
	if _, err := parseBody("M " + hashN('a') + "\nM " + hashN('a') + "\n"); err == nil {
		t.Error("duplicate M hash: Parse succeeded, want error")
	}
}

// TestTCardTwoLevelOrdering exercises the two-level (name, target hash)
// rule of §4.5.2. The primary key is the full name token including its
// leading sign character (§4.7.16), so "+x" and "*x" are distinct names.
//
// The prefix-name case is what distinguishes a two-level comparison from
// a comparison of the concatenated name+target: under concatenation the
// target's leading hex digit competes with the longer name's next
// character, so "f0..." on "+sym-foo" would sort it after "+sym-foobar"
// and the run would be rejected. Two-level accepts it.
func TestTCardTwoLevelOrdering(t *testing.T) {
	if _, err := parseBody("T +sym-foo f" + strings.Repeat("0", 39) +
		"\nT +sym-foobar " + strings.Repeat("0", 40) + "\n"); err != nil {
		t.Fatalf("prefix T names with a high target hash: Parse: %v", err)
	}
	if _, err := parseBody("T +alpha " + hashN('a') + "\nT +beta " + hashN('a') + "\n"); err != nil {
		t.Fatalf("ascending T names: Parse: %v", err)
	}
	if _, err := parseBody("T +beta " + hashN('a') + "\nT +alpha " + hashN('b') + "\n"); err == nil {
		t.Error("descending T names: Parse succeeded, want error")
	}
	// Equal names: the target hash must strictly increase.
	if _, err := parseBody("T +alpha " + hashN('a') + "\nT +alpha " + hashN('b') + "\n"); err != nil {
		t.Fatalf("equal T names with ascending hash: Parse: %v", err)
	}
	if _, err := parseBody("T +alpha " + hashN('b') + "\nT +alpha " + hashN('a') + "\n"); err == nil {
		t.Error("equal T names with descending hash: Parse succeeded, want error")
	}
	if _, err := parseBody("T +alpha " + hashN('a') + "\nT +alpha " + hashN('a') + "\n"); err == nil {
		t.Error("equal T names with equal hash: Parse succeeded, want error")
	}
}

// TestTCardOrderingUsesDecodedName pins that the T ordering key is the
// escape-decoded name (§4.7.16), the same post-decode rule §4.5.2 states
// for F. The two tag names below order oppositely in the two forms:
// decoded, 0x20 sorts before 0x2D; on the wire, "\s" begins with 0x5C,
// which sorts after 0x2D. Reachable in practice, because a branch name
// may contain a space and so reaches the T card as "sym-my\sbranch".
func TestTCardOrderingUsesDecodedName(t *testing.T) {
	decodedAscending := `T *sym-a\sb ` + hashN('a') + "\nT *sym-a-c " + hashN('b') + "\n"
	if _, err := parseBody(decodedAscending); err != nil {
		t.Fatalf("decoded-ascending T run: Parse: %v", err)
	}
	decodedDescending := "T *sym-a-c " + hashN('a') + "\nT *sym-a\\sb " + hashN('b') + "\n"
	if _, err := parseBody(decodedDescending); err == nil {
		t.Error("decoded-descending T run: Parse succeeded, want error")
	}
}

// TestTCardRejectsUnknownSign pins the sign-prefix check of §4.7.16: the
// first character of the name token MUST be +, - or *. Without it a sign
// byte >= 0x80 would expand to two bytes when the ordering key is
// assembled, silently corrupting the comparison.
func TestTCardRejectsUnknownSign(t *testing.T) {
	if _, err := parseBody("T ?alpha " + hashN('a') + "\n"); err == nil {
		t.Error("T card with sign '?': Parse succeeded, want error")
	}
	if _, err := parseBody("T \xc3alpha " + hashN('a') + "\n"); err == nil {
		t.Error("T card with a non-ASCII sign byte: Parse succeeded, want error")
	}
	for _, sign := range []string{"+", "-", "*"} {
		if _, err := parseBody("T " + sign + "alpha " + hashN('a') + "\n"); err != nil {
			t.Errorf("T card with sign %q: Parse: %v", sign, err)
		}
	}
}

// TestZCardTokenValidation pins §4.7.19: the Z payload is a checksum token
// of exactly 32 hexadecimal characters, never decoded. An early Z line is
// legal content (§4.5.1) but it is still a typed card, not an
// arbitrary-byte channel.
func TestZCardTokenValidation(t *testing.T) {
	prefix := "D 2024-01-15T10:30:00.000\n"
	bad := []struct {
		name  string
		token string
	}{
		{"not hex", "not-a-checksum-at-all-whatever!!"},
		{"too short", strings.Repeat("0", 31)},
		{"too long", strings.Repeat("0", 33)},
		{"empty", ""},
	}
	for _, tc := range bad {
		if _, err := parseBody(prefix + "Z " + tc.token + "\n"); err == nil {
			t.Errorf("early Z card (%s): Parse succeeded, want error", tc.name)
		}
	}
	// Uppercase hex digits are accepted: the canonical writer form is
	// lowercase, but "hexadecimal" in §4.7.19 is case-insensitive, so
	// rejecting uppercase would refuse an artifact canonical accepts.
	for _, good := range []string{strings.Repeat("0", 32), strings.Repeat("A", 32), strings.Repeat("f", 32)} {
		if _, err := parseBody(prefix + "Z " + good + "\n"); err != nil {
			t.Errorf("early Z card %q: Parse: %v", good, err)
		}
	}
}

// TestPCardsAreNotSortedOrDeduplicated pins the P exception in §4.5.2: P
// order is semantic — the primary parent comes first — so a descending or
// repeating run is valid and must survive parsing unchanged. Sorting or
// deduplicating P would silently break parent resolution.
func TestPCardsAreNotSortedOrDeduplicated(t *testing.T) {
	body := "P " + hashN('c') + " " + hashN('a') + "\nP " + hashN('b') + " " + hashN('a') + "\n"
	d, err := parseBody(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{hashN('c'), hashN('a'), hashN('b'), hashN('a')}
	if len(d.P) != len(want) {
		t.Fatalf("P = %v, want %v", d.P, want)
	}
	for i := range want {
		if d.P[i] != want[i] {
			t.Fatalf("P = %v, want %v", d.P, want)
		}
	}
}

// TestQCardsAreUnorderedRepeatable pins the Q exception in §4.5.2: Q has
// no duplicate guard and no ordering check at all.
func TestQCardsAreUnorderedRepeatable(t *testing.T) {
	body := "Q +" + hashN('c') + "\nQ +" + hashN('a') + "\nQ +" + hashN('a') + "\n"
	d, err := parseBody(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Q) != 3 {
		t.Fatalf("Q = %+v, want 3 entries", d.Q)
	}
	if d.Q[0].Target != hashN('c') || d.Q[1].Target != hashN('a') || d.Q[2].Target != hashN('a') {
		t.Fatalf("Q = %+v, want descending run with a duplicate preserved", d.Q)
	}
}

// TestZCardHasNoDuplicateOrLastCardGuard pins §4.5.1: the Z card belongs
// to neither occurrence class. A Z line before the framing checksum is
// simply content covered by that checksum, so it must not be rejected as a
// duplicate or as a misplaced last card.
func TestZCardHasNoDuplicateOrLastCardGuard(t *testing.T) {
	body := "D 2024-01-15T10:30:00.000\nZ " + strings.Repeat("0", 32) + "\n"
	if _, err := parseBody(body); err != nil {
		t.Fatalf("earlier Z line: Parse: %v", err)
	}
}

// TestMarshalEmitsParseableIntraRunOrder pins that every artifact this
// package can produce satisfies the intra-run ordering rules its own
// parser now enforces (§4.5.2). The deck below is deliberately built in
// non-ascending order for each repeatable type, because callers construct
// decks in whatever order suits them and the marshaller owns wire order.
//
// The T case is the subtle one: sorting T by the concatenation
// name+target rather than by the two-level (name, target) key reorders
// tags whose names are prefixes of one another, because the target's
// leading hex digit then competes with the longer name's next character.
// Here "f0..." sorts after "bar", so a concatenated key would emit
// "+sym-foobar" ahead of "+sym-foo" and produce an artifact the parser
// rejects.
func TestMarshalEmitsParseableIntraRunOrder(t *testing.T) {
	d := &Deck{
		F: []FileCard{
			{Name: "zeta.txt", UUID: hashN('a')},
			{Name: "alpha.txt", UUID: hashN('b')},
		},
		J: []TicketField{
			{Name: "title", Value: "Test ticket"},
			{Name: "status", Value: "Open"},
		},
		M: []string{hashN('c'), hashN('a')},
		T: []TagCard{
			{Type: TagSingleton, Name: "sym-foo", UUID: "f" + strings.Repeat("0", 39)},
			{Type: TagSingleton, Name: "sym-foobar", UUID: strings.Repeat("0", 40)},
		},
		U: User("alice"),
	}
	data, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("Parse of marshalled deck: %v\n%s", err, data)
	}
}

// TestMarshalSortsTagsByDecodedName pins that the marshaller orders T
// cards by the same decoded key the parser checks (§4.7.16). TagCard.Name
// holds the raw wire form, so a name carrying an escape orders one way
// decoded and the other way raw; sorting on the raw form would emit a run
// the parser rejects.
func TestMarshalSortsTagsByDecodedName(t *testing.T) {
	d := &Deck{
		T: []TagCard{
			{Type: TagPropagating, Name: "sym-a-c", UUID: hashN('b')},
			{Type: TagPropagating, Name: `sym-a\sb`, UUID: hashN('a')},
		},
	}
	data, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), "T *sym-a\\sb "+hashN('a')+"\nT *sym-a-c "+hashN('b')+"\n") {
		t.Fatalf("T cards not emitted in decoded-name order:\n%s", data)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("Parse of marshalled deck: %v\n%s", err, data)
	}
}

// TestCompare pins the canonical comparator of §4.5.3: raw-byte
// comparison of unsigned octets, no case folding, no Unicode
// normalization, a prefix before any extension of itself, an absent value
// before any present one, and no special treatment for '/' (0x2F).
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"", "a", -1},        // absent sorts before present
		{"abc", "abcd", -1},  // prefix sorts before its extension
		{"A", "a", -1},       // no case folding: 0x41 < 0x61
		{"Z", "a", -1},       // no case folding: 0x5A < 0x61
		{"\x7e", "\x80", -1}, // octets compared as unsigned values
		{"\x80", "\x7e", 1},  //
		{"a.b", "a/b", -1},   // '/' (0x2F) gets no special treatment
		{"a-b", "a/b", -1},   //
		{"a/b", "a0b", -1},   //
		{"a b", "a-b", -1},   // 0x20 < 0x2D
		{"\xc3\xa9", "z", 1}, // no Unicode normalization or collation
	}
	for _, tc := range cases {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
