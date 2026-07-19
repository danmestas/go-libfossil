package manifest

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/deck"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
)

// TestTimelineEnumeratesAllEvents is the core regression for the bug
// Timeline replaces Ancestry-as-Timeline for: enumeration must see every
// row of the event table, not just first-parent ancestors of one start rid.
func TestTimelineEnumeratesAllEvents(t *testing.T) {
	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "first", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v2")}},
		Comment: "second", User: "testuser", Parent: rid1,
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	// A sibling branch head off the same parent — not a first-parent
	// ancestor of the "second" trunk tip above.
	Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v3")}},
		Comment: "branch commit", User: "testuser", Parent: rid1,
		Time: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
		Tags: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feature-x"},
		},
	})

	var wantCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM event").Scan(&wantCount); err != nil {
		t.Fatalf("count query: %v", err)
	}

	entries, err := Timeline(r, TimelineOpts{})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != wantCount {
		t.Fatalf("Timeline returned %d entries, want %d (SELECT count(*) FROM event)", len(entries), wantCount)
	}
}

// TestTimelineIncludesNonTrunkBranches is the acceptance criterion that
// distinguishes Timeline from the old ancestry-only behavior directly:
// a sibling branch head must appear even though it is not reachable by
// following primary parents from any single start rid.
func TestTimelineIncludesNonTrunkBranches(t *testing.T) {
	r := setupTestRepo(t)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "first", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v2")}},
		Comment: "trunk tip", User: "testuser", Parent: rid1,
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	_, branchUUID, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v3")}},
		Comment: "branch commit", User: "testuser", Parent: rid1,
		Time: time.Date(2024, 1, 15, 9, 30, 0, 0, time.UTC),
		Tags: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feature-x"},
		},
	})

	entries, err := Timeline(r, TimelineOpts{})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.UUID == branchUUID {
			found = true
		}
	}
	if !found {
		t.Fatalf("branch commit %s not present in Timeline output: %+v", branchUUID, entries)
	}
}

// TestTimelineFiltersByType covers the canonical default (all kinds) and
// an explicit ci filter, per src/timeline.c:timeline_cmd()'s
// `zType && zType[0]!='a'` guard.
func TestTimelineFiltersByType(t *testing.T) {
	r := setupTestRepo(t)
	Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "checkin", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})

	wikiTime := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)
	d := &deck.Deck{
		Type: deck.Wiki,
		L:    "TestPage",
		U:    deck.Str("testuser"),
		W:    []byte("wiki content"),
		D:    wikiTime,
	}
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, _, err := blob.Store(r.DB(), manifestBytes); err != nil {
		t.Fatalf("store wiki manifest: %v", err)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	var wantCi, wantAll int
	r.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&wantCi)
	r.DB().QueryRow("SELECT count(*) FROM event").Scan(&wantAll)
	if wantAll == wantCi {
		t.Fatalf("test fixture did not produce a mixed-type event table (all=%d ci=%d)", wantAll, wantCi)
	}

	all, err := Timeline(r, TimelineOpts{})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(all) != wantAll {
		t.Fatalf("Timeline() len = %d, want %d", len(all), wantAll)
	}

	ciOnly, err := Timeline(r, TimelineOpts{Type: libfossil.EventKindCheckin})
	if err != nil {
		t.Fatalf("Timeline(Type=ci): %v", err)
	}
	if len(ciOnly) != wantCi {
		t.Fatalf("Timeline(Type=ci) len = %d, want %d", len(ciOnly), wantCi)
	}
	for _, e := range ciOnly {
		if e.Kind != libfossil.EventKindCheckin {
			t.Fatalf("Timeline(Type=ci) returned kind %q", e.Kind)
		}
	}
}

// TestTimelineOrdering asserts (mtime DESC, rid DESC) — the deliberate
// divergence from canonical fossil's bare `ORDER BY event.mtime DESC`.
// Two entries share an identical mtime; the higher rid must sort first.
func TestTimelineOrdering(t *testing.T) {
	r := setupTestRepo(t)
	sameTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	rid1, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "one", User: "testuser", Time: sameTime,
	})
	rid2, _, _ := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("v2")}},
		Comment: "two", User: "testuser", Parent: rid1, Time: sameTime,
	})

	entries, err := Timeline(r, TimelineOpts{})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].RID != rid2 || entries[1].RID != rid1 {
		t.Fatalf("order = [%d, %d], want [%d, %d] (rid DESC tie-break on equal mtime)",
			entries[0].RID, entries[1].RID, rid2, rid1)
	}
}

// TestTimelinePagination is the acceptance criterion that paginating the
// full set in pages of N yields each event exactly once — no duplicates
// at boundaries, no gaps — even when several events share an identical
// mtime, which is exactly where canonical fossil's bare-timestamp cursor
// can repeat or skip rows.
func TestTimelinePagination(t *testing.T) {
	r := setupTestRepo(t)
	sameTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	otherTime := time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC)

	var allUUIDs []string
	var parent libfossil.FslID
	for i := 0; i < 5; i++ {
		ts := sameTime
		if i >= 3 {
			ts = otherTime // last two entries get a distinct, earlier mtime
		}
		rid, uuid, err := Checkin(r, CheckinOpts{
			Files:   []File{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
			Comment: fmt.Sprintf("commit %d", i), User: "testuser",
			Parent: parent, Time: ts,
		})
		if err != nil {
			t.Fatalf("Checkin %d: %v", i, err)
		}
		parent = rid
		allUUIDs = append(allUUIDs, uuid)
	}

	const pageSize = 2
	// Exactly ceil(n/pageSize) pages: a fixed bound, not a "while non-empty"
	// loop, so a cursor that fails to advance fails this test immediately
	// instead of hanging it.
	maxPages := (len(allUUIDs) + pageSize - 1) / pageSize
	seen := make(map[string]int)
	var order []string
	var after libfossil.Cursor
	for page := 0; page < maxPages; page++ {
		entries, err := Timeline(r, TimelineOpts{Limit: pageSize, After: after})
		if err != nil {
			t.Fatalf("Timeline page %d: %v", page, err)
		}
		if len(entries) == 0 {
			t.Fatalf("page %d returned 0 entries after only %d of %d events seen — cursor did not advance", page, len(order), len(allUUIDs))
		}
		for _, e := range entries {
			seen[e.UUID]++
			order = append(order, e.UUID)
		}
		after = entries[len(entries)-1].Cursor
	}

	if len(order) != len(allUUIDs) {
		t.Fatalf("paginated total = %d entries across exactly %d pages, want %d: %v", len(order), maxPages, len(allUUIDs), order)
	}
	for _, uuid := range allUUIDs {
		if seen[uuid] != 1 {
			t.Fatalf("event %s seen %d times across %d pages, want exactly 1", uuid, seen[uuid], maxPages)
		}
	}

	// The defining symptom of a broken cursor is non-termination: every row
	// enumerated correctly, but the cursor never signals exhaustion. One
	// more call past the last real page must come back empty.
	final, err := Timeline(r, TimelineOpts{Limit: pageSize, After: after})
	if err != nil {
		t.Fatalf("final page: %v", err)
	}
	if len(final) != 0 {
		t.Fatalf("pagination did not terminate: page after the last expected page returned %d entries, want 0: %v", len(final), final)
	}
}

// TestTimelinePaginationSubMillisecondBoundary is the regression test for
// a cursor predicate that only distinguishes rows several milliseconds
// apart via a tolerance window: rid stops being a true tie-break at exact
// mtime equality and becomes a filter applied to rows that are not ties,
// which can skip a row entirely, duplicate another, or fail to advance
// the cursor at all. rid assignment order (checkin/insertion order) is
// deliberately decoupled from mtime order here — the two need not agree
// in general (backdated commits, imports) — because a predicate bug that
// only manifests when they disagree would pass unnoticed against a
// fixture where they happen to move together.
func TestTimelinePaginationSubMillisecondBoundary(t *testing.T) {
	r := setupTestRepo(t)

	const n = 6
	const pageSize = 2
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	var parent libfossil.FslID
	var allUUIDs []string
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(n-1-i) * 3 * time.Millisecond) // 3ms apart, descending as rid ascends
		rid, uuid, err := Checkin(r, CheckinOpts{
			Files:   []File{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
			Comment: fmt.Sprintf("commit %d", i), User: "testuser",
			Parent: parent, Time: ts,
		})
		if err != nil {
			t.Fatalf("Checkin %d: %v", i, err)
		}
		parent = rid
		allUUIDs = append(allUUIDs, uuid)
	}

	maxPages := (n + pageSize - 1) / pageSize // exactly ceil(n/pageSize) = 3
	seen := make(map[string]int)
	var order []string
	var after libfossil.Cursor
	for page := 0; page < maxPages; page++ {
		entries, err := Timeline(r, TimelineOpts{Limit: pageSize, After: after})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(entries) == 0 {
			t.Fatalf("page %d returned 0 entries after only %d of %d events seen (order so far: %v) — cursor did not advance", page, len(order), n, order)
		}
		for _, e := range entries {
			seen[e.UUID]++
			order = append(order, e.UUID)
		}
		after = entries[len(entries)-1].Cursor
	}

	if len(order) != n {
		t.Fatalf("paginated total = %d entries across exactly %d pages, want %d: order=%v", len(order), maxPages, n, order)
	}
	for _, uuid := range allUUIDs {
		if seen[uuid] != 1 {
			t.Fatalf("event %s seen %d times across %d pages (bound=ceil(%d/%d)), want exactly 1", uuid, seen[uuid], maxPages, n, pageSize)
		}
	}

	// Non-termination is the actual user-facing failure mode: every row
	// enumerated correctly but the cursor never signals exhaustion. Assert
	// the page past the last expected one comes back empty.
	final, err := Timeline(r, TimelineOpts{Limit: pageSize, After: after})
	if err != nil {
		t.Fatalf("final page: %v", err)
	}
	if len(final) != 0 {
		t.Fatalf("pagination did not terminate: page after the last expected page returned %d entries, want 0: %v", len(final), final)
	}
}

// TestTimelineLimit covers that Limit both bounds the result and is
// reachable — the original bug's defining symptom was a limit that never
// bound because the (wrong) walk exhausted first.
func TestTimelineLimit(t *testing.T) {
	r := setupTestRepo(t)
	var parent libfossil.FslID
	for i := 0; i < 5; i++ {
		rid, _, _ := Checkin(r, CheckinOpts{
			Files:   []File{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
			Comment: fmt.Sprintf("commit %d", i), User: "testuser",
			Parent: parent, Time: time.Date(2024, 1, 15, 10, i, 0, 0, time.UTC),
		})
		parent = rid
	}

	entries, err := Timeline(r, TimelineOpts{Limit: 2})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2 (Limit must bind and be reachable)", len(entries))
	}
}

// TestTimelineInvalidType covers the TigerStyle "fail fast and loudly on
// programmer error" requirement: an EventKind that is not one of the six
// recognized codes (or the zero value) is a caller bug, not unusual-but-
// valid data.
func TestTimelineInvalidType(t *testing.T) {
	r := setupTestRepo(t)
	defer func() {
		if recover() == nil {
			t.Fatal("Timeline did not panic on an invalid EventKind")
		}
	}()
	Timeline(r, TimelineOpts{Type: libfossil.EventKind("bogus")})
}

// TestTimelineNilRepoPanics matches the codebase's `panic("pkg.Func: arg
// must not be nil")` idiom for programmer error (see internal/blob/blob.go).
func TestTimelineNilRepoPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Timeline did not panic on a nil repo")
		}
	}()
	Timeline(nil, TimelineOpts{})
}
