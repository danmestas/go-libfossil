package manifest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// buildCascadeGraph stores the artifact graph one phantom fill unblocks, in the
// shapes the walk actually distinguishes:
//
//	rid 1  file blob        the F-card target every manifest names
//	rid 2  root checkin     the filled phantom the cascade starts from
//	rid 3  child checkin    delta of root, P-card -> root (parent-manifest lookup)
//	rid 4  child checkin    delta of root, P-card -> root, renames the file
//	rid 5  wiki manifest    delta of root, the non-checkin branch of linkArtifact
//	rid 6  plain blob       delta of root, not a manifest: the parse-failure branch
//	rid 7  orphan checkin   delta of root AND an orphan row keyed on root
//	rid 8  orphan checkin   same, but renames the file so it writes mlink rows
//
// rids 7 and 8 are the read-after-write case, and they land on opposite sides
// of it. The walk crosslinks both as root's orphans and then asks which delta
// children of root still lack mlink rows. rid 8's crosslink wrote mlink rows,
// so under per-artifact transactions -- committed by the time the query ran --
// the query filtered rid 8 out and the walk visited it once. rid 7 carries its
// file over unchanged and so writes no mlink row at all, so the query did NOT
// filter it and the walk visited it a second time, idempotently. Anything that
// defers those writes past the query has to reproduce both halves exactly.
//
// Rids are spelled out because a fresh test repository assigns them in store
// order, which is what makes the golden dump below readable.
func buildCascadeGraph(t *testing.T, r *repo.Repo) libfossil.FslID {
	t.Helper()

	fileRid, fileUUID, err := blob.Store(r.DB(), []byte("cascade file content"))
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}
	if fileRid != 1 {
		t.Fatalf("file blob rid = %d, want 1 (fixture assumes a fresh repository)", fileRid)
	}

	at := func(min int) time.Time {
		return time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
	}
	marshal := func(d *deck.Deck) []byte {
		mb, err := d.Marshal()
		if err != nil {
			t.Fatalf("Marshal %v: %v", d.Type, err)
		}
		return mb
	}

	rootRid, rootUUID, err := blob.Store(r.DB(), marshal(&deck.Deck{
		Type: deck.Checkin,
		C:    "cascade root",
		U:    deck.User("testuser"),
		D:    at(0),
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
	}))
	if err != nil {
		t.Fatalf("Store root checkin: %v", err)
	}

	childBytes := marshal(&deck.Deck{
		Type: deck.Checkin,
		C:    "cascade child",
		U:    deck.User("testuser"),
		D:    at(1),
		P:    []string{rootUUID},
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
	})
	if _, _, err := blob.StoreDelta(r.DB(), childBytes, rootRid); err != nil {
		t.Fatalf("StoreDelta child checkin: %v", err)
	}

	renameBytes := marshal(&deck.Deck{
		Type: deck.Checkin,
		C:    "cascade rename",
		U:    deck.User("testuser"),
		D:    at(2),
		P:    []string{rootUUID},
		F:    []deck.FileCard{{Name: "goodbye.txt", UUID: fileUUID, OldName: "hello.txt"}},
	})
	if _, _, err := blob.StoreDelta(r.DB(), renameBytes, rootRid); err != nil {
		t.Fatalf("StoreDelta rename checkin: %v", err)
	}

	wikiBytes := marshal(&deck.Deck{
		Type: deck.Wiki,
		L:    "CascadePage",
		U:    deck.User("testuser"),
		D:    at(3),
		W:    []byte("cascade wiki body"),
	})
	if _, _, err := blob.StoreDelta(r.DB(), wikiBytes, rootRid); err != nil {
		t.Fatalf("StoreDelta wiki: %v", err)
	}

	if _, _, err := blob.StoreDelta(r.DB(), []byte("not a manifest at all"), rootRid); err != nil {
		t.Fatalf("StoreDelta plain blob: %v", err)
	}

	orphanBytes := marshal(&deck.Deck{
		Type: deck.Checkin,
		C:    "cascade orphan",
		U:    deck.User("testuser"),
		D:    at(4),
		P:    []string{rootUUID},
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
	})
	orphanRid, _, err := blob.StoreDelta(r.DB(), orphanBytes, rootRid)
	if err != nil {
		t.Fatalf("StoreDelta orphan checkin: %v", err)
	}
	if _, err := r.DB().Exec("INSERT INTO orphan(rid, baseline) VALUES(?, ?)", orphanRid, rootRid); err != nil {
		t.Fatalf("insert orphan row: %v", err)
	}

	mlinkOrphanBytes := marshal(&deck.Deck{
		Type: deck.Checkin,
		C:    "cascade orphan rename",
		U:    deck.User("testuser"),
		D:    at(5),
		P:    []string{rootUUID},
		F:    []deck.FileCard{{Name: "orphan.txt", UUID: fileUUID, OldName: "hello.txt"}},
	})
	mlinkOrphanRid, _, err := blob.StoreDelta(r.DB(), mlinkOrphanBytes, rootRid)
	if err != nil {
		t.Fatalf("StoreDelta mlink orphan checkin: %v", err)
	}
	if _, err := r.DB().Exec("INSERT INTO orphan(rid, baseline) VALUES(?, ?)", mlinkOrphanRid, rootRid); err != nil {
		t.Fatalf("insert mlink orphan row: %v", err)
	}

	for _, tbl := range []string{
		"DELETE FROM event", "DELETE FROM plink", "DELETE FROM mlink",
		"DELETE FROM tagxref", "DELETE FROM leaf",
	} {
		if _, err := r.DB().Exec(tbl); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
	}
	return rootRid
}

// dumpDerivedRows renders every derived row the cascade can write, in a stable
// order, as one comparable string.
func dumpDerivedRows(t *testing.T, r *repo.Repo) string {
	t.Helper()

	queries := []struct {
		table string
		sql   string
	}{
		{"event", "SELECT type, objid, user, comment FROM event ORDER BY objid, type"},
		{"plink", "SELECT pid, cid, isprim, baseid FROM plink ORDER BY cid, pid"},
		{"mlink", "SELECT mid, fid, pmid, pid, fnid, pfnid, mperm, isaux FROM mlink ORDER BY mid, fnid, fid"},
		{"tagxref", "SELECT tagid, tagtype, srcid, value, rid FROM tagxref ORDER BY rid, tagid"},
		{"filename", "SELECT fnid, name FROM filename ORDER BY fnid"},
		{"orphan", "SELECT rid, baseline FROM orphan ORDER BY rid"},
	}

	var out strings.Builder
	for _, q := range queries {
		rows, err := r.DB().Query(q.sql)
		if err != nil {
			t.Fatalf("dump %s: %v", q.table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("dump %s columns: %v", q.table, err)
		}
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatalf("dump %s scan: %v", q.table, err)
			}
			parts := make([]string, len(cols))
			for i, v := range vals {
				switch tv := v.(type) {
				case nil:
					parts[i] = cols[i] + "=NULL"
				case []byte:
					parts[i] = cols[i] + "=" + string(tv)
				case bool:
					// The two SQLite drivers disagree on how a boolean column
					// scans into an any: ncruces yields a Go bool where modernc
					// yields an int64. The stored value is identical either way,
					// so normalize to SQLite's own 1/0 and keep this golden
					// driver-independent rather than pinning one driver's
					// rendering (isprim, isaux).
					if tv {
						parts[i] = cols[i] + "=1"
					} else {
						parts[i] = cols[i] + "=0"
					}
				default:
					parts[i] = fmt.Sprintf("%s=%v", cols[i], tv)
				}
			}
			lines = append(lines, q.table+" "+strings.Join(parts, " "))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("dump %s rows: %v", q.table, err)
		}
		rows.Close()
		sort.Strings(lines)
		for _, l := range lines {
			out.WriteString(l)
			out.WriteString("\n")
		}
	}
	return out.String()
}

// cascadeGolden is the derived state buildCascadeGraph's cascade produced under
// the per-artifact-transaction shape this package used before the cascade was
// batched. It is a characterization: it passed before that change and must keep
// passing after, because batching commits and sharing a content cache is a
// change to how the work runs, never to what it writes.
// The filename rows are load-bearing: fnids are handed out in the order the
// walk crosslinks artifacts, so orphan.txt landing on fnid 2 and goodbye.txt on
// fnid 3 pins that rid 8 is still linked before rid 4. A cascade that reordered
// or dropped a visit would renumber them.
const cascadeGolden = `event type=ci objid=2 user=testuser comment=cascade root
event type=ci objid=3 user=testuser comment=cascade child
event type=ci objid=4 user=testuser comment=cascade rename
event type=ci objid=7 user=testuser comment=cascade orphan
event type=ci objid=8 user=testuser comment=cascade orphan rename
event type=w objid=5 user=testuser comment=+CascadePage
plink pid=2 cid=3 isprim=1 baseid=NULL
plink pid=2 cid=4 isprim=1 baseid=NULL
plink pid=2 cid=7 isprim=1 baseid=NULL
plink pid=2 cid=8 isprim=1 baseid=NULL
mlink mid=2 fid=1 pmid=0 pid=0 fnid=1 pfnid=0 mperm=0 isaux=0
mlink mid=4 fid=0 pmid=2 pid=1 fnid=1 pfnid=0 mperm=0 isaux=0
mlink mid=4 fid=1 pmid=2 pid=1 fnid=3 pfnid=1 mperm=0 isaux=0
mlink mid=8 fid=0 pmid=2 pid=1 fnid=1 pfnid=0 mperm=0 isaux=0
mlink mid=8 fid=1 pmid=2 pid=1 fnid=2 pfnid=1 mperm=0 isaux=0
tagxref tagid=12 tagtype=1 srcid=5 value=17 rid=5
filename fnid=1 name=hello.txt
filename fnid=2 name=orphan.txt
filename fnid=3 name=goodbye.txt
`

// cascadeVisits is how many artifacts the walk over buildCascadeGraph hands to
// the crosslink path: root, both orphans, then the four delta children of root
// the child query still returns, and rid 7 a second time as one of them. It was
// measured against the per-artifact-transaction shape (visit order
// [2 7 8 3 4 5 6 7]) and is asserted rather than derived so that a batching bug
// in the pending-mlink filter shows up as a count change even where the derived
// rows themselves are idempotent: mis-filtering rid 8 makes it 9, over-filtering
// rid 7 makes it 7.
const cascadeVisits = 8

func TestAfterDephantomizeCascadeDerivedRowsUnchanged(t *testing.T) {
	r := setupTestRepo(t)
	root := buildCascadeGraph(t, r)

	stats := afterDephantomize(context.Background(), r, root, true)

	got := dumpDerivedRows(t, r)
	if got != cascadeGolden {
		t.Errorf("cascade derived rows changed.\n--- got ---\n%s\n--- want ---\n%s", got, cascadeGolden)
	}
	if stats.attempts != cascadeVisits {
		t.Errorf("cascade crosslinked %d artifacts, want %d: the set of artifacts the walk "+
			"visits changed", stats.attempts, cascadeVisits)
	}
}

// buildSharedParentFanout stores a root check-in plus fanout children that are
// all delta-encoded against that root AND all name it as their P-card parent.
// That is the cascade's real shape: one phantom fill unblocks a burst of related
// artifacts that share a delta base and a parent manifest. Crosslinking a child
// expands the parent manifest (insertCheckinMlinks -> loadPrimaryParentFiles),
// so without a cache shared across the burst the root is expanded once per
// child, and with one it is expanded once for the whole cascade.
func buildSharedParentFanout(t *testing.T, r *repo.Repo, fanout int) (libfossil.FslID, []libfossil.FslID) {
	t.Helper()

	_, fileUUID, err := blob.Store(r.DB(), []byte("shared parent file"))
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}
	marshal := func(d *deck.Deck) []byte {
		mb, err := d.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return mb
	}

	root, rootUUID, err := blob.Store(r.DB(), marshal(&deck.Deck{
		Type: deck.Checkin,
		C:    "shared parent root",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 4, 1, 9, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
	}))
	if err != nil {
		t.Fatalf("Store root: %v", err)
	}

	children := make([]libfossil.FslID, fanout)
	for i := range children {
		body := marshal(&deck.Deck{
			Type: deck.Checkin,
			C:    fmt.Sprintf("shared parent child %d", i),
			U:    deck.User("testuser"),
			D:    time.Date(2024, 4, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(i+1) * time.Minute),
			P:    []string{rootUUID},
			F:    []deck.FileCard{{Name: fmt.Sprintf("child%d.txt", i), UUID: fileUUID}},
		})
		rid, _, err := blob.StoreDelta(r.DB(), body, root)
		if err != nil {
			t.Fatalf("StoreDelta child %d: %v", i, err)
		}
		children[i] = rid
	}
	return root, children
}

// TestAfterDephantomizeCascadeBatchesAndSharesCache is the regression guard for
// issue #172: the cascade used to open one write transaction per artifact and
// hand crosslinkCheckin a nil content cache, so a burst of related artifacts
// paid a commit each and re-expanded the same delta base and parent manifest
// over and over.
//
// The bounds are structural, not timings. fanout+1 artifacts fit in one
// crosslinkBatchSize batch, so the whole cascade must cost exactly one commit.
// Every artifact's own content is expanded once (fanout+1 expansions) and every
// child's parent-manifest lookup must then be a cache hit, because the parent is
// the root the cache already holds -- so hits scale with fanout while expansions
// do not. Undo either half and this fails loudly: without batching commits
// become fanout+1, without a shared cache hits collapse to zero and expansions
// roughly double.
func TestAfterDephantomizeCascadeBatchesAndSharesCache(t *testing.T) {
	const fanout = 300
	if fanout+1 >= crosslinkBatchSize {
		t.Fatalf("fixture of %d artifacts no longer fits one batch of %d", fanout+1, crosslinkBatchSize)
	}

	r := setupTestRepo(t)
	root, children := buildSharedParentFanout(t, r, fanout)

	stats := afterDephantomize(context.Background(), r, root, true)

	if got := countCrosslinked(t, r, children); got != fanout {
		t.Fatalf("crosslinked %d of %d children, want all of them", got, fanout)
	}
	if stats.attempts != fanout+1 {
		t.Fatalf("visited %d artifacts, want %d", stats.attempts, fanout+1)
	}
	if stats.commits != 1 {
		t.Errorf("cascade committed %d transactions for %d artifacts, want 1: the batch "+
			"buffer is not batching (issue #172)", stats.commits, stats.attempts)
	}
	if stats.expansions > fanout+1 {
		t.Errorf("cascade materialized %d content expansions for %d artifacts, want at most %d: "+
			"content shared across the cascade is being re-expanded (issue #172)",
			stats.expansions, stats.attempts, fanout+1)
	}
	if stats.cacheHits < fanout {
		t.Errorf("cascade served %d expansions from its shared cache, want at least %d: "+
			"the parent manifest every child names is not being reused (issue #172)",
			stats.cacheHits, fanout)
	}
}

// TestAfterDephantomizeCascadeIsolatesOneArtifactsFailure pins the failure
// contract batching had to preserve. Each artifact used to get its own
// transaction, which bought two things: a blob whose crosslink failed halfway
// left no partial event/plink/mlink/tagxref behind, and its failure cost the
// walk nothing else. Sharing a transaction across a batch threatens both, so
// each artifact's writes are wrapped in a savepoint.
//
// Dropping 'filename' makes a check-in that emits any mlink row fail inside
// insertCheckinMlinks -- after crosslinkCheckinTables has already written its
// event and plink rows. The wiki ahead of it in the same batch must survive,
// and the check-in must leave nothing.
func TestAfterDephantomizeCascadeIsolatesOneArtifactsFailure(t *testing.T) {
	r := setupTestRepo(t)

	_, fileUUID, err := blob.Store(r.DB(), []byte("isolation file"))
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}
	marshal := func(d *deck.Deck) []byte {
		mb, err := d.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return mb
	}

	wikiRid, _, err := blob.Store(r.DB(), marshal(&deck.Deck{
		Type: deck.Wiki,
		L:    "IsolationPage",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 5, 1, 8, 0, 0, 0, time.UTC),
		W:    []byte("isolation wiki body"),
	}))
	if err != nil {
		t.Fatalf("Store wiki root: %v", err)
	}

	// Reached as an orphan of the wiki, so both land in the same batch: the
	// wiki as the popped work item, the check-in right behind it.
	failRid, _, err := blob.Store(r.DB(), marshal(&deck.Deck{
		Type: deck.Checkin,
		C:    "isolation checkin",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 5, 1, 8, 1, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "isolation.txt", UUID: fileUUID}},
	}))
	if err != nil {
		t.Fatalf("Store failing checkin: %v", err)
	}
	if _, err := r.DB().Exec("INSERT INTO orphan(rid, baseline) VALUES(?, ?)", failRid, wikiRid); err != nil {
		t.Fatalf("insert orphan row: %v", err)
	}

	if _, err := r.DB().Exec("DROP TABLE filename"); err != nil {
		t.Fatalf("DROP TABLE filename: %v", err)
	}

	stats := afterDephantomize(context.Background(), r, wikiRid, true)

	if stats.attempts != 2 {
		t.Fatalf("visited %d artifacts, want 2", stats.attempts)
	}
	if stats.commits != 1 {
		t.Errorf("committed %d transactions, want 1: one artifact's failure took the "+
			"batch down with it", stats.commits)
	}
	if stats.linked != 1 {
		t.Errorf("linked %d artifacts, want 1 (the wiki; the check-in fails)", stats.linked)
	}

	var wikiEvents int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", wikiRid).Scan(&wikiEvents); err != nil {
		t.Fatalf("count wiki events: %v", err)
	}
	if wikiEvents != 1 {
		t.Errorf("wiki rid=%d has %d event rows, want 1: a sibling's failure rolled back "+
			"work that had already succeeded", wikiRid, wikiEvents)
	}

	for _, tbl := range []struct{ name, sql string }{
		{"event", "SELECT count(*) FROM event WHERE objid=?"},
		{"plink", "SELECT count(*) FROM plink WHERE cid=?"},
		{"mlink", "SELECT count(*) FROM mlink WHERE mid=?"},
	} {
		var n int
		if err := r.DB().QueryRow(tbl.sql, failRid).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl.name, err)
		}
		if n != 0 {
			t.Errorf("failed check-in rid=%d left %d %s rows, want 0: its partial writes "+
				"were committed with the rest of the batch", failRid, n, tbl.name)
		}
	}
}

// TestAfterDephantomizeCascadeCancellationAbandonsOpenBatch pins that the
// deadline is still observed once the walk is inside a batch, not only between
// pops. A batch flush is the one place the cascade now does a bounded pile of
// work in a single call, so it polls ctx on the same crosslinkCancelCheckStride
// the sweep's linkBatch uses and abandons the open batch when the poll fires --
// committed batches stand, the open one does not, and nothing pretends the
// cascade finished.
func TestAfterDephantomizeCascadeCancellationAbandonsOpenBatch(t *testing.T) {
	r := setupTestRepo(t)
	root, children := buildSharedParentFanout(t, r, crosslinkBatchSize+10)

	// Live through every walk poll of the first crosslinkBatchSize pops
	// (crosslinkBatchSize/crosslinkCancelCheckStride of them) and the flush's
	// own first poll, then dead: the only check that can observe that is the
	// one inside the flush.
	ctx := newPollCancelCtx(crosslinkBatchSize/crosslinkCancelCheckStride + 1)
	stats := afterDephantomize(ctx, r, root, true)

	if stats.commits != 0 {
		t.Errorf("committed %d transactions, want 0: the batch open when the deadline "+
			"fired was not abandoned", stats.commits)
	}
	if got := countCrosslinked(t, r, children); got != 0 {
		t.Errorf("crosslinked %d children, want 0: an abandoned batch left rows behind", got)
	}
}

// TestAfterDephantomizeCascadeCommitsWholeBatches pins that the buffer really
// does flush on crosslinkBatchSize rather than only at the end of the walk: a
// cascade wider than one batch must commit more than once, so the WAL bound the
// sweep's batching exists to hold (see crosslinkBatchSize) also holds here.
func TestAfterDephantomizeCascadeCommitsWholeBatches(t *testing.T) {
	r := setupTestRepo(t)
	root, children := buildSharedParentFanout(t, r, crosslinkBatchSize+10)

	stats := afterDephantomize(context.Background(), r, root, true)

	if got := countCrosslinked(t, r, children); got != len(children) {
		t.Fatalf("crosslinked %d of %d children, want all of them", got, len(children))
	}
	if stats.commits != 2 {
		t.Errorf("cascade committed %d transactions for %d artifacts, want 2 "+
			"(one full batch of %d plus the remainder)", stats.commits, stats.attempts, crosslinkBatchSize)
	}
}

// TestAfterDephantomizeCascadeAbandonsPartialBatchAtWalkPoll pins that a
// deadline firing at a *walk* poll, with artifacts buffered but not yet
// flushed, abandons that buffer instead of committing it.
//
// This is the half of the cancellation contract the other two tests miss.
// TestAfterDephantomizeCascadeCancellationAbandonsOpenBatch cancels inside a
// flush, where the failed flush has already drained pending, so the follow-up
// flush is a no-op either way; TestAfterDephantomizeObservesCancellationMidWalk
// asserts only a lower bound, which a partial post-deadline flush still
// satisfies. Neither notices if the final flush stops honoring ctx.
//
// Flushing a full buffer after the deadline fired is exactly the overshoot
// issue #166 closed -- up to crosslinkBatchSize crosslinks charged to a clone
// that was already out of time -- so it is worth a guard of its own.
func TestAfterDephantomizeCascadeAbandonsPartialBatchAtWalkPoll(t *testing.T) {
	r := setupTestRepo(t)

	// Fewer than crosslinkBatchSize, so nothing auto-flushes mid-walk and the
	// whole burst is still buffered when the deadline fires.
	root, children := buildSharedParentFanout(t, r, 500)

	stats := afterDephantomize(newPollCancelCtx(2), r, root, true)

	if stats.commits != 0 {
		t.Errorf("cascade committed %d transactions after cancellation, want 0: "+
			"the open batch was flushed past the deadline (issue #166's overshoot)", stats.commits)
	}
	if got := countCrosslinked(t, r, children); got != 0 {
		t.Errorf("crosslinked %d of %d children after the deadline fired, want 0",
			got, len(children))
	}
}
