package content

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/blob"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func TestIsAvailable_Phantom(t *testing.T) {
	d := setupTestDB(t)

	rid, err := blob.StorePhantom(d, "0000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}

	if IsAvailable(d, rid) {
		t.Fatalf("IsAvailable(phantom rid=%d) = true, want false", rid)
	}
}

// A delta whose base is a phantom is unavailable even though every blob row
// in the chain exists and the delta's own size is >= 0. A size-only check
// passes this case; the transitive walk must not.
func TestIsAvailable_DeltaOnPhantom(t *testing.T) {
	d := setupTestDB(t)

	baseRid, err := blob.StorePhantom(d, "0000000000000000000000000000000000000002")
	if err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}

	// Store a delta row directly: the blob exists with a real size, but its
	// delta source is the phantom above. This is the normal steady state
	// during clone when a delta arrives before its base.
	res, err := d.Exec(
		"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, 42, x'00', 1)",
		"0000000000000000000000000000000000000003",
	)
	if err != nil {
		t.Fatalf("insert delta blob: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	deltaRid := libfossil.FslID(id)
	if _, err := d.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", deltaRid, baseRid); err != nil {
		t.Fatalf("insert delta: %v", err)
	}

	// Sanity: a size-only check would report this as present.
	var size int64
	if err := d.QueryRow("SELECT size FROM blob WHERE rid=?", deltaRid).Scan(&size); err != nil {
		t.Fatalf("scan size: %v", err)
	}
	if size < 0 {
		t.Fatalf("test setup wrong: delta blob size = %d, want >= 0", size)
	}

	if IsAvailable(d, deltaRid) {
		t.Fatalf("IsAvailable(delta-on-phantom rid=%d) = true, want false", deltaRid)
	}
}

func TestIsAvailable_UnknownRid(t *testing.T) {
	d := setupTestDB(t)

	if IsAvailable(d, libfossil.FslID(9999)) {
		t.Fatalf("IsAvailable(unknown rid) = true, want false")
	}
}

func TestIsAvailable_GroundedChain(t *testing.T) {
	d := setupTestDB(t)

	source := []byte("the original source content for delta testing purposes here")
	target := []byte("the original source content for MODIFIED testing purposes here")

	srcRid, _, err := blob.Store(d, source)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	deltaRid, _, err := blob.StoreDelta(d, target, srcRid)
	if err != nil {
		t.Fatalf("StoreDelta: %v", err)
	}

	if !IsAvailable(d, srcRid) {
		t.Fatalf("IsAvailable(full-text rid=%d) = false, want true", srcRid)
	}
	if !IsAvailable(d, deltaRid) {
		t.Fatalf("IsAvailable(grounded delta rid=%d) = false, want true", deltaRid)
	}
}

// A cyclic delta table must terminate rather than hang the process. Cycles
// are reachable from a corrupt repository or from hostile input over the
// wire during clone, so the walk carries an explicit bound.
func TestIsAvailable_CyclicChainTerminates(t *testing.T) {
	d := setupTestDB(t)

	var rids []libfossil.FslID
	for i := 0; i < 3; i++ {
		res, err := d.Exec(
			"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, 42, x'00', 1)",
			[]string{
				"000000000000000000000000000000000000000a",
				"000000000000000000000000000000000000000b",
				"000000000000000000000000000000000000000c",
			}[i],
		)
		if err != nil {
			t.Fatalf("insert blob %d: %v", i, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}
		rids = append(rids, libfossil.FslID(id))
	}
	// a -> b -> c -> a
	for i := range rids {
		src := rids[(i+1)%len(rids)]
		if _, err := d.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", rids[i], src); err != nil {
			t.Fatalf("insert delta: %v", err)
		}
	}

	done := make(chan bool, 1)
	go func() { done <- IsAvailable(d, rids[0]) }()

	select {
	case got := <-done:
		if got {
			t.Fatalf("IsAvailable(cyclic chain) = true, want false")
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("IsAvailable(cyclic chain) did not terminate")
	}
}

func TestAvailableByUUID(t *testing.T) {
	d := setupTestDB(t)

	content := []byte("available by uuid content")
	rid, uuid, err := blob.Store(d, content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	gotRid, ok := AvailableByUUID(d, uuid)
	if !ok || gotRid != rid {
		t.Fatalf("AvailableByUUID(filled) = (%d, %v), want (%d, true)", gotRid, ok, rid)
	}

	phantomUUID := "000000000000000000000000000000000000000f"
	if _, err := blob.StorePhantom(d, phantomUUID); err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}
	if _, ok := AvailableByUUID(d, phantomUUID); ok {
		t.Fatalf("AvailableByUUID(phantom) = true, want false")
	}

	if _, ok := AvailableByUUID(d, "00000000000000000000000000000000deadbeef"); ok {
		t.Fatalf("AvailableByUUID(unknown) = true, want false")
	}
}

// deltaQueryFailer delegates every query to the wrapped Querier except the
// chain-step lookup that reads a blob's delta linkage, which it rewrites into
// one that genuinely fails. That yields a real error on the exact path
// IsAvailable uses to decide a chain is grounded.
type deltaQueryFailer struct {
	db.Querier
}

func (f deltaQueryFailer) QueryRow(query string, args ...any) *sql.Row {
	if strings.Contains(query, "delta") {
		return f.Querier.QueryRow("SELECT no_such_column FROM no_such_table")
	}
	return f.Querier.QueryRow(query, args...)
}

// A chain-step lookup that fails tells us nothing about whether the chain is
// grounded. Reporting the content available on that basis would resurrect the
// exact bug this predicate exists to prevent — claiming readable content we
// never verified — under error conditions, on data arriving over the wire
// during clone.
func TestIsAvailable_DeltaLookupErrorFailsClosed(t *testing.T) {
	d := setupTestDB(t)

	// A perfectly ordinary full-text blob: no delta row, size >= 0. Against
	// the real DB this is available, which is what makes the injected failure
	// the only difference between the two assertions below.
	rid, _, err := blob.Store(d, []byte("grounded full text"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if !IsAvailable(d, rid) {
		t.Fatalf("IsAvailable(full-text rid=%d) = false, want true (test premise broken)", rid)
	}

	if IsAvailable(deltaQueryFailer{d}, rid) {
		t.Fatalf("IsAvailable(rid=%d) = true when the delta lookup errored; "+
			"must fail closed on anything but sql.ErrNoRows", rid)
	}
}
