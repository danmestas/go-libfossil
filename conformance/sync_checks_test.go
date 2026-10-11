package conformance

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
)

// A server case's expectation is a replyCheck: nil when the reply does what
// the case expects, else what it did instead. The same check judges
// go-libfossil's reply and fossil(1)'s, so it may name only what the draft
// fixes; where the draft fixes no text, a check lists each
// implementation's text for the same reason, never "any error".

type replyCheck func(r *serverReply) error

// all passes when every check passes, and reports the first that does not.
func all(checks ...replyCheck) replyCheck {
	return func(r *serverReply) error {
		for _, c := range checks {
			if err := c(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// either passes when any of checks passes, and otherwise reports every
// failure. It is for behavior the draft leaves open and fossil(1) builds
// resolve differently.
func either(checks ...replyCheck) replyCheck {
	return func(r *serverReply) error {
		var failures []string
		for _, c := range checks {
			err := c(r)
			if err == nil {
				return nil
			}
			failures = append(failures, err.Error())
		}
		return expectf("%s", strings.Join(failures, "; or "))
	}
}

// syncReply checks that the reply is a sync reply at all: status 200, a
// sync media type, and cards that parse.
func syncReply(r *serverReply) error {
	if r.status != http.StatusOK {
		return expectf("%s: HTTP %d %.120q, want a 200 sync reply", r.who, r.status, r.text)
	}
	if !strings.HasPrefix(r.ctype, "application/x-fossil") {
		return expectf("%s: Content-Type %q, want a sync reply", r.who, r.ctype)
	}
	return nil
}

// completed checks for a sync reply ending in the timestamp comment.
func completed(r *serverReply) error {
	if err := syncReply(r); err != nil {
		return err
	}
	if n := len(r.cards); n == 0 || !strings.HasPrefix(r.cards[n-1].line, "# timestamp ") {
		return expectf("%s: reply does not end with the timestamp comment: %q", r.who, tail(r))
	}
	return nil
}

// withCard checks that some card line is exactly line.
func withCard(line string) replyCheck {
	return func(r *serverReply) error {
		want := r.fx.expand(line)
		if !slices.Contains(lines(r.cards), want) {
			return expectf("%s: no card %q in %q", r.who, want, tail(r))
		}
		return nil
	}
}

// noCard checks that no card line starts with prefix.
func noCard(prefix string) replyCheck {
	return func(r *serverReply) error {
		want := r.fx.expand(prefix)
		if i := index(r.cards, want); i >= 0 {
			return expectf("%s: unexpected card %q", r.who, r.cards[i].line)
		}
		return nil
	}
}

// count checks that exactly n card lines start with prefix.
func count(prefix string, n int) replyCheck {
	return func(r *serverReply) error {
		got := len(find(r.cards, r.fx.expand(prefix)))
		if got != n {
			return expectf("%s: %d cards start %q, want %d", r.who, got, prefix, n)
		}
		return nil
	}
}

// onlyError checks that the reply has exactly one error card and that its
// decoded text contains one of reasons: each implementation's wording of
// the reason the case is about. Any other error means another rule fired.
func onlyError(reasons ...string) replyCheck {
	return func(r *serverReply) error {
		errs := errorTexts(r.cards)
		if len(errs) != 1 {
			return expectf("%s: error cards %q, want one for %q", r.who, errs, reasons)
		}
		for _, reason := range reasons {
			if strings.Contains(errs[0], r.fx.expand(reason)) {
				return nil
			}
		}
		return expectf("%s: error %q, want one of %q", r.who, errs[0], reasons)
	}
}

// noErrors checks that the reply carries no error card.
func noErrors(r *serverReply) error {
	if errs := errorTexts(r.cards); len(errs) > 0 {
		return expectf("%s: error cards %q", r.who, errs)
	}
	return nil
}

// before checks that the first card starting with a precedes the first
// starting with b, and that both are present.
func before(a, b string) replyCheck {
	return func(r *serverReply) error {
		i, j := index(r.cards, r.fx.expand(a)), index(r.cards, r.fx.expand(b))
		if i < 0 || j < 0 || i >= j {
			return expectf("%s: want %q (at %d) before %q (at %d) in %q",
				r.who, a, i, b, j, tail(r))
		}
		return nil
	}
}

// exactly checks that the card lines, timestamp comment left out, are
// exactly want.
func exactly(want ...string) replyCheck {
	return func(r *serverReply) error {
		var got []string
		for _, line := range lines(r.cards) {
			if !strings.HasPrefix(line, "# timestamp ") {
				got = append(got, line)
			}
		}
		expanded := make([]string, len(want))
		for i := range want {
			expanded[i] = r.fx.expand(want[i])
		}
		if !slices.Equal(got, expanded) {
			return expectf("%s: cards %q, want %q", r.who, got, expanded)
		}
		return nil
	}
}

// repoCount checks that a query on the repository after the request returns n.
func repoCount(query string, n int) replyCheck {
	return func(r *serverReply) error {
		got, err := queryInt(r.fx, r.repo, query)
		if err != nil {
			return err
		}
		if got != n {
			return expectf("%s: %s gives %d, want %d", r.who, query, got, n)
		}
		return nil
	}
}

// matches checks that the line of the first card starting with prefix
// matches pattern.
func matches(prefix, pattern string) replyCheck {
	re := regexp.MustCompile(pattern)
	return func(r *serverReply) error {
		i := index(r.cards, r.fx.expand(prefix))
		if i < 0 {
			return expectf("%s: no card %q in %q", r.who, prefix, tail(r))
		}
		if !re.MatchString(r.cards[i].line) {
			return expectf("%s: %q does not match %s", r.who, r.cards[i].line, pattern)
		}
		return nil
	}
}

// tail is the reply's card lines for a message, with the long inventory
// of igot cards counted rather than listed.
func tail(r *serverReply) []string {
	var out []string
	igots := 0
	for _, line := range lines(r.cards) {
		if strings.HasPrefix(line, "igot ") && !strings.Contains(line[5:], " ") {
			igots++
			continue
		}
		out = append(out, line)
	}
	if igots > 0 {
		out = append(out, "(+"+strconv.Itoa(igots)+" public igot)")
	}
	if len(r.cards) == 0 {
		out = append(out, string(r.text[:min(len(r.text), 120)]))
	}
	return out
}

// mediaType checks the reply's Content-Type, parameters aside.
func mediaType(want string) replyCheck {
	return func(r *serverReply) error {
		got, _, _ := strings.Cut(r.ctype, ";")
		if strings.TrimSpace(got) != want {
			return expectf("%s: Content-Type %q, want %s", r.who, r.ctype, want)
		}
		return nil
	}
}

// payloadIs checks the payload of the first card starting with prefix.
func payloadIs(prefix, data string) replyCheck {
	return func(r *serverReply) error {
		i := index(r.cards, r.fx.expand(prefix))
		if i < 0 {
			return expectf("%s: no card %q in %q", r.who, prefix, tail(r))
		}
		if want := r.fx.expand(data); string(r.cards[i].payload) != want {
			return expectf("%s: payload %q, want %q", r.who, r.cards[i].payload, want)
		}
		return nil
	}
}

// tokenCount checks the number of tokens of the first card starting with
// prefix.
func tokenCount(prefix string, n int) replyCheck {
	return func(r *serverReply) error {
		i := index(r.cards, r.fx.expand(prefix))
		if i < 0 {
			return expectf("%s: no card %q in %q", r.who, prefix, tail(r))
		}
		if got := len(r.cards[i].tokens()); got != n {
			return expectf("%s: %q has %d tokens, want %d", r.who, r.cards[i].line, got, n)
		}
		return nil
	}
}

// eachCard applies check to every card, comments included.
func eachCard(check func(c wireCard) error) replyCheck {
	return func(r *serverReply) error {
		if len(r.cards) == 0 {
			return expectf("%s: no cards: %.120q", r.who, r.text)
		}
		for _, c := range r.cards {
			if err := check(c); err != nil {
				return expectf("%s: %v", r.who, err)
			}
		}
		return nil
	}
}

// canonicalLine is the canonical card form of Section 5.2: a lowercase
// operator and tokens separated by one SP, or a comment.
var canonicalLine = regexp.MustCompile(`^(#.*|[a-z_]+( [^\x00-\x20]+)*)$`)

func canonical(c wireCard) error {
	if !canonicalLine.MatchString(c.line) {
		return expectf("%q is not a canonical card", c.line)
	}
	return nil
}

// sixTokens checks the Section 3.9 window: no card line has a seventh
// token.
func sixTokens(c wireCard) error {
	if !strings.HasPrefix(c.line, "#") && len(c.tokens()) > 6 {
		return expectf("%q has %d tokens", c.line, len(c.tokens()))
	}
	return nil
}

// cfilePadding checks Section 5.1: a cfile payload that does not end in LF
// is followed by one LF, and one that does is not.
func cfilePadding(c wireCard) error {
	if c.op() != "cfile" || len(c.payload) == 0 {
		return nil
	}
	if endsLF := c.payload[len(c.payload)-1] == '\n'; c.padded == endsLF {
		return expectf("%q: payload ends in LF %v, padded %v", c.line, endsLF, c.padded)
	}
	return nil
}

// container checks the reply entity is the Section 4.1 container: the
// big-endian length of the card text, then a zlib stream (CMF 0x78).
func container(r *serverReply) error {
	if err := mediaType(typeCompressed)(r); err != nil {
		return err
	}
	if len(r.raw) < 6 || r.raw[4] != 0x78 {
		return expectf("%s: entity %.12q does not start a zlib stream at byte 4", r.who, r.raw)
	}
	if n := int(binary.BigEndian.Uint32(r.raw[:4])); n != len(r.text) {
		return expectf("%s: length field %d, card text %d bytes", r.who, n, len(r.text))
	}
	return nil
}

// recentUTC checks the timestamp comment's DATETIME is the current UTC
// time, within the time a test run takes.
func recentUTC(r *serverReply) error {
	i := index(r.cards, "# timestamp ")
	if i < 0 {
		return expectf("%s: no timestamp comment in %q", r.who, tail(r))
	}
	fields := strings.Fields(r.cards[i].line)
	when, err := time.Parse("2006-01-02T15:04:05", fields[2])
	if err != nil {
		return expectf("%s: %q: %v", r.who, r.cards[i].line, err)
	}
	if d := time.Since(when); d < -5*time.Minute || d > 5*time.Minute {
		return expectf("%s: timestamp %s is %v from UTC now", r.who, fields[2], d)
	}
	return nil
}

// cfileRows looks up each cfile card's artifact in the repository after the
// request: its rid, stored content, and whether it is stored as a delta.
type cfileRow struct {
	card    wireCard
	rid     int
	content []byte
	isDelta bool
}

func cfileRows(r *serverReply) ([]cfileRow, error) {
	conn, err := db.OpenSQL(r.repo, db.OpenConfig{}, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out []cfileRow
	for _, c := range find(r.cards, "cfile ") {
		row := cfileRow{card: c}
		err := conn.QueryRow(`SELECT rid, content, EXISTS(SELECT 1 FROM delta WHERE rid=blob.rid)
			FROM blob WHERE uuid=?`, c.tokens()[1]).Scan(&row.rid, &row.content, &row.isDelta)
		if err != nil {
			return nil, expectf("%s: %q names no stored artifact: %v", r.who, c.line, err)
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil, expectf("%s: no cfile card in %q", r.who, tail(r))
	}
	return out, nil
}

// verbatim checks Section 7.2: a cfile payload is the stored blob as it is
// stored. A delta-stored artifact sent in full, as Section 7.5 requires
// when its basis is private, has no stored form to compare.
func verbatim(r *serverReply) error {
	rows, err := cfileRows(r)
	if err != nil {
		return err
	}
	for _, row := range rows {
		full := len(row.card.tokens()) == 4
		if full && row.isDelta {
			continue
		}
		if !bytes.Equal(row.card.payload, row.content) {
			return expectf("%s: %q: payload of %d bytes is not the %d stored", r.who,
				row.card.line, len(row.card.payload), len(row.content))
		}
	}
	return nil
}

// ridOrder checks the cfile cards come in ascending rid order, starting
// at rid first or later and including it.
func ridOrder(first int) replyCheck {
	return func(r *serverReply) error {
		rows, err := cfileRows(r)
		if err != nil {
			return err
		}
		if rows[0].rid != first {
			return expectf("%s: first cfile is rid %d, want %d", r.who, rows[0].rid, first)
		}
		for i := 1; i < len(rows); i++ {
			if rows[i].rid <= rows[i-1].rid {
				return expectf("%s: rid %d sent after rid %d", r.who, rows[i].rid, rows[i-1].rid)
			}
		}
		return nil
	}
}

// nothingBut checks every card other than a comment starts with one of
// prefixes.
func nothingBut(prefixes ...string) replyCheck {
	return func(r *serverReply) error {
		for _, c := range r.cards {
			if strings.HasPrefix(c.line, "#") || hasAnyPrefix(c.line, prefixes) {
				continue
			}
			return expectf("%s: unexpected card %q", r.who, c.line)
		}
		return nil
	}
}

func hasAnyPrefix(line string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// scalarPayload is Appendix B's config-scalar-payload, then the LF that
// ends a config payload.
var scalarPayload = regexp.MustCompile(`^[0-9]+ '([^']|'')*' value '([^']|'')*'\n$`)

// present checks that some card line starts with prefix.
func present(prefix string) replyCheck {
	return func(r *serverReply) error {
		if index(r.cards, r.fx.expand(prefix)) < 0 {
			return expectf("%s: no card %q in %q", r.who, prefix, tail(r))
		}
		return nil
	}
}
