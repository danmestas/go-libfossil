package repo

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/simio"
)

func setupTSRepo(t *testing.T) *Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ts.fossil")
	r, err := Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestValidateTableName(t *testing.T) {
	valid := []string{"peer_registry", "config", "a", "abc_123"}
	for _, name := range valid {
		if err := ValidateTableName(name); err != nil {
			t.Errorf("ValidateTableName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{"", "Peer", "123abc", "drop;table", "blob", "delta", "event", "unversioned", "x_reserved"}
	for _, name := range invalid {
		if err := ValidateTableName(name); err == nil {
			t.Errorf("ValidateTableName(%q) = nil, want error", name)
		}
	}
}

func TestPKHash(t *testing.T) {
	pkCols := []ColumnDef{{Name: "peer_id", Type: "text", PK: true}}
	h := PKHash(pkCols, map[string]any{"peer_id": "leaf-01"})
	if h == "" {
		t.Fatal("PKHash returned empty")
	}
	h2 := PKHash(pkCols, map[string]any{"peer_id": "leaf-01"})
	if h != h2 {
		t.Fatalf("PKHash not deterministic: %q vs %q", h, h2)
	}
	h3 := PKHash(pkCols, map[string]any{"peer_id": "leaf-02"})
	if h == h3 {
		t.Fatal("different inputs produced same hash")
	}
}

func TestPKHashTypeAware(t *testing.T) {
	pkCols := []ColumnDef{{Name: "id", Type: "integer", PK: true}}

	// int64 and float64 of same value must produce identical hashes.
	h1 := PKHash(pkCols, map[string]any{"id": int64(42)})
	h2 := PKHash(pkCols, map[string]any{"id": float64(42)})
	if h1 != h2 {
		t.Fatalf("int64 vs float64: %q != %q", h1, h2)
	}

	// Large integer (>2^53): float64 loses precision before PKHash even sees it.
	// float64(1<<53+1) == float64(1<<53) due to IEEE 754 rounding, so the
	// hashes will differ — that is correct and expected. We verify that the
	// int64 path at least produces a stable, non-empty hash.
	big := int64(1<<53 + 1) // 9007199254740993
	h3 := PKHash(pkCols, map[string]any{"id": big})
	if h3 == "" {
		t.Fatal("large int64 PK hash is empty")
	}
	// Verify the int64 hash is deterministic.
	h3b := PKHash(pkCols, map[string]any{"id": big})
	if h3 != h3b {
		t.Fatalf("large int64 PK hash not deterministic: %q vs %q", h3, h3b)
	}

	// Composite PK with mixed types.
	mixedCols := []ColumnDef{
		{Name: "org", Type: "text", PK: true},
		{Name: "seq", Type: "integer", PK: true},
	}
	h5 := PKHash(mixedCols, map[string]any{"org": "acme", "seq": int64(7)})
	h6 := PKHash(mixedCols, map[string]any{"org": "acme", "seq": float64(7)})
	if h5 != h6 {
		t.Fatalf("composite mixed types: %q != %q", h5, h6)
	}

	// Text PK must still work.
	textCols := []ColumnDef{{Name: "name", Type: "text", PK: true}}
	h7 := PKHash(textCols, map[string]any{"name": "hello"})
	if h7 == "" {
		t.Fatal("text PK hash is empty")
	}
}

func TestEnsureSyncSchema(t *testing.T) {
	r := setupTSRepo(t)
	if err := EnsureSyncSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSyncSchema: %v", err)
	}
	if err := EnsureSyncSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSyncSchema (2nd): %v", err)
	}
}

func TestRegisterAndListSyncedTables(t *testing.T) {
	r := setupTSRepo(t)
	if err := EnsureSyncSchema(r.DB()); err != nil {
		t.Fatal(err)
	}
	def := TableDef{
		Columns:  []ColumnDef{{Name: "peer_id", Type: "text", PK: true}, {Name: "addr", Type: "text"}},
		Conflict: "self-write",
	}
	if err := RegisterSyncedTable(r.DB(), "peer_registry", def, 1711300000); err != nil {
		t.Fatalf("RegisterSyncedTable: %v", err)
	}
	tables, err := ListSyncedTables(r.DB())
	if err != nil {
		t.Fatalf("ListSyncedTables: %v", err)
	}
	if len(tables) != 1 || tables[0].Name != "peer_registry" {
		t.Fatalf("got %+v, want 1 table named peer_registry", tables)
	}
	var count int
	if err := r.DB().QueryRow("SELECT count(*) FROM x_peer_registry").Scan(&count); err != nil {
		t.Fatalf("x_peer_registry should exist: %v", err)
	}
}

func TestUpsertAndLookupXRow(t *testing.T) {
	r := setupTSRepo(t)
	EnsureSyncSchema(r.DB())
	def := TableDef{
		Columns:  []ColumnDef{{Name: "peer_id", Type: "text", PK: true}, {Name: "addr", Type: "text"}},
		Conflict: "self-write",
	}
	RegisterSyncedTable(r.DB(), "peer_registry", def, 1711300000)

	row := map[string]any{"peer_id": "leaf-01", "addr": "10.0.0.1:9000"}
	if err := UpsertXRow(r.DB(), "peer_registry", row, 1711300000); err != nil {
		t.Fatalf("UpsertXRow: %v", err)
	}

	pkCols := []ColumnDef{{Name: "peer_id", Type: "text", PK: true}}
	pkHash := PKHash(pkCols, map[string]any{"peer_id": "leaf-01"})
	got, mtime, err := LookupXRow(r.DB(), "peer_registry", def, pkHash)
	if err != nil {
		t.Fatalf("LookupXRow: %v", err)
	}
	if got == nil {
		t.Fatal("row not found")
	}
	if mtime != 1711300000 {
		t.Fatalf("mtime = %d, want 1711300000", mtime)
	}
}

func TestExtensionTableNullableValueColumns(t *testing.T) {
	r := setupTSRepo(t)
	EnsureSyncSchema(r.DB())
	def := TableDef{
		Columns:  []ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "data", Type: "text"}},
		Conflict: "mtime-wins",
	}
	if err := RegisterSyncedTable(r.DB(), "nullable_test", def, 1000); err != nil {
		t.Fatal(err)
	}
	// Insert row with NULL value column — should succeed for tombstone support.
	_, err := r.DB().Exec("INSERT INTO x_nullable_test(id, data, mtime) VALUES('k1', NULL, 1000)")
	if err != nil {
		t.Fatalf("insert with NULL value column should succeed: %v", err)
	}
}

func TestIsTombstone(t *testing.T) {
	def := TableDef{
		Columns: []ColumnDef{
			{Name: "id", Type: "text", PK: true},
			{Name: "data", Type: "text"},
			{Name: "count", Type: "integer"},
		},
		Conflict: "mtime-wins",
	}
	if IsTombstone(def, map[string]any{"id": "k1", "data": "hello", "count": int64(5)}) {
		t.Error("live row should not be tombstone")
	}
	if !IsTombstone(def, map[string]any{"id": "k1", "data": nil, "count": nil}) {
		t.Error("row with all nil values should be tombstone")
	}
	if IsTombstone(def, map[string]any{"id": "k1", "data": nil, "count": int64(5)}) {
		t.Error("partial nil should not be tombstone")
	}
}

func TestDeleteXRowByPKHash(t *testing.T) {
	r := setupTSRepo(t)
	EnsureSyncSchema(r.DB())
	def := TableDef{
		Columns:  []ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "data", Type: "text"}},
		Conflict: "mtime-wins",
	}
	RegisterSyncedTable(r.DB(), "del_test", def, 1000)
	UpsertXRow(r.DB(), "del_test", map[string]any{"id": "k1", "data": "hello"}, 1000)

	pkColDefs := []ColumnDef{{Name: "id", Type: "text", PK: true}}
	pkHash := PKHash(pkColDefs, map[string]any{"id": "k1"})
	deleted, err := DeleteXRowByPKHash(r.DB(), "del_test", def, pkHash, 2000)
	if err != nil {
		t.Fatalf("DeleteXRowByPKHash: %v", err)
	}
	if !deleted {
		t.Fatal("expected deletion to apply")
	}

	row, mtime, err := LookupXRow(r.DB(), "del_test", def, pkHash)
	if err != nil {
		t.Fatalf("LookupXRow: %v", err)
	}
	if row == nil {
		t.Fatal("tombstone row should still exist")
	}
	if mtime != 2000 {
		t.Errorf("mtime = %d, want 2000", mtime)
	}
	if !IsTombstone(def, row) {
		t.Error("row should be tombstone after deletion")
	}

	deleted, err = DeleteXRowByPKHash(r.DB(), "del_test", def, pkHash, 1500)
	if err != nil {
		t.Fatalf("DeleteXRowByPKHash (older): %v", err)
	}
	if deleted {
		t.Error("older delete should be rejected")
	}

	UpsertXRow(r.DB(), "del_test", map[string]any{"id": "k1", "data": "revived"}, 3000)
	row, _, _ = LookupXRow(r.DB(), "del_test", def, pkHash)
	if IsTombstone(def, row) {
		t.Error("row should be live after resurrection")
	}
}

func TestCatalogHashExcludesTombstones(t *testing.T) {
	r := setupTSRepo(t)
	EnsureSyncSchema(r.DB())
	def := TableDef{
		Columns:  []ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "data", Type: "text"}},
		Conflict: "mtime-wins",
	}
	RegisterSyncedTable(r.DB(), "cat_test", def, 1000)

	UpsertXRow(r.DB(), "cat_test", map[string]any{"id": "k1", "data": "a"}, 1000)
	UpsertXRow(r.DB(), "cat_test", map[string]any{"id": "k2", "data": "b"}, 1000)
	hashBefore, _ := CatalogHash(r.DB(), "cat_test", def)

	pkColDefs := []ColumnDef{{Name: "id", Type: "text", PK: true}}
	pkHash := PKHash(pkColDefs, map[string]any{"id": "k1"})
	DeleteXRowByPKHash(r.DB(), "cat_test", def, pkHash, 2000)
	hashAfter, _ := CatalogHash(r.DB(), "cat_test", def)

	if hashBefore == hashAfter {
		t.Error("catalog hash should change after deletion")
	}

	pkHash2 := PKHash(pkColDefs, map[string]any{"id": "k2"})
	DeleteXRowByPKHash(r.DB(), "cat_test", def, pkHash2, 2000)
	hashEmpty, _ := CatalogHash(r.DB(), "cat_test", def)

	RegisterSyncedTable(r.DB(), "cat_empty", def, 1000)
	hashFresh, _ := CatalogHash(r.DB(), "cat_empty", def)
	if hashEmpty != hashFresh {
		t.Errorf("all-tombstone table hash %q != empty table hash %q", hashEmpty, hashFresh)
	}
}

func TestCatalogHash(t *testing.T) {
	r := setupTSRepo(t)
	EnsureSyncSchema(r.DB())
	def := TableDef{
		Columns:  []ColumnDef{{Name: "peer_id", Type: "text", PK: true}},
		Conflict: "mtime-wins",
	}
	RegisterSyncedTable(r.DB(), "cfg", def, 100)

	UpsertXRow(r.DB(), "cfg", map[string]any{"peer_id": "a"}, 100)
	UpsertXRow(r.DB(), "cfg", map[string]any{"peer_id": "b"}, 200)

	h1, err := CatalogHash(r.DB(), "cfg", def)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("empty catalog hash")
	}
	h2, _ := CatalogHash(r.DB(), "cfg", def)
	if h1 != h2 {
		t.Fatal("catalog hash not deterministic")
	}
	UpsertXRow(r.DB(), "cfg", map[string]any{"peer_id": "c"}, 300)
	h3, _ := CatalogHash(r.DB(), "cfg", def)
	if h1 == h3 {
		t.Fatal("catalog hash unchanged after insert")
	}
}
