package libfossil

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// TestTimelineEnumeratesEveryEvent is the acceptance criterion for the bug
// this issue fixes: Timeline with no type filter must return every row of
// event, including check-ins that are not first-parent ancestors of any
// single start rid — the thing the old Timeline (now Ancestry) could
// never see.
func TestTimelineEnumeratesEveryEvent(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid1, _, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "trunk root", User: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(CommitOpts{
		ParentID: rid1,
		Files:    []FileToCommit{{Name: "a.txt", Content: []byte("v2")}},
		Comment:  "trunk tip", User: "test",
	}); err != nil {
		t.Fatal(err)
	}
	branchRID, branchUUID, err := r.Commit(CommitOpts{
		ParentID: rid1,
		Files:    []FileToCommit{{Name: "a.txt", Content: []byte("v3")}},
		Comment:  "sibling branch head", User: "test",
		Tags: []TagSpec{{Name: "branch", Value: "feature-x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if branchRID <= 0 {
		t.Fatal("branch commit returned rid <= 0")
	}

	var wantCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM event").Scan(&wantCount); err != nil {
		t.Fatal(err)
	}

	entries, err := r.Timeline(TimelineOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != wantCount {
		t.Fatalf("Timeline returned %d entries, want %d (SELECT count(*) FROM event)", len(entries), wantCount)
	}

	found := false
	for _, e := range entries {
		if e.UUID == branchUUID {
			found = true
		}
	}
	if !found {
		t.Fatalf("sibling branch head %s missing from Timeline output", branchUUID)
	}
}

// TestTimelineTypeFilter covers Type=EventKindCheckin returning exactly
// the check-in rows.
func TestTimelineTypeFilter(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, _, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "checkin", User: "test",
	}); err != nil {
		t.Fatal(err)
	}

	var wantCi int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&wantCi); err != nil {
		t.Fatal(err)
	}

	entries, err := r.Timeline(TimelineOpts{Type: EventKindCheckin})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != wantCi {
		t.Fatalf("Timeline(Type=ci) returned %d entries, want %d", len(entries), wantCi)
	}
	for _, e := range entries {
		if e.Kind != EventKindCheckin {
			t.Fatalf("entry kind = %q, want %q", e.Kind, EventKindCheckin)
		}
	}
}

// TestTimelineOrderingAndLimit covers (mtime DESC, rid DESC) ordering and
// that Limit both bounds the result and is reachable — the defining
// symptom of the original bug was a limit that never bound.
func TestTimelineOrderingAndLimit(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var parent int64
	var rids []int64
	for i := 0; i < 5; i++ {
		rid, _, err := r.Commit(CommitOpts{
			ParentID: parent,
			Files:    []FileToCommit{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
			Comment:  fmt.Sprintf("commit %d", i), User: "test",
			Time: time.Date(2024, 1, 15, 10, i, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		parent = rid
		rids = append(rids, rid)
	}

	entries, err := r.Timeline(TimelineOpts{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	// Newest first: the last two commits, newest rid first.
	if entries[0].RID != rids[4] || entries[1].RID != rids[3] {
		t.Fatalf("order = [%d, %d], want [%d, %d]", entries[0].RID, entries[1].RID, rids[4], rids[3])
	}
}

// TestTimelinePaginationExactlyOnce covers the acceptance criterion that
// paginating the full set yields each event exactly once, including when
// several events share an identical mtime.
func TestTimelinePaginationExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	sameTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	var parent int64
	var allUUIDs []string
	for i := 0; i < 6; i++ {
		rid, uuid, err := r.Commit(CommitOpts{
			ParentID: parent,
			Files:    []FileToCommit{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
			Comment:  fmt.Sprintf("commit %d", i), User: "test",
			Time: sameTime, // every commit shares the exact same mtime
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		parent = rid
		allUUIDs = append(allUUIDs, uuid)
	}

	const pageSize = 2
	seen := make(map[string]int)
	var total int
	var before time.Time
	var after FslID
	for page := 0; page < 10; page++ { // explicit bound on the pagination loop
		entries, err := r.Timeline(TimelineOpts{Limit: pageSize, Before: before, After: after})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			seen[e.UUID]++
			total++
		}
		last := entries[len(entries)-1]
		before = last.Time
		after = FslID(last.RID)
	}

	if total != len(allUUIDs) {
		t.Fatalf("paginated total = %d, want %d", total, len(allUUIDs))
	}
	for _, uuid := range allUUIDs {
		if seen[uuid] != 1 {
			t.Fatalf("event %s seen %d times, want exactly 1", uuid, seen[uuid])
		}
	}
}
