package conformance

// The sync suite checks draft-fossil-sync-protocol-00. It drives the real
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
// be, so a case cannot drift from fossil unnoticed; a case that knowingly
// differs from fossil says why in its fossil field. Each case's input
// differs from an exchange another case shows to be handled normally in
// one respect, and each refusal names its reason, so a case fails when
// something other than its rule decides the outcome.

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
