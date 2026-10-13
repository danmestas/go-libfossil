package conformance

import "testing"

// syncGaps marks the sync cases go-libfossil is known to fail, by case ID,
// grouped by the code at fault.
var syncGaps = map[string]string{
	// The ci-lock and ci-unlock dispatch.
	"sync.9.4.ci-lock-no-write-gate": "#392",
}

// syncDeviations marks the cases where go-libfossil differs from the draft
// on purpose, by case ID, with the reason.
var syncDeviations = map[string]string{
	"sync.5.2.client-clone-seqno": "the server tolerates a client-sent clone_seqno, " +
		"which no conforming client sends, rather than answering bad command (#255)",
	"sync.8.2.clone-zero-phantom": "Clone fetches the phantoms a clone leaves with a " +
		"follow-up pull, so that a clone it reports done is complete (#255)",
}

// TestSyncGapKeys checks that every marked case exists.
func TestSyncGapKeys(t *testing.T) {
	ids := map[string]bool{}
	for _, c := range syncSuiteCases() {
		ids[c.ID] = true
	}
	for id := range syncGaps {
		if !ids[id] {
			t.Errorf("syncGaps names %s, which is not a case", id)
		}
	}
	for id := range syncDeviations {
		if !ids[id] {
			t.Errorf("syncDeviations names %s, which is not a case", id)
		}
	}
}
