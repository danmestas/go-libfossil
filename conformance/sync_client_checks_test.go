package conformance

import (
	"regexp"
	"slices"
	"strings"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
)

// A client case's expectation is a clientCheck: nil when the client did what
// the case expects. Checks on requests apply to fossil(1)'s client too;
// checks on the repository read whichever client's repository the run
// left.

type clientCheck func(r *clientRun) error

func every(checks ...clientCheck) clientCheck {
	return func(r *clientRun) error {
		for _, c := range checks {
			if err := c(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// rounds checks the client sent exactly n requests.
func rounds(n int) clientCheck {
	return func(r *clientRun) error {
		if len(r.requests) != n {
			return expectf("%s sent %d requests, want %d (error %v): %q",
				r.who, len(r.requests), n, r.err, requestSummary(r))
		}
		return nil
	}
}

// sent checks that request i (from 1) has a card line equal to line.
func sent(i int, line string) clientCheck {
	return func(r *clientRun) error {
		req, err := r.request(i)
		if err != nil {
			return err
		}
		want := r.fx.expand(line)
		if !slices.Contains(lines(req.cards), want) {
			return expectf("%s: request %d has no %q: %q", r.who, i, want, summary(req.cards))
		}
		return nil
	}
}

// notSent checks that no card line of request i starts with prefix; a
// request that was never sent has none.
func notSent(i int, prefix string) clientCheck {
	return func(r *clientRun) error {
		if i > len(r.requests) {
			return nil
		}
		want := r.fx.expand(prefix)
		if j := index(r.requests[i-1].cards, want); j >= 0 {
			return expectf("%s: request %d has %q", r.who, i, r.requests[i-1].cards[j].line)
		}
		return nil
	}
}

// neverSent checks that no request has a card line starting with prefix.
func neverSent(prefix string) clientCheck {
	return func(r *clientRun) error {
		for i := range r.requests {
			if err := notSent(i+1, prefix)(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// sentBefore checks that in request i the first line starting with a
// precedes the first starting with b, both present.
func sentBefore(i int, a, b string) clientCheck {
	return func(r *clientRun) error {
		req, err := r.request(i)
		if err != nil {
			return err
		}
		x, y := index(req.cards, r.fx.expand(a)), index(req.cards, r.fx.expand(b))
		if x < 0 || y < 0 || x >= y {
			return expectf("%s: request %d: want %q (at %d) before %q (at %d): %q",
				r.who, i, a, x, b, y, summary(req.cards))
		}
		return nil
	}
}

// eachRequest applies check to every request, numbered from 1.
func eachRequest(check func(i int, req peerRequest) error) clientCheck {
	return func(r *clientRun) error {
		if len(r.requests) == 0 {
			return expectf("%s sent no request", r.who)
		}
		for i, req := range r.requests {
			if err := check(i+1, req); err != nil {
				return expectf("%s: %v", r.who, err)
			}
		}
		return nil
	}
}

// clientRows checks that a query on the client's repository returns n.
func clientRows(query string, n int) clientCheck {
	return func(r *clientRun) error {
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

// stored checks that the client holds the content of an artifact,
// named by placeholder.
func stored(name string) clientCheck {
	return clientRows("SELECT count(*) FROM blob WHERE uuid='{"+name+"}' AND size>=0", 1)
}

// notStored checks that the client holds neither the artifact nor a
// phantom for it.
func notStored(name string) clientCheck {
	return clientRows("SELECT count(*) FROM blob WHERE uuid='{"+name+"}'", 0)
}

// phantom checks that the client holds a phantom for an artifact.
func phantom(name string) clientCheck {
	return clientRows("SELECT count(*) FROM blob WHERE uuid='{"+name+"}' AND size<0", 1)
}

// noFailure checks that go-libfossil's session returned no error. fossil(1)'s
// exit status is not judged by it.
func noFailure(r *clientRun) error {
	if r.who == "go-libfossil" && r.err != nil {
		return expectf("%s failed: %v", r.who, r.err)
	}
	return nil
}

// failsWith checks that go-libfossil's session failed for the reason
// given, a substring of its error.
func failsWith(reason string) clientCheck {
	return func(r *clientRun) error {
		if r.who != "go-libfossil" {
			return nil
		}
		if r.err == nil || !strings.Contains(r.err.Error(), reason) {
			return expectf("%s: error %v, want one containing %q", r.who, r.err, reason)
		}
		return nil
	}
}

func (r *clientRun) request(i int) (peerRequest, error) {
	if i < 1 || i > len(r.requests) {
		return peerRequest{}, expectf("%s sent %d requests, no request %d (error %v): %q",
			r.who, len(r.requests), i, r.err, requestSummary(r))
	}
	return r.requests[i-1], nil
}

// summary is a request's card lines for a message, without its inventory.
func summary(cards []wireCard) []string {
	var out []string
	for _, line := range lines(cards) {
		if strings.HasPrefix(line, "igot ") && !strings.Contains(line[5:], " ") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func requestSummary(r *clientRun) [][]string {
	var out [][]string
	for _, req := range r.requests {
		out = append(out, summary(req.cards))
	}
	return out
}

// sentPayload checks request i has a card line equal to line, carrying data.
func sentPayload(i int, line, data string) clientCheck {
	return func(r *clientRun) error {
		req, err := r.request(i)
		if err != nil {
			return err
		}
		j := index(req.cards, r.fx.expand(line))
		if j < 0 || req.cards[j].line != r.fx.expand(line) {
			return expectf("%s: request %d has no %q: %q", r.who, i, line, summary(req.cards))
		}
		if want := r.fx.expand(data); string(req.cards[j].payload) != want {
			return expectf("%s: request %d: payload %q, want %q",
				r.who, i, req.cards[j].payload, want)
		}
		return nil
	}
}

// randomnessLast checks Section 3.2: every request ends with a comment of
// random bytes, written as 40 uppercase hex digits (Appendix B.3).
var randomnessLast = eachRequest(func(i int, req peerRequest) error {
	n := len(req.cards)
	if n == 0 || !nonceComment.MatchString(req.cards[n-1].line) {
		return expectf("request %d does not end with randomness: %q", i, summary(req.cards))
	}
	return nil
})

var nonceComment = regexp.MustCompile(`^# [0-9A-F]{40}$`)

// eachCardSent applies check to every card of every request.
func eachCardSent(check func(c wireCard) error) clientCheck {
	return eachRequest(func(i int, req peerRequest) error {
		for _, c := range req.cards {
			if err := check(c); err != nil {
				return expectf("request %d: %v", i, err)
			}
		}
		return nil
	})
}

// cookieAfterOperation checks Table 2: in request i the cookie card comes
// straight after the last push or pull card.
func cookieAfterOperation(i int, cookie string) clientCheck {
	return func(r *clientRun) error {
		req, err := r.request(i)
		if err != nil {
			return err
		}
		last := -1
		for j, c := range req.cards {
			if c.op() == "push" || c.op() == "pull" {
				last = j
			}
		}
		if last < 0 || last+1 >= len(req.cards) || req.cards[last+1].line != "cookie "+cookie {
			return expectf("%s: request %d: cookie %q not straight after the operation: %q",
				r.who, i, cookie, summary(req.cards))
		}
		return nil
	}
}

// expands checks the client can reconstruct an artifact, named by
// placeholder, to its content.
func expands(name string) clientCheck {
	return func(r *clientRun) error {
		repo, err := libfossil.Open(r.repo)
		if err != nil {
			return err
		}
		defer repo.Close()
		rid, ok := content.AvailableByUUID(repo.DB(), r.fx.vars[name])
		if !ok {
			return expectf("%s: %s is not available", r.who, name)
		}
		got, err := content.Expand(repo.DB(), rid)
		if err != nil {
			return expectf("%s: expand %s: %v", r.who, name, err)
		}
		if want := r.fx.vars[name+".data"]; string(got) != want {
			return expectf("%s: %s expands to %q, want %q", r.who, name, got, want)
		}
		return nil
	}
}

// reported checks the client told its user text: go-libfossil in its
// result's errors, fossil(1) on its output.
func reported(text string) clientCheck {
	return func(r *clientRun) error {
		want := r.fx.expand(text)
		var got []string
		if r.result != nil {
			got = r.result.Errors
		}
		if r.output != "" {
			got = []string{r.output}
		}
		for _, line := range got {
			if strings.Contains(line, want) {
				return nil
			}
		}
		return expectf("%s reported %q, want %q", r.who, got, want)
	}
}

// private checks whether the client holds an artifact, or a phantom for
// it, as private.
func private(name string, n int) clientCheck {
	return clientRows("SELECT count(*) FROM private JOIN blob USING(rid) WHERE uuid='{"+
		name+"}'", n)
}

// loginCard returns request i's login card fields and the bytes after its
// line, which its nonce covers.
func (r *clientRun) loginCard(i int) (fields []string, tail string, err error) {
	req, err := r.request(i)
	if err != nil {
		return nil, "", err
	}
	first, rest, _ := strings.Cut(string(req.text), "\n")
	fields = strings.Split(first, " ")
	if len(fields) != 4 || fields[0] != "login" {
		return nil, "", expectf("%s: request %d starts %q, not a login card", r.who, i, first)
	}
	return fields, rest, nil
}

// signedWith checks request 1's login signature is SHA1(NONCE || secret).
func signedWith(secret string) clientCheck {
	return func(r *clientRun) error {
		fields, _, err := r.loginCard(1)
		if err != nil {
			return err
		}
		if want := signature(fields[2], r.fx.expand(secret)); fields[3] != want {
			return expectf("%s: signature %s, want %s", r.who, fields[3], want)
		}
		return nil
	}
}

// atLeast checks the client sent n requests or more.
func atLeast(n int) clientCheck {
	return func(r *clientRun) error {
		if len(r.requests) < n {
			return expectf("%s sent %d requests, want at least %d (error %v): %q",
				r.who, len(r.requests), n, r.err, requestSummary(r))
		}
		return nil
	}
}

// mentions checks the client reported one of reasons: each client's
// wording of the reason a case is about, in go-libfossil's error or
// result, or fossil(1)'s output.
func mentions(reasons ...string) clientCheck {
	return func(r *clientRun) error {
		text := r.output
		if r.err != nil {
			text += r.err.Error()
		}
		if r.result != nil {
			text += strings.Join(r.result.Errors, "\n")
		}
		for _, reason := range reasons {
			if strings.Contains(text, reason) {
				return nil
			}
		}
		return expectf("%s did not report one of %q: %.200q", r.who, reasons, text)
	}
}

// sentPrefix checks request i has a card line starting with prefix.
func sentPrefix(i int, prefix string) clientCheck {
	return func(r *clientRun) error {
		req, err := r.request(i)
		if err != nil {
			return err
		}
		if index(req.cards, r.fx.expand(prefix)) < 0 {
			return expectf("%s: request %d has no %q: %q", r.who, i, prefix, summary(req.cards))
		}
		return nil
	}
}
