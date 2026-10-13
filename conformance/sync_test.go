package conformance

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
)

// The sync suite checks draft-fossil-sync-protocol-01. It drives the real
// code end to end, with nothing mocked but the far end of the wire:
//
//   - a server case is raw request bytes sent to Repo.XferHandler, and the
//     same request sent to fossil(1) through `fossil http`, each on its own
//     copy of the fixture repository;
//   - a client case is a scripted peer answering the public Sync and Clone
//     through a FramedTransport; for the cases marked fc, fossil(1)'s
//     client talks to the same script over HTTP;
//   - an HTTP case is a scripted httptest server, for redirects, status
//     codes and media types;
//   - an interop case is a whole session between go-libfossil and fossil(1).
//
// Every expectation is checked against fossil(1) on every run where it can
// be, so a case cannot drift from fossil unnoticed, and a disagreement is
// fatal whatever the case's mark; a case that knowingly differs from
// fossil, or cannot be sent to it, says why in its fossil or notSent
// field. Each case's input
// differs from an exchange another case shows to be handled normally in
// one respect, and each refusal names its reason, so a case fails when
// something other than its rule decides the outcome.

// checkSync runs one sync case as a subtest. run returns two problems:
// fatal, which fails the case whatever its mark (fossil(1) disagreeing
// with the case, a deviation other than the one named, or the harness
// itself failing), and conforms, go-libfossil's own departure from the
// case, which the mark judges as check does.
func checkSync(t *testing.T, c Case, run func() (fatal, conforms error)) {
	t.Helper()
	t.Run(c.ID, func(t *testing.T) {
		t.Helper()
		fatal, conforms := run()
		if fatal != nil {
			t.Fatalf("%s [%s]: %v", c.ID, c.Req, fatal)
		}
		result, message := judge(c, conforms)
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

// quietLogs discards the slog output of go-libfossil's server for the rest
// of a test: it logs every request it refuses, and many cases are refused
// requests.
func quietLogs(t *testing.T) {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
}

// markSync sets a case's Gap or Deviation from the suite's maps.
func markSync(c Case) Case {
	c.Gap = syncGaps[c.ID]
	c.Deviation = syncDeviations[c.ID]
	return c
}

// syncSuiteCases is what the coverage test audits for this suite.
func syncSuiteCases() []Case {
	var all []Case
	for _, c := range syncServerCases() {
		all = append(all, markSync(c.Case))
	}
	for _, c := range syncClientCases() {
		all = append(all, markSync(c.Case))
	}
	for _, c := range syncHTTPCases() {
		all = append(all, markSync(c.Case))
	}
	for _, c := range syncInteropCases() {
		all = append(all, markSync(c.Case))
	}
	return all
}
