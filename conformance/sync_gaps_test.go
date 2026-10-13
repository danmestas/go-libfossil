package conformance

import "testing"

// syncGaps marks the sync cases go-libfossil is known to fail, by case ID,
// grouped by the code at fault.
var syncGaps = map[string]string{
	// The client sends an unversioned file named by SHA3 to a server older
	// than SHA3 support, where fossil sends the refusal in its place
	// (internal/sync/client.go buildUVFileCards).
	"sync.7.6.client-uv-gate": "#303",
	// A payload cut short by the end of the body fails the whole decode:
	// the server answers HTTP 400 and the client keeps none of the reply
	// (internal/xfer decode), where Section 5.1 makes it a handler failure.
	"sync.5.1.truncated-payload-server": "#373",
	"sync.5.1.truncated-payload":        "#373",
	// The server's card loop (internal/sync/handler.go): a file card after
	// a refused push is refused without a reset and does not end card
	// processing (Section 6.6); a failed login skips the end-of-request
	// steps and the closing comment (6.4); errors N counts the unknown-card
	// error and the private-content refusal (5.3).
	"sync.6.6.file-after-refused-push":      "#374",
	"sync.5.3.errors-counts-reset":          "#374",
	"sync.6.4.failure-end-of-request":       "#374",
	"sync.5.3.errors-skips-bad-command":     "#374",
	"sync.5.3.errors-skips-private-refusal": "#374",
	// handleGimme skips private content outside a private sync, where
	// Section 8.6 answers a peer dated 2020-04-13 or later with igot HASH 1.
	"sync.8.6.private-gimme-igot": "#375",
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
