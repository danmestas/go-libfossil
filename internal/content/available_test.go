package content

import (
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
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
