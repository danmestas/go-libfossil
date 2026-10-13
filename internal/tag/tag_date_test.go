package tag_test

import (
	"database/sql"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
)

// The date tag replaces a check-in's event time (draft-fossil-repo-state-01
// Section 4.2, step 6). These tests pin the cases where the draft departs
// from fossil(1) 2.28: a cancel, an unparsable value and a time that is not
// positive all leave the D-card time, and an inherited row sets nothing.

// eventTime reads a check-in's event.mtime and event.omtime as Julian days.
// The casts keep a driver that maps DATETIME columns to time.Time from
// converting them.
func eventTime(t *testing.T, r *repo.Repo, rid int64) (mtime, omtime sql.NullFloat64) {
	t.Helper()
	err := r.DB().QueryRow(
		"SELECT CAST(mtime AS REAL), CAST(omtime AS REAL) FROM event WHERE objid=?", rid).
		Scan(&mtime, &omtime)
	if err != nil {
		t.Fatalf("event row for %d: %v", rid, err)
	}
	return mtime, omtime
}

// addDateTag applies a date tag of the given type and value to rid, n hours
// from now so successive tags are each newer than the last.
func addDateTag(t *testing.T, r *repo.Repo, rid int64, tagType int, value string, n int) {
	t.Helper()
	if _, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: libfossil.FslID(rid),
		TagName:   "date",
		TagType:   tagType,
		Value:     value,
		User:      "testuser",
		Time:      tagTime(n),
	}); err != nil {
		t.Fatalf("tag.AddTag date %q: %v", value, err)
	}
}

// wantDCardTime fails unless rid's event time is its D-card time.
func wantDCardTime(t *testing.T, r *repo.Repo, rid int64) {
	t.Helper()
	mtime, omtime := eventTime(t, r, rid)
	if !omtime.Valid || !mtime.Valid || mtime.Float64 != omtime.Float64 {
		t.Errorf("rid %d: mtime %v, want the D-card time %v", rid, mtime, omtime)
	}
}

func TestDateTagCancelRestoresDCardTime(t *testing.T) {
	r := setupTestRepo(t)
	rid := makeCheckin(t, r, 0, "a.txt", "a", "A")
	addDateTag(t, r, rid, tag.TagSingleton, "2001-02-03T04:05:06", 1)
	if mtime, omtime := eventTime(t, r, rid); mtime.Float64 == omtime.Float64 {
		t.Fatalf("date tag did not move the time: mtime %v", mtime)
	}
	addDateTag(t, r, rid, tag.TagCancel, "", 2)
	wantDCardTime(t, r, rid)
}

func TestDateTagIgnoredValuesKeepDCardTime(t *testing.T) {
	for _, value := range []string{"not-a-date", "0", "-1", "5373484.5"} {
		t.Run(value, func(t *testing.T) {
			r := setupTestRepo(t)
			rid := makeCheckin(t, r, 0, "a.txt", "a", "A")
			addDateTag(t, r, rid, tag.TagSingleton, "2001-02-03T04:05:06", 1)
			addDateTag(t, r, rid, tag.TagSingleton, value, 2)
			wantDCardTime(t, r, rid)
		})
	}
}

// TestDateTagInheritedRowSetsNoTime re-crosslinks after a propagating date
// tag reached B, keeping the inherited row as a second sweep does: only a
// tag applied directly to a check-in sets its time (#372).
func TestDateTagInheritedRowSetsNoTime(t *testing.T) {
	r := setupTestRepo(t)
	ridA := makeCheckin(t, r, 0, "a.txt", "a", "A")
	ridB := makeCheckin(t, r, ridA, "b.txt", "b", "B")
	addDateTag(t, r, ridA, tag.TagPropagating, "2001-02-03T04:05:06", 1)
	for _, clear := range []string{"DELETE FROM event", "DELETE FROM tagxref WHERE srcid > 0"} {
		if _, err := r.DB().Exec(clear); err != nil {
			t.Fatalf("%s: %v", clear, err)
		}
	}
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	wantDCardTime(t, r, ridB)
	if mtime, omtime := eventTime(t, r, ridA); mtime.Float64 == omtime.Float64 {
		t.Errorf("A: propagating date tag did not move its own time: mtime %v", mtime)
	}
}
