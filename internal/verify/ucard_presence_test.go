package verify_test

import (
	"crypto/md5"
	"database/sql"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// rawCheckinManifest hand-builds a minimal, valid Checkin manifest so the
// test controls the U-card line byte-for-byte — something deck.Deck can't
// express for the present-but-empty case, since Marshal deliberately never
// emits an empty U-card. userLine must be "" (card omitted entirely),
// "U \n" (card present, empty), or "U <name>\n" (card present with a
// value). Mirrors manifest.rawCheckinManifest in
// internal/manifest/ucard_presence_test.go — duplicated here rather than
// exported since it's test-only fixture, not production surface.
func rawCheckinManifest(userLine string) []byte {
	body := "D 2024-01-15T10:30:00.000\nR d41d8cd98f00b204e9800998ecf8427e\n" + userLine
	h := md5.Sum([]byte(body))
	return []byte(fmt.Sprintf("%sZ %x\n", body, h))
}

// TestRebuild_UCardPresenceStates covers the rebuild path with the same
// three-state U-card coverage as manifest.TestCrosslink_UCardPresenceStates.
// verify.Rebuild now derives event through manifest.Crosslink, so the U-card
// binding is crosslink's; this exercises it from the rebuild entry point so a
// future edit that collapses SQL NULL back into "" on either path is caught.
func TestRebuild_UCardPresenceStates(t *testing.T) {
	r := newTestRepo(t)

	absentRID, _, err := blob.Store(r.DB(), rawCheckinManifest(""))
	if err != nil {
		t.Fatalf("blob.Store(absent): %v", err)
	}
	emptyRID, _, err := blob.Store(r.DB(), rawCheckinManifest("U \n"))
	if err != nil {
		t.Fatalf("blob.Store(empty): %v", err)
	}
	valuedRID, _, err := blob.Store(r.DB(), rawCheckinManifest("U alice\n"))
	if err != nil {
		t.Fatalf("blob.Store(valued): %v", err)
	}

	// Rebuild empties the derived tables and reconstructs event (and friends)
	// from raw blob content through the crosslink sweep.
	if _, err := verify.Rebuild(r); err != nil {
		t.Fatalf("verify.Rebuild: %v", err)
	}

	assertRebuiltEventUser(t, r, absentRID, sql.NullString{Valid: false}, "absent U-card")
	assertRebuiltEventUser(t, r, emptyRID, sql.NullString{String: "anonymous", Valid: true}, "present-empty U-card")
	assertRebuiltEventUser(t, r, valuedRID, sql.NullString{String: "alice", Valid: true}, "present-valued U-card")
}

// assertRebuiltEventUser reads the event.user column for objid=rid and
// compares it against want, distinguishing SQL NULL (Valid=false) from any
// string value (including the empty string) so a regression that collapses
// NULL back into "" is caught.
func assertRebuiltEventUser(t *testing.T, r *repo.Repo, rid libfossil.FslID, want sql.NullString, label string) {
	t.Helper()
	var got sql.NullString
	if err := r.DB().QueryRow("SELECT user FROM event WHERE objid=?", rid).Scan(&got); err != nil {
		t.Fatalf("%s: query event.user: %v", label, err)
	}
	if got != want {
		t.Fatalf("%s: event.user = %+v, want %+v", label, got, want)
	}
}
