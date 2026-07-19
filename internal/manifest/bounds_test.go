package manifest

import (
	"testing"
	"time"
)

// TestLogBoundedAgainstPlinkCycle covers the TigerStyle requirement that
// the ancestry walk's loop has an explicit, defensible bound: a corrupt or
// cyclic plink table must produce an error, not hang the process. The
// bound is shrunk for the duration of the test so this runs in
// milliseconds instead of walking maxAncestryDepth's real-world value.
func TestLogBoundedAgainstPlinkCycle(t *testing.T) {
	orig := maxAncestryDepth
	maxAncestryDepth = 3
	t.Cleanup(func() { maxAncestryDepth = orig })

	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "first", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v2")}},
		Comment: "second", User: "testuser", Parent: rid1,
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	// Corrupt the plink table into a 2-cycle: rid1's primary parent becomes
	// rid2, while rid2's primary parent is already rid1 from the checkin
	// above. Following isprim=1 from rid2 now loops forever without a bound.
	if _, err := r.DB().Exec(
		"INSERT INTO plink(pid, cid, isprim, mtime) VALUES(?, ?, 1, 0)",
		rid2, rid1,
	); err != nil {
		t.Fatalf("corrupt plink: %v", err)
	}

	_, err := Log(r, LogOpts{Start: rid2})
	if err == nil {
		t.Fatal("Log did not error on a cyclic plink chain")
	}
}

// TestTimelineBoundedRowGuard covers the same requirement on the
// enumeration side: an unbounded (Limit <= 0) query against more rows
// than the configured guard allows must error rather than grow without
// limit.
func TestTimelineBoundedRowGuard(t *testing.T) {
	orig := maxTimelineRows
	maxTimelineRows = 2
	t.Cleanup(func() { maxTimelineRows = orig })

	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "one", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v2")}},
		Comment: "two", User: "testuser", Parent: rid1,
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v3")}},
		Comment: "three", User: "testuser", Parent: rid2,
		Time: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
	})

	_, err := Timeline(r, TimelineOpts{})
	if err == nil {
		t.Fatal("Timeline did not error when row count exceeded maxTimelineRows")
	}
}
