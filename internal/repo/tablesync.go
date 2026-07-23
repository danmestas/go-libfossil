package repo

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// ColumnDef defines a column in a synced table.
type ColumnDef struct {
	Name string `json:"name"`
	Type string `json:"type"` // "text", "integer", "real", "blob"
	PK   bool   `json:"pk"`   // Primary key component
}

// TableDef defines the schema and conflict resolution for a synced table.
type TableDef struct {
	Columns  []ColumnDef `json:"columns"`
	Conflict string      `json:"conflict"` // "mtime-wins", "self-write", "owner-write"
}

// TableInfo represents a registered synced table.
type TableInfo struct {
	Name     string   `json:"name"`
	Def      TableDef `json:"def"`
	MTime    int64    `json:"mtime"`
	CatHash  string   `json:"cat_hash"`  // Cached catalog hash
	HashTime int64    `json:"hash_time"` // Unix seconds when cat_hash computed
}

// Reserved Fossil core tables that cannot be used as synced table names.
// Only the most critical structural tables are reserved; operational tables
// like "config" are allowed since extension tables use the x_ prefix.
var reservedTables = map[string]bool{
	"blob":        true, // Core content storage
	"delta":       true, // Delta chains
	"event":       true, // Timeline/checkin manifests
	"mlink":       true, // File-to-checkin mappings
	"unversioned": true, // UV file storage
}

var validNameRegex = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ValidateTableName validates a synced table name.
// Must match ^[a-z_][a-z0-9_]*$, not be reserved, and not start with x_.
func ValidateTableName(name string) error {
	if name == "" {
		return fmt.Errorf("table name must not be empty")
	}
	if !validNameRegex.MatchString(name) {
		return fmt.Errorf("table name %q must match ^[a-z_][a-z0-9_]*$", name)
	}
	if reservedTables[name] {
		return fmt.Errorf("table name %q is reserved by Fossil", name)
	}
	if strings.HasPrefix(name, "x_") {
		return fmt.Errorf("table name %q must not start with x_ (reserved for extension tables)", name)
	}
	return nil
}

// ValidateColumnName validates a column name. Same rules as table name, but no reserved check.
func ValidateColumnName(name string) error {
	if name == "" {
		return fmt.Errorf("column name must not be empty")
	}
	if !validNameRegex.MatchString(name) {
		return fmt.Errorf("column name %q must match ^[a-z_][a-z0-9_]*$", name)
	}
	return nil
}

// EnsureSyncSchema creates the _sync_schema table if it doesn't exist.
func EnsureSyncSchema(d *db.DB) error {
	if d == nil {
		panic("repo.EnsureSyncSchema: d must not be nil")
	}
	const schema = `CREATE TABLE IF NOT EXISTS _sync_schema(
		name TEXT PRIMARY KEY,
		columns TEXT NOT NULL,
		conflict TEXT NOT NULL,
		mtime INTEGER NOT NULL,
		cat_hash TEXT,
		hash_time INTEGER
	)`
	_, err := d.Exec(schema)
	return err
}

// RegisterSyncedTable registers a table for sync and creates its extension table.
func RegisterSyncedTable(d *db.DB, name string, def TableDef, mtime int64) error {
	if d == nil {
		panic("repo.RegisterSyncedTable: d must not be nil")
	}
	if err := ValidateTableName(name); err != nil {
		return fmt.Errorf("repo.RegisterSyncedTable: %w", err)
	}
	if len(def.Columns) == 0 {
		return fmt.Errorf("repo.RegisterSyncedTable: table %q must have at least one column", name)
	}

	// Validate columns and check for at least one PK.
	hasPK := false
	for _, col := range def.Columns {
		if err := ValidateColumnName(col.Name); err != nil {
			return fmt.Errorf("repo.RegisterSyncedTable: %w", err)
		}
		if col.PK {
			hasPK = true
		}
	}
	if !hasPK {
		return fmt.Errorf("repo.RegisterSyncedTable: table %q must have at least one primary key column", name)
	}

	// Validate conflict mode.
	validConflict := map[string]bool{
		"mtime-wins":  true,
		"self-write":  true,
		"owner-write": true,
	}
	if !validConflict[def.Conflict] {
		return fmt.Errorf("repo.RegisterSyncedTable: invalid conflict mode %q (want mtime-wins, self-write, or owner-write)", def.Conflict)
	}

	// Marshal columns JSON.
	columnsJSON, err := json.Marshal(def.Columns)
	if err != nil {
		return fmt.Errorf("repo.RegisterSyncedTable: marshal columns: %w", err)
	}

	// Insert/update registry.
	_, err = d.Exec(
		`INSERT OR REPLACE INTO _sync_schema(name, columns, conflict, mtime, cat_hash, hash_time)
		 VALUES(?, ?, ?, ?, NULL, 0)`,
		name, string(columnsJSON), def.Conflict, mtime,
	)
	if err != nil {
		return fmt.Errorf("repo.RegisterSyncedTable: insert: %w", err)
	}

	// Create extension table.
	return createExtensionTable(d, name, def)
}

// createExtensionTable creates the x_<name> table with mtime and optional _owner.
func createExtensionTable(d *db.DB, name string, def TableDef) error {
	var cols []string
	var pkCols []string
	for _, col := range def.Columns {
		sqlType := col.Type
		// Normalize type.
		switch strings.ToLower(col.Type) {
		case "text", "integer", "real", "blob":
			sqlType = strings.ToUpper(col.Type)
		default:
			return fmt.Errorf("createExtensionTable: unsupported column type %q", col.Type)
		}
		if col.PK {
			cols = append(cols, fmt.Sprintf("%s %s NOT NULL", col.Name, sqlType))
		} else {
			cols = append(cols, fmt.Sprintf("%s %s", col.Name, sqlType))
		}
		if col.PK {
			pkCols = append(pkCols, col.Name)
		}
	}
	cols = append(cols, "mtime INTEGER NOT NULL")
	if def.Conflict == "owner-write" || def.Conflict == "self-write" {
		cols = append(cols, "_owner TEXT NOT NULL DEFAULT ''")
	}

	// Paired assertion — RegisterSyncedTable validates hasPK above, but
	// defend against future callers that bypass registration.
	if len(pkCols) == 0 {
		panic(fmt.Sprintf("createExtensionTable: table %q must have at least one PK column", name))
	}

	ddl := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS x_%s(\n  %s,\n  PRIMARY KEY(%s)\n)",
		name,
		strings.Join(cols, ",\n  "),
		strings.Join(pkCols, ", "),
	)
	_, err := d.Exec(ddl)
	if err != nil {
		return fmt.Errorf("createExtensionTable: %w", err)
	}
	return nil
}

// ListSyncedTables returns all registered synced tables.
func ListSyncedTables(d *db.DB) ([]TableInfo, error) {
	if d == nil {
		panic("repo.ListSyncedTables: d must not be nil")
	}
	rows, err := d.Query("SELECT name, columns, conflict, mtime, cat_hash, hash_time FROM _sync_schema ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("repo.ListSyncedTables: %w", err)
	}
	defer rows.Close()

	var tables []TableInfo
	for rows.Next() {
		var info TableInfo
		var columnsJSON string
		var catHash sql.NullString
		var hashTime sql.NullInt64
		if err := rows.Scan(&info.Name, &columnsJSON, &info.Def.Conflict, &info.MTime, &catHash, &hashTime); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(columnsJSON), &info.Def.Columns); err != nil {
			return nil, fmt.Errorf("repo.ListSyncedTables: unmarshal columns: %w", err)
		}
		if catHash.Valid {
			info.CatHash = catHash.String
		}
		if hashTime.Valid {
			info.HashTime = hashTime.Int64
		}
		tables = append(tables, info)
	}
	return tables, rows.Err()
}

// normalizeValue converts a value to its canonical string representation
// based on the declared column type. This ensures PK hashes are identical
// regardless of how the value arrived (JSON unmarshal, SQLite scan, etc.).
func normalizeValue(colType string, v any) string {
	if v == nil {
		panic(fmt.Sprintf("repo.normalizeValue: value must not be nil for column type %q", colType))
	}
	switch colType {
	case "integer":
		switch n := v.(type) {
		case int64:
			return strconv.FormatInt(n, 10)
		case float64:
			return strconv.FormatInt(int64(n), 10)
		case int:
			return strconv.FormatInt(int64(n), 10)
		case json.Number:
			i, err := n.Int64()
			if err != nil {
				panic(fmt.Sprintf("repo.normalizeValue: integer column got non-integer json.Number %q", n))
			}
			return strconv.FormatInt(i, 10)
		default:
			panic(fmt.Sprintf("repo.normalizeValue: integer column got unexpected type %T", v))
		}
	case "real":
		switch n := v.(type) {
		case float64:
			return strconv.FormatFloat(n, 'f', -1, 64)
		case int64:
			return strconv.FormatFloat(float64(n), 'f', -1, 64)
		case json.Number:
			f, err := n.Float64()
			if err != nil {
				panic(fmt.Sprintf("repo.normalizeValue: real column got non-float json.Number %q", n))
			}
			return strconv.FormatFloat(f, 'f', -1, 64)
		default:
			panic(fmt.Sprintf("repo.normalizeValue: real column got unexpected type %T", v))
		}
	case "text":
		s, ok := v.(string)
		if !ok {
			panic(fmt.Sprintf("repo.normalizeValue: text column got unexpected type %T", v))
		}
		return s
	case "blob":
		b, ok := v.([]byte)
		if !ok {
			panic(fmt.Sprintf("repo.normalizeValue: blob column got unexpected type %T", v))
		}
		return hex.EncodeToString(b)
	default:
		panic(fmt.Sprintf("repo.normalizeValue: unsupported column type %q", colType))
	}
}

// PKHash computes a deterministic SHA1 hash of the primary key values.
// Values are normalized to canonical strings based on declared column types
// to avoid JSON round-trip coercion issues (e.g., int64 → float64).
func PKHash(pkCols []ColumnDef, pkValues map[string]any) string {
	if pkCols == nil {
		panic("repo.PKHash: pkCols must not be nil")
	}
	if pkValues == nil {
		panic("repo.PKHash: pkValues must not be nil")
	}

	// Sort PK columns lexicographically for determinism.
	sorted := make([]ColumnDef, len(pkCols))
	copy(sorted, pkCols)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	// Build canonical representation: name=normalizedValue joined by \x00.
	var parts []string
	for _, col := range sorted {
		v := pkValues[col.Name]
		parts = append(parts, col.Name+"="+normalizeValue(col.Type, v))
	}
	canonical := strings.Join(parts, "\x00")
	return hash.SHA1([]byte(canonical))
}

// UpsertXRow inserts or replaces a row in the extension table.
func UpsertXRow(d *db.DB, tableName string, row map[string]any, mtime int64) error {
	if d == nil {
		panic("repo.UpsertXRow: d must not be nil")
	}
	if tableName == "" {
		panic("repo.UpsertXRow: tableName must not be empty")
	}
	if row == nil {
		panic("repo.UpsertXRow: row must not be nil")
	}

	// Build column list and placeholders. Sort keys for deterministic SQL.
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var cols []string
	var placeholders []string
	var values []any
	for _, k := range keys {
		cols = append(cols, k)
		placeholders = append(placeholders, "?")
		values = append(values, row[k])
	}
	cols = append(cols, "mtime")
	placeholders = append(placeholders, "?")
	values = append(values, mtime)

	sql := fmt.Sprintf(
		"INSERT OR REPLACE INTO x_%s(%s) VALUES(%s)",
		tableName,
		strings.Join(cols, ", "),
		strings.Join(placeholders, ", "),
	)
	_, err := d.Exec(sql, values...)
	if err != nil {
		return fmt.Errorf("repo.UpsertXRow: %w", err)
	}
	return nil
}

// ListXRows returns all rows from the extension table.
func ListXRows(d *db.DB, tableName string, def TableDef) ([]map[string]any, []int64, error) {
	if d == nil {
		panic("repo.ListXRows: d must not be nil")
	}
	if tableName == "" {
		panic("repo.ListXRows: tableName must not be empty")
	}

	sql := fmt.Sprintf("SELECT * FROM x_%s ORDER BY mtime", tableName)
	rows, err := d.Query(sql)
	if err != nil {
		return nil, nil, fmt.Errorf("repo.ListXRows: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, fmt.Errorf("repo.ListXRows: columns: %w", err)
	}

	var results []map[string]any
	var mtimes []int64
	for rows.Next() {
		// Create slice of interface{} to scan into.
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, nil, err
		}

		// Convert to map, extract mtime.
		rowMap := make(map[string]any)
		var mtime int64
		for i, col := range columns {
			if col == "mtime" {
				mtime = values[i].(int64)
			} else if col != "_owner" {
				rowMap[col] = values[i]
			}
		}
		results = append(results, rowMap)
		mtimes = append(mtimes, mtime)
	}
	return results, mtimes, rows.Err()
}

// LookupXRow returns a row by primary key hash.
func LookupXRow(d *db.DB, tableName string, def TableDef, pkHash string) (map[string]any, int64, error) {
	if d == nil {
		panic("repo.LookupXRow: d must not be nil")
	}
	if tableName == "" {
		panic("repo.LookupXRow: tableName must not be empty")
	}
	if pkHash == "" {
		panic("repo.LookupXRow: pkHash must not be empty")
	}

	rows, mtimes, err := ListXRows(d, tableName, def)
	if err != nil {
		return nil, 0, err
	}

	// Extract PK columns (with type info for normalizeValue).
	var pkColDefs []ColumnDef
	for _, col := range def.Columns {
		if col.PK {
			pkColDefs = append(pkColDefs, col)
		}
	}

	// Find matching row.
	for i, row := range rows {
		pkValues := make(map[string]any)
		for _, col := range pkColDefs {
			pkValues[col.Name] = row[col.Name]
		}
		computed := PKHash(pkColDefs, pkValues)
		if computed == pkHash {
			// Postcondition: re-derive PK hash from the row we're about to
			// return and verify it matches the requested hash. Guards against
			// PK column extraction bugs.
			verifyPK := make(map[string]any)
			for _, col := range pkColDefs {
				verifyPK[col.Name] = row[col.Name]
			}
			if PKHash(pkColDefs, verifyPK) != pkHash {
				panic(fmt.Sprintf("repo.LookupXRow: postcondition violated: re-derived PK hash != %q", pkHash))
			}
			return row, mtimes[i], nil
		}
	}
	return nil, 0, nil
}

// LookupXRowOwner returns the _owner field for the row identified by pkHash,
// or "" if the row doesn't exist or the table has no owner column.
func LookupXRowOwner(d *db.DB, tableName string, def TableDef, pkHash string) (string, error) {
	if d == nil {
		panic("repo.LookupXRowOwner: d must not be nil")
	}
	if tableName == "" {
		panic("repo.LookupXRowOwner: tableName must not be empty")
	}
	if pkHash == "" {
		panic("repo.LookupXRowOwner: pkHash must not be empty")
	}

	// Only tables with self-write/owner-write conflict have an _owner column.
	hasOwner := def.Conflict == "self-write" || def.Conflict == "owner-write"
	if !hasOwner {
		return "", nil
	}

	// Scan all rows to find the one matching pkHash (same strategy as LookupXRow).
	sql := fmt.Sprintf("SELECT * FROM x_%s", tableName)
	rows, err := d.Query(sql)
	if err != nil {
		return "", fmt.Errorf("repo.LookupXRowOwner: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("repo.LookupXRowOwner: columns: %w", err)
	}

	var pkColDefs []ColumnDef
	for _, col := range def.Columns {
		if col.PK {
			pkColDefs = append(pkColDefs, col)
		}
	}

	for rows.Next() {
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return "", err
		}

		rowMap := make(map[string]any)
		var owner string
		for i, col := range columns {
			if col == "_owner" {
				if v, ok := values[i].(string); ok {
					owner = v
				}
			} else if col != "mtime" {
				rowMap[col] = values[i]
			}
		}

		pkValues := make(map[string]any)
		for _, col := range pkColDefs {
			pkValues[col.Name] = rowMap[col.Name]
		}
		if PKHash(pkColDefs, pkValues) == pkHash {
			return owner, nil
		}
	}
	return "", rows.Err()
}

// IsTombstone returns true if all non-PK value columns in the row are nil.
// A tombstone represents a deleted row (UV-style convention).
// Returns false for tables with only PK columns (no value columns to NULL).
func IsTombstone(def TableDef, row map[string]any) bool {
	if row == nil {
		panic("repo.IsTombstone: row must not be nil")
	}
	if len(def.Columns) == 0 {
		panic("repo.IsTombstone: def.Columns must not be empty")
	}
	hasValueCol := false
	for _, col := range def.Columns {
		if col.PK {
			continue
		}
		hasValueCol = true
		if row[col.Name] != nil {
			return false
		}
	}
	return hasValueCol
}

// DeleteXRowByPKHash tombstones a row identified by PK hash. Sets all non-PK
// value columns to NULL and updates mtime. Returns true if applied, false if
// row doesn't exist or has a newer mtime.
func DeleteXRowByPKHash(d *db.DB, tableName string, def TableDef, pkHash string, mtime int64) (bool, error) {
	if d == nil {
		panic("repo.DeleteXRowByPKHash: d must not be nil")
	}
	if tableName == "" {
		panic("repo.DeleteXRowByPKHash: tableName must not be empty")
	}
	if pkHash == "" {
		panic("repo.DeleteXRowByPKHash: pkHash must not be empty")
	}
	if len(def.Columns) == 0 {
		panic("repo.DeleteXRowByPKHash: def.Columns must not be empty")
	}

	row, currentMtime, err := LookupXRow(d, tableName, def, pkHash)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, nil
	}
	if currentMtime > mtime {
		return false, nil
	}

	var pkCols []string
	var pkValues []any
	for _, col := range def.Columns {
		if col.PK {
			pkCols = append(pkCols, col.Name)
			pkValues = append(pkValues, row[col.Name])
		}
	}

	var setClauses []string
	for _, col := range def.Columns {
		if col.PK {
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = NULL", col.Name))
	}
	setClauses = append(setClauses, "mtime = ?")

	var whereClauses []string
	for _, pk := range pkCols {
		whereClauses = append(whereClauses, fmt.Sprintf("%s = ?", pk))
	}

	args := []any{mtime}
	args = append(args, pkValues...)

	sqlStr := fmt.Sprintf(
		"UPDATE x_%s SET %s WHERE %s",
		tableName,
		strings.Join(setClauses, ", "),
		strings.Join(whereClauses, " AND "),
	)
	_, err = d.Exec(sqlStr, args...)
	if err != nil {
		return false, fmt.Errorf("repo.DeleteXRowByPKHash: update: %w", err)
	}
	return true, nil
}

// CatalogHash computes the SHA1 hash of all rows in the extension table.
// Format: "pk_hash mtime\n" for each row, sorted by pk_hash.
func CatalogHash(d *db.DB, tableName string, def TableDef) (string, error) {
	if d == nil {
		panic("repo.CatalogHash: d must not be nil")
	}
	if tableName == "" {
		panic("repo.CatalogHash: tableName must not be empty")
	}

	rows, mtimes, err := ListXRows(d, tableName, def)
	if err != nil {
		return "", err
	}

	// Extract PK columns (with type info for normalizeValue).
	var pkColDefs []ColumnDef
	for _, col := range def.Columns {
		if col.PK {
			pkColDefs = append(pkColDefs, col)
		}
	}

	// Build list of "pk_hash mtime" entries.
	type entry struct {
		pkHash string
		mtime  int64
	}
	var entries []entry
	for i, row := range rows {
		// Exclude tombstones from catalog hash (UV convention).
		if IsTombstone(def, row) {
			continue
		}
		pkValues := make(map[string]any)
		for _, col := range pkColDefs {
			pkValues[col.Name] = row[col.Name]
		}
		entries = append(entries, entry{
			pkHash: PKHash(pkColDefs, pkValues),
			mtime:  mtimes[i],
		})
	}

	// Sort by pk_hash.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].pkHash < entries[j].pkHash
	})

	// Concatenate and hash.
	var buf strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&buf, "%s %d\n", e.pkHash, e.mtime)
	}
	result := hash.SHA1([]byte(buf.String()))
	if len(result) != 40 {
		panic(fmt.Sprintf("repo.CatalogHash: expected 40-char SHA1 hex, got %d chars", len(result)))
	}
	return result, nil
}
