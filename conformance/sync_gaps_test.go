package conformance

import "testing"

// syncGaps marks the sync cases go-libfossil is known to fail, by case ID,
// grouped by the code at fault.
var syncGaps = map[string]string{
	"sync.10.1.integrity-error-continues": "#421",
}

// syncDeviations marks the cases where go-libfossil differs from the draft
// on purpose, by case ID, with the reason.
var syncDeviations = map[string]string{}

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
