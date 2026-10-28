package blob

import (
	"bytes"
	"errors"
	"math/rand"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// hostedLimit is the consumer's configured ceiling, below the 2,000,000-byte
// value limit the triggers in addHostedLimit enforce.
const hostedLimit = 1_990_000

// incompressible returns n deterministic pseudo-random bytes, which zlib
// cannot shrink below n.
func incompressible(n int, seed int64) []byte {
	b := make([]byte, n)
	_, _ = rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// addHostedLimit simulates a hosted backend's per-value ceiling: any value
// over 2,000,000 bytes written to blob or blobpart aborts the statement.
func addHostedLimit(t *testing.T, d *db.DB) {
	t.Helper()
	if err := ensureParts(d); err != nil {
		t.Fatalf("ensureParts: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TRIGGER hosted_blob_insert BEFORE INSERT ON blob WHEN length(NEW.content)>2000000
		 BEGIN SELECT RAISE(ABORT, 'hosted SQLite value too large'); END`,
		`CREATE TRIGGER hosted_blob_update BEFORE UPDATE ON blob WHEN length(NEW.content)>2000000
		 BEGIN SELECT RAISE(ABORT, 'hosted SQLite value too large'); END`,
		`CREATE TRIGGER hosted_part_insert BEFORE INSERT ON blobpart WHEN length(NEW.content)>2000000
		 BEGIN SELECT RAISE(ABORT, 'hosted SQLite value too large'); END`,
	} {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatalf("trigger: %v", err)
		}
	}
}

func withLimit(t *testing.T, n int) {
	t.Helper()
	t.Cleanup(SetMaxValueBytes(n))
}

func partCount(t *testing.T, q db.Querier, rid libfossil.FslID) int {
	t.Helper()
	if !hasParts(t, q) {
		return 0
	}
	var n int
	if err := q.QueryRow("SELECT count(*) FROM blobpart WHERE rid=?", rid).Scan(&n); err != nil {
		t.Fatalf("count parts: %v", err)
	}
	return n
}

func hasParts(t *testing.T, q db.Querier) bool {
	t.Helper()
	ok, err := partsExist(q)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return ok
}

// The trigger reproduction from issue #459: without a limit the store fails;
// with one, the same content round-trips and no value exceeds the ceiling.
func TestHostedValueLimit(t *testing.T) {
	d := setupTestDB(t)
	addHostedLimit(t, d)
	content := incompressible(2_500_000, 1)
	if _, _, err := Store(d, content); err == nil ||
		!strings.Contains(err.Error(), "hosted SQLite value too large") {
		t.Fatalf("unlimited store: got %v, want the trigger's rejection", err)
	}

	withLimit(t, hostedLimit)
	rid, _, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := Load(d, rid)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("round trip: %v", err)
	}
	var size int64
	var inline []byte
	if err := d.QueryRow("SELECT size, content FROM blob WHERE rid=?", rid).Scan(&size, &inline); err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) || inline != nil {
		t.Fatalf("row: size=%d inline=%d bytes, want size=%d and NULL content", size, len(inline), len(content))
	}
	var parts, largest int
	if err := d.QueryRow("SELECT count(*), max(length(content)) FROM blobpart WHERE rid=?", rid).
		Scan(&parts, &largest); err != nil {
		t.Fatal(err)
	}
	if parts != 3 || largest != PartBytes {
		t.Fatalf("parts=%d largest=%d, want 3 parts of at most %d", parts, largest, PartBytes)
	}
}

// With no limit configured the store is byte-identical to before: values go
// inline and no blobpart table is created.
func TestStoredOffCreatesNoPartsTable(t *testing.T) {
	d := setupTestDB(t)
	rid, _, err := Store(d, incompressible(2_500_000, 2))
	if err != nil {
		t.Fatal(err)
	}
	if hasParts(t, d) {
		t.Fatal("blobpart created with MaxValueBytes off")
	}
	if n, _, err := StoredHead(d, rid, 4); err != nil || n <= 2_000_000 {
		t.Fatalf("StoredHead: n=%d err=%v", n, err)
	}
}

func TestStoredFillsPhantomInParts(t *testing.T) {
	d := setupTestDB(t)
	addHostedLimit(t, d)
	withLimit(t, hostedLimit)
	content := incompressible(2_100_000, 3)
	uuid := mustNaming(t, d).Hash(content)
	phantom, err := StorePhantom(d, uuid)
	if err != nil {
		t.Fatal(err)
	}
	rid, _, outcome, err := StoreUnderPolicy(d, content)
	if err != nil || rid != phantom || outcome != FilledPhantom {
		t.Fatalf("fill: rid=%d outcome=%v err=%v", rid, outcome, err)
	}
	if got, err := Load(d, rid); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Load: %v", err)
	}
	if partCount(t, d, rid) != 3 {
		t.Fatalf("parts = %d, want 3", partCount(t, d, rid))
	}
}

// Rewriting a chunked row inline drops its parts; rewriting it chunked again
// replaces them; dropping it leaves a phantom and no parts.
func TestWriteStoredRewritesParts(t *testing.T) {
	d := setupTestDB(t)
	addHostedLimit(t, d)
	withLimit(t, hostedLimit)
	big := incompressible(2_100_000, 4)
	rid, _, err := Store(d, big)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := ReadStored(d, rid)
	if err != nil {
		t.Fatal(err)
	}
	n, head, err := StoredHead(d, rid, 6)
	if err != nil || n != int64(len(stored)) || !bytes.Equal(head, stored[:6]) {
		t.Fatalf("StoredHead: n=%d head=%x err=%v", n, head, err)
	}

	small := []byte("small stored value")
	if err := WriteStored(d, rid, small); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadStored(d, rid); err != nil || !bytes.Equal(got, small) {
		t.Fatalf("inline rewrite: %q %v", got, err)
	}
	if partCount(t, d, rid) != 0 {
		t.Fatal("inline rewrite kept parts")
	}

	if err := WriteStored(d, rid, stored); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadStored(d, rid); err != nil || !bytes.Equal(got, stored) {
		t.Fatalf("chunked rewrite: %v", err)
	}
	if err := DropStored(d, rid); err != nil {
		t.Fatal(err)
	}
	if partCount(t, d, rid) != 0 || !isPhantom(d, rid) {
		t.Fatal("DropStored left parts or a non-phantom row")
	}
}

// A failed write rolls back every part along with the row.
func TestStoredRollsBackParts(t *testing.T) {
	d := setupTestDB(t)
	withLimit(t, hostedLimit)
	boom := errors.New("boom")
	err := d.WithTx(func(tx *db.Tx) error {
		if _, _, err := Store(tx, incompressible(2_100_000, 5)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx: %v", err)
	}
	if hasParts(t, d) {
		var n int
		if err := d.QueryRow("SELECT count(*) FROM blobpart").Scan(&n); err != nil || n != 0 {
			t.Fatalf("parts after rollback: %d %v", n, err)
		}
	}
}

// A chunked row with a missing part is reported, never returned short.
func TestReadStoredMissingPart(t *testing.T) {
	d := setupTestDB(t)
	withLimit(t, hostedLimit)
	rid, _, err := Store(d, incompressible(2_100_000, 6))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("DELETE FROM blobpart WHERE rid=? AND seq=1", rid); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStored(d, rid); err == nil {
		t.Fatal("ReadStored returned a value with a part missing")
	}
	if _, err := Load(d, rid); err == nil {
		t.Fatal("Load returned a value with a part missing")
	}
}

// SweepParts removes parts whose row is gone or no longer chunked.
func TestSweepParts(t *testing.T) {
	d := setupTestDB(t)
	withLimit(t, hostedLimit)
	keep, _, err := Store(d, incompressible(2_100_000, 7))
	if err != nil {
		t.Fatal(err)
	}
	gone, _, err := Store(d, incompressible(2_100_000, 8))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("DELETE FROM blob WHERE rid=?", gone); err != nil {
		t.Fatal(err)
	}
	if err := SweepParts(d); err != nil {
		t.Fatal(err)
	}
	if partCount(t, d, gone) != 0 || partCount(t, d, keep) != 3 {
		t.Fatalf("sweep: gone=%d keep=%d", partCount(t, d, gone), partCount(t, d, keep))
	}
}

func TestInsertStoredChunks(t *testing.T) {
	d := setupTestDB(t)
	addHostedLimit(t, d)
	withLimit(t, hostedLimit)
	content := incompressible(2_100_000, 9)
	stored, err := Compress(content)
	if err != nil {
		t.Fatal(err)
	}
	uuid := mustNaming(t, d).Hash(content)
	rid, err := InsertStored(d, uuid, int64(len(content)), stored, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Load(d, rid); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Load: %v", err)
	}
}

func mustNaming(t *testing.T, q db.Querier) hash.Alg {
	t.Helper()
	naming, err := hash.NamingFor(q)
	if err != nil {
		t.Fatal(err)
	}
	return naming.New
}

// The scan expressions report the same length and prefix for chunked and
// inline rows, and -1 / NULL for a phantom.
func TestStoredSQLExpressions(t *testing.T) {
	d := setupTestDB(t)
	inline, _, err := Store(d, []byte("inline value"))
	if err != nil {
		t.Fatal(err)
	}
	phantom, err := StorePhantom(d, strings.Repeat("ab", 20))
	if err != nil {
		t.Fatal(err)
	}
	withLimit(t, hostedLimit)
	big, _, err := Store(d, incompressible(2_100_000, 10))
	if err != nil {
		t.Fatal(err)
	}
	lenSQL, err := StoredLenSQL(d, "b")
	if err != nil {
		t.Fatal(err)
	}
	prefixSQL, err := StoredPrefixSQL(d, "b", 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, rid := range []libfossil.FslID{inline, big, phantom} {
		var n int64
		var prefix []byte
		if err := d.QueryRow("SELECT "+lenSQL+", "+prefixSQL+" FROM blob b WHERE b.rid=?", rid).
			Scan(&n, &prefix); err != nil {
			t.Fatal(err)
		}
		if rid == phantom {
			if n != -1 || prefix != nil {
				t.Fatalf("phantom: n=%d prefix=%x", n, prefix)
			}
			continue
		}
		stored, err := ReadStored(d, rid)
		if err != nil {
			t.Fatal(err)
		}
		if n != int64(len(stored)) || !bytes.Equal(prefix, stored[:4]) {
			t.Fatalf("rid %d: n=%d prefix=%x, want %d %x", rid, n, prefix, len(stored), stored[:4])
		}
	}
}
