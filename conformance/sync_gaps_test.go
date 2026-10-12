package conformance

import "testing"

// syncGaps marks the sync cases go-libfossil is known to fail, by case ID,
// grouped by the code at fault.
var syncGaps = map[string]string{
	// The client sends an unversioned file named by SHA3 to a server older
	// than SHA3 support, where fossil sends the refusal in its place
	// (internal/sync/client.go buildUVFileCards).
	"sync.7.6.client-uv-gate": "#303",
	// The read gates treat only o, i and Admin as Read, where fossil's
	// trunk and the s7.3 erratum let Clone imply it
	// (internal/auth/auth.go:42).
	"sync.7.3.uvgimme-clone-implies-read": "#312",
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
