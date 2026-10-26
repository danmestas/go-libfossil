package conformance

import (
	"fmt"
	"strings"
	"testing"
)

// Case is what every conformance case carries, whatever it checks.
type Case struct {
	// ID names the case: "<suite>.<section>.<what>", e.g.
	// "artifact.4.7.6.unsorted". Where libfossil's suite has the same case,
	// the ID matches its, so the two implementations can be compared.
	ID string
	// Req is the requirement ID from the suite's requirements table.
	Req string
	// Gap is the issue tracking a known failure, e.g. "#270". The case still
	// runs: while it fails it is skipped, and once it passes it fails until
	// Gap is removed.
	Gap string
	// Deviation says why go-libfossil deliberately does not do what the
	// draft requires. It behaves like Gap, but there is nothing to fix.
	Deviation string
}

// outcome is how one case run is reported.
type outcome int

const (
	outcomePass outcome = iota
	outcomeFail
	outcomeSkip
)

// judge decides how a case is reported, given the problem its check found
// (nil when go-libfossil conforms). It is kept apart from check so the gap
// rules are testable without a *testing.T.
func judge(c Case, problem error) (outcome, string) {
	if c.ID == "" {
		panic("conformance: case without an ID")
	}
	if c.Gap != "" && c.Deviation != "" {
		panic("conformance: case " + c.ID + " is marked both Gap and Deviation")
	}
	marked, mark := c.Gap, "gap "+c.Gap
	if c.Deviation != "" {
		marked, mark = c.Deviation, "deviation: "+c.Deviation
	}
	if problem == nil {
		if marked == "" {
			return outcomePass, ""
		}
		return outcomeFail, fmt.Sprintf("%s now conforms; remove the mark (%s)", c.ID, mark)
	}
	if marked == "" {
		return outcomeFail, fmt.Sprintf("%s [%s]: %v", c.ID, c.Req, problem)
	}
	return outcomeSkip, fmt.Sprintf("%s (%s): %v", c.ID, mark, problem)
}

// check runs one case as a subtest. conforms returns nil when go-libfossil
// does what the case expects, or a one-line description of what it did
// instead. It is handed the subtest's t, so a fixture that skips or fails
// (fossil(1) absent, a setup error) ends that case alone; calling Skip or
// FailNow on the parent from inside the subtest fails the whole run.
func check(t *testing.T, c Case, conforms func(t *testing.T) error) {
	t.Helper()
	t.Run(c.ID, func(t *testing.T) {
		t.Helper()
		result, message := judge(c, conforms(t))
		switch result {
		case outcomePass:
		case outcomeFail:
			t.Fatal(message)
		case outcomeSkip:
			t.Skip(message)
		default:
			panic(fmt.Sprintf("conformance: unknown outcome %d", result))
		}
	})
}

// expectf builds the problem a check reports: what was expected, what
// happened.
func expectf(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if strings.ContainsRune(message, '\n') {
		message = strings.ReplaceAll(message, "\n", `\n`)
	}
	return fmt.Errorf("%s", message)
}
