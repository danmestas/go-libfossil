package repo

import "testing"

var identKV = TableDef{
	Columns:  []ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "val", Type: "text"}},
	Conflict: "mtime-wins",
}

func setupIdentRepo(t *testing.T) *Repo {
	t.Helper()
	r := setupTSRepo(t)
	if err := EnsureSyncSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSyncSchema: %v", err)
	}
	if err := RegisterSyncedTable(r.DB(), "kv", identKV, 100); err != nil {
		t.Fatalf("RegisterSyncedTable: %v", err)
	}
	return r
}

func countKV(t *testing.T, r *Repo) int {
	t.Helper()
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM x_kv").Scan(&n); err != nil {
		t.Fatalf("count x_kv: %v", err)
	}
	return n
}

// TestUpsertXRowRefusesUndeclaredColumns checks that a row key is used as a
// column name only when the table's registered schema declares it. Method:
// upsert rows whose extra key is a statement fragment. UpsertXRow wrote keys
// into its INSERT unchecked, so the first key below replaced the statement's
// column list and VALUES with the caller's own and stored the row 'inj'.
func TestUpsertXRowRefusesUndeclaredColumns(t *testing.T) {
	keys := []string{
		"val, mtime) VALUES('inj','x',7) --",
		"nosuch",
		"_owner", // only ownership-mode tables have it
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			r := setupIdentRepo(t)
			err := UpsertXRow(r.DB(), "kv", map[string]any{"id": "row1", key: "x"}, 1)
			if err == nil {
				t.Fatalf("UpsertXRow accepted column %q", key)
			}
			if n := countKV(t, r); n != 0 {
				t.Fatalf("x_kv holds %d rows after a refused upsert", n)
			}
		})
	}
}

// TestTableSyncSQLRefusesBadTableNames checks every function that writes a
// synced table's name into SQL text. Method: pass a name that is a statement
// fragment; each must return an error without running it.
func TestTableSyncSQLRefusesBadTableNames(t *testing.T) {
	r := setupIdentRepo(t)
	const bad = "kv WHERE 0; DELETE FROM blob; --"
	owned := identKV
	owned.Conflict = "owner-write"
	if err := UpsertXRow(r.DB(), bad, map[string]any{"id": "a"}, 1); err == nil {
		t.Error("UpsertXRow accepted a bad table name")
	}
	if err := UpsertXRow(r.DB(), "unregistered", map[string]any{"id": "a"}, 1); err == nil {
		t.Error("UpsertXRow accepted an unregistered table")
	}
	if _, _, err := ListXRows(r.DB(), bad, identKV); err == nil {
		t.Error("ListXRows accepted a bad table name")
	}
	if _, err := LookupXRowOwner(r.DB(), bad, owned, "abc"); err == nil {
		t.Error("LookupXRowOwner accepted a bad table name")
	}
	if _, err := DeleteXRowByPKHash(r.DB(), bad, identKV, "abc", 1); err == nil {
		t.Error("DeleteXRowByPKHash accepted a bad table name")
	}
	badCol := TableDef{Columns: []ColumnDef{{Name: "id = 1; --", Type: "text", PK: true}}}
	if _, err := DeleteXRowByPKHash(r.DB(), "kv", badCol, "abc", 1); err == nil {
		t.Error("DeleteXRowByPKHash accepted a bad column name")
	}
}
