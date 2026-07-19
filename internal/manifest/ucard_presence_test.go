package manifest

import (
	"crypto/md5"
	"database/sql"
	"fmt"
	"testing"

	"github.com/danmestas/libfossil/internal/blob"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/repo"
)

// rawCheckinManifest hand-builds a minimal, valid Checkin manifest so the
// test controls the U-card line byte-for-byte — something deck.Deck can't
// express for the present-but-empty case, since Marshal deliberately never
// emits an empty U-card (see marshal.go). userLine must be "" (card
// omitted entirely), "U \n" (card present, empty), or "U <name>\n" (card
// present with a value); it is inserted in the same ASCII card position
// deck.Marshal would use.
func rawCheckinManifest(userLine string) []byte {
	body := "D 2024-01-15T10:30:00.000\nR d41d8cd98f00b204e9800998ecf8427e\n" + userLine
	h := md5.Sum([]byte(body))
	return []byte(fmt.Sprintf("%sZ %x\n", body, h))
}

// TestCrosslink_UCardPresenceStates verifies the three-way U-card
// resolution required by issue #50: a wholly absent U-card must crosslink
// to SQL NULL, a present-but-empty U-card must crosslink to the literal
// "anonymous" (matching fossil's own parse-time substitution, see
// src/manifest.c:1008-1016), and a present U-card with a value must
// crosslink to that value unchanged. Before this fix, both the absent and
// present-empty cases collapsed to the same stored value: empty string.
func TestCrosslink_UCardPresenceStates(t *testing.T) {
	r := setupTestRepo(t)

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

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked != 3 {
		t.Fatalf("Crosslink: linked = %d, want 3", linked)
	}

	assertEventUser(t, r, absentRID, sql.NullString{Valid: false}, "absent U-card")
	assertEventUser(t, r, emptyRID, sql.NullString{String: "anonymous", Valid: true}, "present-empty U-card")
	assertEventUser(t, r, valuedRID, sql.NullString{String: "alice", Valid: true}, "present-valued U-card")
}

// assertEventUser reads the event.user column for objid=rid and compares
// it against want, distinguishing SQL NULL (Valid=false) from any string
// value (including the empty string) so a regression that collapses NULL
// back into "" is caught.
func assertEventUser(t *testing.T, r *repo.Repo, rid libfossil.FslID, want sql.NullString, label string) {
	t.Helper()
	var got sql.NullString
	if err := r.DB().QueryRow("SELECT user FROM event WHERE objid=?", rid).Scan(&got); err != nil {
		t.Fatalf("%s: query event.user: %v", label, err)
	}
	if got != want {
		t.Fatalf("%s: event.user = %+v, want %+v", label, got, want)
	}
}
