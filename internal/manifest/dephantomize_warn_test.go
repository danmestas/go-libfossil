package manifest

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// captureWarnings redirects the default logger into a buffer for one test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// marshalArtifact renders a deck to its wire bytes.
func marshalArtifact(t *testing.T, d *deck.Deck) []byte {
	t.Helper()
	raw, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

// storeOrphanOf stores content and files it as an orphan of baseline, which is
// how the cascade discovers an artifact it did not walk to through the delta
// graph.
func storeOrphanOf(t *testing.T, r *repo.Repo, content []byte, baseline libfossil.FslID) libfossil.FslID {
	t.Helper()
	rid, _, err := blob.Store(r.DB(), content)
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	if _, err := r.DB().Exec("INSERT INTO orphan(rid, baseline) VALUES(?, ?)", rid, baseline); err != nil {
		t.Fatalf("insert orphan row: %v", err)
	}
	return rid
}

// TestAfterDephantomizeDoesNotWarnForFileBlobs pins issue #186: a blob that is
// not a manifest is the ordinary case, not a fault. The whole-repository sweep
// has always skipped it silently (linkBatch), while the cascade warned -- one
// line per file blob a clone filled, ~39k of them on fossil's own repository,
// which buried the crosslink failures that do matter.
func TestAfterDephantomizeDoesNotWarnForFileBlobs(t *testing.T) {
	r := setupTestRepo(t)
	logs := captureWarnings(t)

	rootRid, _, err := blob.Store(r.DB(), marshalArtifact(t, &deck.Deck{
		Type: deck.Wiki,
		L:    "QuietPage",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC),
		W:    []byte("a wiki page"),
	}))
	if err != nil {
		t.Fatalf("Store wiki root: %v", err)
	}

	// Ordinary file content, of the kind the issue sampled: HTML whose last
	// lines are </script> and </body></html>. Nothing about it is a manifest.
	storeOrphanOf(t, r, []byte("<html>\n<body>\n<script>x=1</script>\n</body></html>\n"), rootRid)
	storeOrphanOf(t, r, []byte("plain text, no cards at all\n"), rootRid)
	storeOrphanOf(t, r, []byte("short\n"), rootRid)

	stats := afterDephantomize(context.Background(), r, rootRid, true)

	if stats.attempts != 4 {
		t.Fatalf("visited %d artifacts, want 4 (the wiki and three file blobs)", stats.attempts)
	}
	if stats.linked != 1 {
		t.Errorf("linked %d artifacts, want 1 (only the wiki is a manifest)", stats.linked)
	}
	if got := logs.String(); strings.Contains(got, "un-crosslinked") {
		t.Errorf("cascade warned about blobs that are simply not manifests:\n%s", got)
	}
}

// TestAfterDephantomizeStillWarnsWhenLinkFails is the other half of #186:
// silencing the not-a-manifest case must not silence a genuine fault. A blob
// that parses as a manifest but cannot be linked is a real failure and still
// has to be logged, or the fix would trade one blind spot for another.
func TestAfterDephantomizeStillWarnsWhenLinkFails(t *testing.T) {
	r := setupTestRepo(t)

	_, fileUUID, err := blob.Store(r.DB(), []byte("content for the failing checkin"))
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}
	rootRid, _, err := blob.Store(r.DB(), marshalArtifact(t, &deck.Deck{
		Type: deck.Wiki,
		L:    "LoudPage",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC),
		W:    []byte("a wiki page"),
	}))
	if err != nil {
		t.Fatalf("Store wiki root: %v", err)
	}
	failRid := storeOrphanOf(t, r, marshalArtifact(t, &deck.Deck{
		Type: deck.Checkin,
		C:    "checkin that cannot link",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 6, 1, 9, 1, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "f.txt", UUID: fileUUID}},
	}), rootRid)

	// Removing the table the check-in path writes makes linking fail after the
	// manifest has parsed -- the shape that must still be reported.
	if _, err := r.DB().Exec("DROP TABLE filename"); err != nil {
		t.Fatalf("DROP TABLE filename: %v", err)
	}

	logs := captureWarnings(t)
	afterDephantomize(context.Background(), r, rootRid, true)

	got := logs.String()
	if !strings.Contains(got, "un-crosslinked") {
		t.Fatalf("a check-in that parsed but failed to link was not reported:\n%s", got)
	}
	if !strings.Contains(got, "rid="+itoaFsl(failRid)) {
		t.Errorf("the warning does not name the failing rid %d:\n%s", failRid, got)
	}
}

// TestAfterDephantomizeCascadeLinksTicketArtifacts covers the cascade half of
// issue #184. The cascade routes through linkArtifact, the same type switch the
// sweep uses, and discards the pending items it returns -- so for as long as a
// ticket's event row was pending work, a ticket filled by a phantom fill was
// lost exactly as one found by the sweep was. It must now leave a full set of
// rows behind on its own.
func TestAfterDephantomizeCascadeLinksTicketArtifacts(t *testing.T) {
	r := setupTestRepo(t)

	rootRid, _, err := blob.Store(r.DB(), marshalArtifact(t, &deck.Deck{
		Type: deck.Wiki,
		L:    "CascadeRoot",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 6, 2, 9, 0, 0, 0, time.UTC),
		W:    []byte("root"),
	}))
	if err != nil {
		t.Fatalf("Store wiki root: %v", err)
	}

	uuid := ticketUUID(3)
	createRid := storeOrphanOf(t, r, ticketArtifact(t, uuid,
		time.Date(2024, 6, 2, 9, 1, 0, 0, time.UTC), "alice",
		deck.TicketField{Name: "status", Value: "Open"},
		deck.TicketField{Name: "title", Value: "cascaded ticket"},
	), rootRid)
	changeRid := storeOrphanOf(t, r, ticketArtifact(t, uuid,
		time.Date(2024, 6, 2, 9, 2, 0, 0, time.UTC), "bob",
		deck.TicketField{Name: "status", Value: "Closed"},
	), rootRid)

	afterDephantomize(context.Background(), r, rootRid, true)

	for _, rid := range []libfossil.FslID{createRid, changeRid} {
		var n int
		if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=? AND type='t'", rid).Scan(&n); err != nil {
			t.Fatalf("count ticket event for rid=%d: %v", rid, err)
		}
		if n != 1 {
			t.Errorf("ticket artifact rid=%d has %d event rows, want 1", rid, n)
		}
	}

	var tickets, changes int
	if err := r.DB().QueryRow("SELECT count(*) FROM ticket WHERE tkt_uuid=?", uuid).Scan(&tickets); err != nil {
		t.Fatalf("count ticket rows: %v", err)
	}
	if tickets != 1 {
		t.Errorf("ticket rows for %s = %d, want 1", uuid, tickets)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM ticketchng").Scan(&changes); err != nil {
		t.Fatalf("count ticketchng rows: %v", err)
	}
	if changes != 2 {
		t.Errorf("ticketchng rows = %d, want 2 (one per artifact)", changes)
	}
}

func itoaFsl(rid libfossil.FslID) string {
	return strconv.FormatInt(int64(rid), 10)
}
