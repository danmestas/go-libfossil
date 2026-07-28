package tag_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

func setupTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestAddTag(t *testing.T) {
	r := setupTestRepo(t)

	// Create a checkin to tag
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Add a singleton tag
	tagRid, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: rid,
		TagName:   "testlabel",
		TagType:   tag.TagSingleton,
		Value:     "myvalue",
		User:      "testuser",
		Time:      time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("tag.AddTag: %v", err)
	}
	if tagRid <= 0 {
		t.Fatalf("tagRid = %d, want > 0", tagRid)
	}

	// Verify tagxref has the correct entry
	var tagtype int
	var value string
	err = r.DB().QueryRow(
		`SELECT tagtype, value FROM tagxref
		 JOIN tag ON tag.tagid = tagxref.tagid
		 WHERE tag.tagname = ? AND tagxref.rid = ?`,
		"testlabel", rid,
	).Scan(&tagtype, &value)
	if err != nil {
		t.Fatalf("tagxref query: %v", err)
	}
	if tagtype != tag.TagSingleton {
		t.Fatalf("tagtype = %d, want %d (singleton)", tagtype, tag.TagSingleton)
	}
	if value != "myvalue" {
		t.Fatalf("value = %q, want %q", value, "myvalue")
	}
}

func TestCancelTag(t *testing.T) {
	r := setupTestRepo(t)

	// Create a checkin (auto-gets sym-trunk tag via propagation in manifest)
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Cancel the sym-trunk tag
	cancelRid, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: rid,
		TagName:   "sym-trunk",
		TagType:   tag.TagCancel,
		User:      "testuser",
		Time:      time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("tag.AddTag cancel: %v", err)
	}
	if cancelRid <= 0 {
		t.Fatalf("cancelRid = %d, want > 0", cancelRid)
	}

	// Verify tagxref has tagtype=0 (cancel)
	var tagtype int
	err = r.DB().QueryRow(
		`SELECT tagtype FROM tagxref
		 JOIN tag ON tag.tagid = tagxref.tagid
		 WHERE tag.tagname = ? AND tagxref.rid = ?`,
		"sym-trunk", rid,
	).Scan(&tagtype)
	if err != nil {
		t.Fatalf("tagxref query: %v", err)
	}
	if tagtype != tag.TagCancel {
		t.Fatalf("tagtype = %d, want %d (cancel)", tagtype, tag.TagCancel)
	}
}

// makeCheckin is a test helper that creates a checkin with one file.
func makeCheckin(t *testing.T, r *repo.Repo, parent int64, name, content, comment string) int64 {
	t.Helper()
	return makeCheckinAt(t, r, parent, name, content, comment, time.Now().UTC())
}

// tagTime returns a tag-application time n hours from now, for fixtures that
// apply tags on top of check-ins made by makeCheckin.
//
// Competing applications have to land on distinct julian mtimes. TimeToJulian
// resolves to roughly 47us at present-day julian values -- a float64 carries
// about 15 significant digits and a julian day number is already ~2.46e6 --
// so two time.Now() calls in the same test can collapse onto one mtime. Tag
// application is mtime-guarded (see superseded), and RFC draft-fossil-repo-
// state-00 §5.3 step 1 notes that the equal-mtime case resolves by write order
// and is "implementation-defined rather than crosslink-order-independent".
// Spacing fixtures by hours keeps them off that tie entirely, and being ahead
// of now keeps them newer than the check-ins they tag.
func tagTime(n int) time.Time { return time.Now().UTC().Add(time.Duration(n) * time.Hour) }

// makeCheckinAt is makeCheckin with an explicit check-in time. A check-in
// declaring a branch writes its own branch/sym-trunk tagxref rows stamped with
// that time, and tag application is mtime-guarded (see superseded), so a test
// that later applies one of those tags by hand has to place the check-in
// before the tag rather than leaving it at time.Now().
func makeCheckinAt(t *testing.T, r *repo.Repo, parent int64, name, content, comment string, when time.Time) int64 {
	t.Helper()
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: name, Content: []byte(content)}},
		Comment: comment,
		User:    "testuser",
		Parent:  libfossil.FslID(parent),
		Time:    when,
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	return int64(rid)
}

func TestPropagateChain(t *testing.T) {
	r := setupTestRepo(t)

	// Create chain A→B→C
	ridA := makeCheckin(t, r, 0, "a.txt", "content A", "commit A")
	ridB := makeCheckin(t, r, ridA, "b.txt", "content B", "commit B")
	ridC := makeCheckin(t, r, ridB, "c.txt", "content C", "commit C")

	// Add propagating "branch" tag to A with value "feature"
	_, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: libfossil.FslID(ridA),
		TagName:   "branch",
		TagType:   tag.TagPropagating,
		Value:     "feature",
		User:      "testuser",
		Time:      tagTime(1),
	})
	if err != nil {
		t.Fatalf("tag.AddTag: %v", err)
	}

	// Verify B has the propagated tag (srcid=0, correct value)
	var srcidb, tagtypeB int
	var valueB string
	err = r.DB().QueryRow(`
		SELECT srcid, tagtype, value FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'branch' AND tagxref.rid = ?
	`, ridB).Scan(&srcidb, &tagtypeB, &valueB)
	if err != nil {
		t.Fatalf("tagxref query for B: %v", err)
	}
	if srcidb != 0 {
		t.Errorf("B srcid = %d, want 0 (propagated)", srcidb)
	}
	if tagtypeB != tag.TagPropagating {
		t.Errorf("B tagtype = %d, want %d", tagtypeB, tag.TagPropagating)
	}
	if valueB != "feature" {
		t.Errorf("B value = %q, want %q", valueB, "feature")
	}

	// Verify C has the propagated tag
	var srcidC, tagtypeC int
	var valueC string
	err = r.DB().QueryRow(`
		SELECT srcid, tagtype, value FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'branch' AND tagxref.rid = ?
	`, ridC).Scan(&srcidC, &tagtypeC, &valueC)
	if err != nil {
		t.Fatalf("tagxref query for C: %v", err)
	}
	if srcidC != 0 {
		t.Errorf("C srcid = %d, want 0 (propagated)", srcidC)
	}
	if tagtypeC != tag.TagPropagating {
		t.Errorf("C tagtype = %d, want %d", tagtypeC, tag.TagPropagating)
	}
	if valueC != "feature" {
		t.Errorf("C value = %q, want %q", valueC, "feature")
	}
}

func TestCancelPropagation(t *testing.T) {
	r := setupTestRepo(t)

	// Create chain A→B→C
	ridA := makeCheckin(t, r, 0, "a.txt", "content A", "commit A")
	ridB := makeCheckin(t, r, ridA, "b.txt", "content B", "commit B")
	ridC := makeCheckin(t, r, ridB, "c.txt", "content C", "commit C")

	// Add propagating tag to A
	_, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: libfossil.FslID(ridA),
		TagName:   "testprop",
		TagType:   tag.TagPropagating,
		Value:     "propvalue",
		User:      "testuser",
		Time:      tagTime(1),
	})
	if err != nil {
		t.Fatalf("tag.AddTag propagating: %v", err)
	}

	// Cancel at B
	_, err = tag.AddTag(r, tag.TagOpts{
		TargetRID: libfossil.FslID(ridB),
		TagName:   "testprop",
		TagType:   tag.TagCancel,
		User:      "testuser",
		Time:      tagTime(2),
	})
	if err != nil {
		t.Fatalf("tag.AddTag cancel: %v", err)
	}

	// Verify B has no active tags (count of tagtype>0 should be 0)
	var countB int
	err = r.DB().QueryRow(`
		SELECT COUNT(*) FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'testprop' AND tagxref.rid = ? AND tagxref.tagtype > 0
	`, ridB).Scan(&countB)
	if err != nil {
		t.Fatalf("count query for B: %v", err)
	}
	if countB != 0 {
		t.Errorf("B has %d active tags, want 0", countB)
	}

	// Verify C has no tagxref row for this tag at all
	var countC int
	err = r.DB().QueryRow(`
		SELECT COUNT(*) FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'testprop' AND tagxref.rid = ?
	`, ridC).Scan(&countC)
	if err != nil {
		t.Fatalf("count query for C: %v", err)
	}
	if countC != 0 {
		t.Errorf("C has %d tagxref rows, want 0", countC)
	}
}

func TestPropagateBgcolor(t *testing.T) {
	r := setupTestRepo(t)

	// Create A→B
	ridA := makeCheckin(t, r, 0, "a.txt", "content A", "commit A")
	ridB := makeCheckin(t, r, ridA, "b.txt", "content B", "commit B")

	// Run crosslink to populate event table
	_, err := manifest.Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	// Add propagating "bgcolor" tag to A
	_, err = tag.AddTag(r, tag.TagOpts{
		TargetRID: libfossil.FslID(ridA),
		TagName:   "bgcolor",
		TagType:   tag.TagPropagating,
		Value:     "#ff0000",
		User:      "testuser",
		Time:      tagTime(1),
	})
	if err != nil {
		t.Fatalf("tag.AddTag bgcolor: %v", err)
	}

	// Verify event.bgcolor updated at B
	var bgcolor string
	err = r.DB().QueryRow("SELECT bgcolor FROM event WHERE objid=?", ridB).Scan(&bgcolor)
	if err != nil {
		t.Fatalf("event query for B: %v", err)
	}
	if bgcolor != "#ff0000" {
		t.Errorf("B bgcolor = %q, want %q", bgcolor, "#ff0000")
	}
}

func TestApplyTag(t *testing.T) {
	r := setupTestRepo(t)

	// The check-ins predate the tag applied below, so its mtime is the newest
	// one standing for (sym-trunk, A) and the application is not superseded.
	checkedInAt := time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC)
	ridA := makeCheckinAt(t, r, 0, "a.txt", "aaa", "commit A", checkedInAt)
	ridB := makeCheckinAt(t, r, ridA, "a.txt", "bbb", "commit B", checkedInAt.Add(time.Minute))

	err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: libfossil.FslID(ridA),
		SrcRID:    999,
		TagName:   "sym-trunk",
		TagType:   tag.TagPropagating,
		Value:     "",
		MTime:     libfossil.TimeToJulian(time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("tag.ApplyTag: %v", err)
	}

	// Verify tagxref at A has srcid=999.
	var srcid int64
	r.DB().QueryRow(
		"SELECT srcid FROM tagxref JOIN tag USING(tagid) WHERE tagname='sym-trunk' AND rid=?", ridA,
	).Scan(&srcid)
	if srcid != 999 {
		t.Errorf("A srcid=%d, want 999", srcid)
	}

	// Verify propagated to B with srcid=0.
	r.DB().QueryRow(
		"SELECT srcid FROM tagxref JOIN tag USING(tagid) WHERE tagname='sym-trunk' AND rid=?", ridB,
	).Scan(&srcid)
	if srcid != 0 {
		t.Errorf("B srcid=%d, want 0 (propagated)", srcid)
	}
}

// TestPropagateAllOrderingAdjacentMillisecond exercises the `tagxref.mtime < ?`
// ordering comparison in propagate() — the one confirmed ScanJulianDay consumer
// that makes an ordering decision — driven through PropagateAll, where the
// bound `?` value is produced by db.ScanJulianDay from the origin row's mtime.
//
// Goal: prove the "overwrite a child's propagated tag only if it is strictly
// older" decision survives a scanned mtime that is only one millisecond away
// from the child's stored mtime. Under the ncruces driver the origin mtime is
// scanned from a time.Time carrying sub-millisecond conversion noise; if that
// noise collapsed the one-millisecond gap (as a prior truncation bug did), the
// strict `<` would flip and the overwrite would silently not happen.
//
// Methodology: A is the ancestor of two children, B and C. B carries an older
// propagated tag (base), C a newer one (base+2ms), and A's own tag sits between
// them at base+1ms. Re-propagating from A must overwrite B (base < base+1ms) and
// leave C untouched (base+2ms is not < base+1ms). Adjacent-millisecond deltas
// are the point: a one-millisecond truncation is exactly what flips the B
// decision, and only sub-10ms intervals can express that — timestamps hours
// apart pass regardless of the bug. This test runs under whichever driver the
// testdriver import selects, so `-tags test_ncruces` runs it where the noise
// lives.
func TestPropagateAllOrderingAdjacentMillisecond(t *testing.T) {
	r := setupTestRepo(t)

	// A is the common ancestor; B and C are both primary children of A. They
	// predate the tag mtimes below so the origin application at A is not
	// superseded by the branch row the check-in itself wrote.
	checkedInAt := time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC)
	ridA := makeCheckinAt(t, r, 0, "a.txt", "content A", "commit A", checkedInAt)
	ridB := makeCheckinAt(t, r, ridA, "b.txt", "content B", "commit B", checkedInAt.Add(time.Minute))
	ridC := makeCheckinAt(t, r, ridA, "c.txt", "content C", "commit C", checkedInAt.Add(2*time.Minute))

	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	mtimeChildOld := libfossil.TimeToJulian(base)                           // B: older than A
	mtimeOrigin := libfossil.TimeToJulian(base.Add(1 * time.Millisecond))   // A: one ms newer than B
	mtimeChildNew := libfossil.TimeToJulian(base.Add(2 * time.Millisecond)) // C: newer than A

	// Apply the propagating tag at A. This creates the tag and, via propagate,
	// seeds srcid=0 tagxref rows on B and C that we overwrite below to set exact
	// mtimes for the comparison.
	if err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: libfossil.FslID(ridA),
		SrcRID:    libfossil.FslID(ridA), // non-zero: an origin, not a propagated row
		TagName:   "branch",
		TagType:   tag.TagPropagating,
		Value:     "new",
		MTime:     mtimeOrigin,
	}); err != nil {
		t.Fatalf("tag.ApplyTag: %v", err)
	}

	// Pin the children's existing propagated tags to exact mtimes a single
	// millisecond on either side of the origin.
	setChildTag := func(rid int64, value string, mtime float64) {
		t.Helper()
		if _, err := r.DB().Exec(
			`UPDATE tagxref SET value=?, mtime=?, srcid=0
			 WHERE tagid=(SELECT tagid FROM tag WHERE tagname='branch') AND rid=?`,
			value, mtime, rid,
		); err != nil {
			t.Fatalf("pin child tag for rid=%d: %v", rid, err)
		}
	}
	setChildTag(ridB, "old", mtimeChildOld)
	setChildTag(ridC, "newer", mtimeChildNew)

	// Re-propagate from A. The `?` in `tagxref.mtime < ?` now comes from
	// ScanJulianDay(A.mtime).
	if err := tag.PropagateAll(r.DB(), libfossil.FslID(ridA)); err != nil {
		t.Fatalf("tag.PropagateAll: %v", err)
	}

	readChildValue := func(rid int64) string {
		t.Helper()
		var value string
		if err := r.DB().QueryRow(
			`SELECT value FROM tagxref
			 WHERE tagid=(SELECT tagid FROM tag WHERE tagname='branch') AND rid=?`,
			rid,
		).Scan(&value); err != nil {
			t.Fatalf("read child tag for rid=%d: %v", rid, err)
		}
		return value
	}

	// B was strictly older than the origin (base < base+1ms) → overwritten.
	if got := readChildValue(ridB); got != "new" {
		t.Fatalf("B tag value = %q, want %q: base < origin (one ms) did not overwrite — a lost millisecond collapsed the strict ordering", got, "new")
	}
	// C was newer than the origin (base+2ms not < base+1ms) → preserved. This
	// negative control confirms the comparison actually gates rather than always
	// overwriting.
	if got := readChildValue(ridC); got != "newer" {
		t.Fatalf("C tag value = %q, want %q: newer child was overwritten — the `<` guard did not hold", got, "newer")
	}
}

func TestPropagateAll(t *testing.T) {
	r := setupTestRepo(t)

	// Create linear chain A→B→C
	ridA := makeCheckin(t, r, 0, "a.txt", "content A", "commit A")
	ridB := makeCheckin(t, r, ridA, "b.txt", "content B", "commit B")
	ridC := makeCheckin(t, r, ridB, "c.txt", "content C", "commit C")

	// Add propagating "branch" tag to A with value "feature"
	_, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: libfossil.FslID(ridA),
		TagName:   "branch",
		TagType:   tag.TagPropagating,
		Value:     "feature",
		User:      "testuser",
		Time:      tagTime(1),
	})
	if err != nil {
		t.Fatalf("tag.AddTag: %v", err)
	}

	// Verify B and C have propagated tags initially
	var countB, countC int
	r.DB().QueryRow(`
		SELECT COUNT(*) FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'branch' AND tagxref.rid = ? AND srcid = 0
	`, ridB).Scan(&countB)
	r.DB().QueryRow(`
		SELECT COUNT(*) FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'branch' AND tagxref.rid = ? AND srcid = 0
	`, ridC).Scan(&countC)
	if countB != 1 || countC != 1 {
		t.Fatalf("initial setup check: B has %d tags, C has %d tags, want both = 1", countB, countC)
	}

	// Clear propagated tags from B and C to simulate incomplete propagation
	if _, err := r.DB().Exec("DELETE FROM tagxref WHERE rid=? AND srcid=0", ridB); err != nil {
		t.Fatalf("clear B tags: %v", err)
	}
	if _, err := r.DB().Exec("DELETE FROM tagxref WHERE rid=? AND srcid=0", ridC); err != nil {
		t.Fatalf("clear C tags: %v", err)
	}

	// Verify cleared
	r.DB().QueryRow(`
		SELECT COUNT(*) FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'branch' AND tagxref.rid = ? AND srcid = 0
	`, ridB).Scan(&countB)
	r.DB().QueryRow(`
		SELECT COUNT(*) FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'branch' AND tagxref.rid = ? AND srcid = 0
	`, ridC).Scan(&countC)
	if countB != 0 || countC != 0 {
		t.Fatalf("after clear: B has %d tags, C has %d tags, want both = 0", countB, countC)
	}

	// Call PropagateAll on A to re-propagate
	if err := tag.PropagateAll(r.DB(), libfossil.FslID(ridA)); err != nil {
		t.Fatalf("tag.PropagateAll: %v", err)
	}

	// Verify B has propagated branch=feature tag (srcid=0)
	var srcidB, tagtypeB int
	var valueB string
	err = r.DB().QueryRow(`
		SELECT srcid, tagtype, value FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'branch' AND tagxref.rid = ?
	`, ridB).Scan(&srcidB, &tagtypeB, &valueB)
	if err != nil {
		t.Fatalf("tagxref query for B: %v", err)
	}
	if srcidB != 0 {
		t.Errorf("B srcid = %d, want 0 (propagated)", srcidB)
	}
	if tagtypeB != tag.TagPropagating {
		t.Errorf("B tagtype = %d, want %d", tagtypeB, tag.TagPropagating)
	}
	if valueB != "feature" {
		t.Errorf("B value = %q, want %q", valueB, "feature")
	}

	// Verify C has propagated branch=feature tag (srcid=0)
	var srcidC, tagtypeC int
	var valueC string
	err = r.DB().QueryRow(`
		SELECT srcid, tagtype, value FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		WHERE tag.tagname = 'branch' AND tagxref.rid = ?
	`, ridC).Scan(&srcidC, &tagtypeC, &valueC)
	if err != nil {
		t.Fatalf("tagxref query for C: %v", err)
	}
	if srcidC != 0 {
		t.Errorf("C srcid = %d, want 0 (propagated)", srcidC)
	}
	if tagtypeC != tag.TagPropagating {
		t.Errorf("C tagtype = %d, want %d", tagtypeC, tag.TagPropagating)
	}
	if valueC != "feature" {
		t.Errorf("C value = %q, want %q", valueC, "feature")
	}
}

// TestApplyTagIgnoresOlderTag pins the guard canonical fossil opens tag_insert
// with (src/tag.c:173-186): if tagxref already holds a row for (tagid, rid)
// whose mtime is >= the incoming one, the application is dropped whole -- no
// tagxref write and no propagation.
//
//	SELECT 1 FROM tagxref WHERE tagid=%d AND rid=%d AND mtime>=:mtime
//	...
//	if( rc==SQLITE_ROW ){
//	  /* Another entry that is more recent already exists.  Do nothing */
//	  return tagid;
//	}
//
// It is what makes the result independent of the order artifacts are applied
// in, which crosslink relies on: a check-in's inline T-cards and a control
// artifact that later retagged the same check-in can be crosslinked in either
// order, and the newer one has to win both times. Without the guard the older
// application overwrites the newer row and then propagates the stale value to
// every descendant (issue #198).
func TestApplyTagIgnoresOlderTag(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	ridA, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "f.txt", Content: []byte("a")}},
		Comment: "A",
		User:    "testuser",
		Time:    base,
	})
	if err != nil {
		t.Fatalf("Checkin A: %v", err)
	}
	ridB, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "f.txt", Content: []byte("b")}},
		Comment: "B",
		User:    "testuser",
		Time:    base.Add(time.Minute),
		Parent:  ridA,
	})
	if err != nil {
		t.Fatalf("Checkin B: %v", err)
	}

	newer := libfossil.TimeToJulian(base.Add(48 * time.Hour))
	older := libfossil.TimeToJulian(base.Add(24 * time.Hour))

	// The newer application lands and propagates to B.
	if err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: ridA,
		SrcRID:    ridA,
		TagName:   "branch",
		TagType:   tag.TagPropagating,
		Value:     "clear-title",
		MTime:     newer,
	}); err != nil {
		t.Fatalf("ApplyTag newer: %v", err)
	}

	// The older one must be dropped whole.
	if err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: ridA,
		SrcRID:    ridA,
		TagName:   "branch",
		TagType:   tag.TagPropagating,
		Value:     "dual-license",
		MTime:     older,
	}); err != nil {
		t.Fatalf("ApplyTag older: %v", err)
	}

	for _, tc := range []struct {
		name string
		rid  libfossil.FslID
	}{{"A", ridA}, {"B", ridB}} {
		var value string
		// mtime comes back with a driver-dependent dynamic type, so it goes
		// through ScanJulianDay the way every other consumer does.
		var mtimeRaw any
		if err := r.DB().QueryRow(`
			SELECT value, mtime FROM tagxref JOIN tag USING(tagid)
			WHERE tag.tagname = 'branch' AND tagxref.rid = ?
		`, tc.rid).Scan(&value, &mtimeRaw); err != nil {
			t.Fatalf("tagxref query for %s: %v", tc.name, err)
		}
		mtime, ok := db.ScanJulianDay(mtimeRaw)
		if !ok {
			t.Fatalf("ScanJulianDay for %s: cannot read %T", tc.name, mtimeRaw)
		}
		if value != "clear-title" {
			t.Errorf("%s value = %q, want %q (older application must not overwrite)", tc.name, value, "clear-title")
		}
		if mtime != newer {
			t.Errorf("%s mtime = %v, want %v", tc.name, mtime, newer)
		}
	}
}

// TestApplyTagSingletonBlocksPropagation pins the downgrade canonical performs
// at the end of tag_insert (src/tag.c:239):
//
//	if( tagtype==1 ) tagtype = 0;
//	tag_propagate(rid, tagid, tagtype, rid, zValue, mtime);
//
// A singleton (+) tag is propagated as a cancel, so applying one at an artifact
// deletes the inherited copies its descendants hold. RFC draft-fossil-repo-
// state-00 §5.3 step 4 states the same rule: "Before propagation is attempted,
// a singleton type (1) is downgraded to cancel (0) ... the cancel it becomes
// blocks a same-named tag from propagating through it to its descendants."
//
// Confirmed against fossil 2.28 on a two-check-in repository: after
// `fossil tag add --raw --propagate zz A v1` the child carries an inherited
// row, and after `fossil tag add --raw zz A v2` that row is gone, leaving only
// A's own singleton.
//
// Our direct-application path propagated for type 2 and type 0 only, so the
// inherited row survived and the singleton failed to block (issue #198).
func TestApplyTagSingletonBlocksPropagation(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC)

	ridA := makeCheckinAt(t, r, 0, "a.txt", "aaa", "commit A", base)
	ridB := makeCheckinAt(t, r, ridA, "a.txt", "bbb", "commit B", base.Add(time.Minute))

	if err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: libfossil.FslID(ridA),
		SrcRID:    libfossil.FslID(ridA),
		TagName:   "zz",
		TagType:   tag.TagPropagating,
		Value:     "v1",
		MTime:     libfossil.TimeToJulian(base.Add(time.Hour)),
	}); err != nil {
		t.Fatalf("ApplyTag propagating: %v", err)
	}

	var inherited int
	if err := r.DB().QueryRow(
		"SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='zz' AND rid=?", ridB,
	).Scan(&inherited); err != nil {
		t.Fatalf("count B before: %v", err)
	}
	if inherited != 1 {
		t.Fatalf("B zz rows before singleton = %d, want 1 (fixture must inherit)", inherited)
	}

	// The singleton at A must propagate as a cancel and clear B's copy.
	if err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: libfossil.FslID(ridA),
		SrcRID:    libfossil.FslID(ridA),
		TagName:   "zz",
		TagType:   tag.TagSingleton,
		Value:     "v2",
		MTime:     libfossil.TimeToJulian(base.Add(2 * time.Hour)),
	}); err != nil {
		t.Fatalf("ApplyTag singleton: %v", err)
	}

	if err := r.DB().QueryRow(
		"SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='zz' AND rid=?", ridB,
	).Scan(&inherited); err != nil {
		t.Fatalf("count B after: %v", err)
	}
	if inherited != 0 {
		t.Errorf("B zz rows after singleton = %d, want 0 (singleton must propagate as cancel)", inherited)
	}

	var tagtype int
	if err := r.DB().QueryRow(
		"SELECT tagtype FROM tagxref JOIN tag USING(tagid) WHERE tagname='zz' AND rid=?", ridA,
	).Scan(&tagtype); err != nil {
		t.Fatalf("A tagtype: %v", err)
	}
	if tagtype != tag.TagSingleton {
		t.Errorf("A tagtype = %d, want %d (singleton stays singleton at its own artifact)", tagtype, tag.TagSingleton)
	}
}
