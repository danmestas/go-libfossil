package sync

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// SyncedTable caches a table definition along with its metadata.
type SyncedTable struct {
	Info repo.TableInfo
	Def  repo.TableDef
}

// loadSyncedTables loads all registered synced tables into the handler cache.
func (h *handler) loadSyncedTables() error {
	if err := repo.EnsureSyncSchema(h.repo.DB()); err != nil {
		return fmt.Errorf("handler.loadSyncedTables: ensure schema: %w", err)
	}

	tables, err := repo.ListSyncedTables(h.repo.DB())
	if err != nil {
		return fmt.Errorf("handler.loadSyncedTables: list tables: %w", err)
	}

	h.syncedTables = make(map[string]*SyncedTable)
	for _, info := range tables {
		h.syncedTables[info.Name] = &SyncedTable{
			Info: info,
			Def:  info.Def,
		}
	}
	return nil
}

// handleSchemaCard processes a schema declaration from the client.
func (h *handler) handleSchemaCard(c *xfer.SchemaCard) {
	if c == nil {
		panic("handler.handleSchemaCard: c must not be nil")
	}

	// Validate table name.
	if err := repo.ValidateTableName(c.Table); err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("schema %s: %v", c.Table, err),
		})
		return
	}

	// Unmarshal and validate definition.
	var def repo.TableDef
	if err := json.Unmarshal(c.Content, &def); err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("schema %s: unmarshal: %v", c.Table, err),
		})
		return
	}

	// Register table.
	if err := repo.RegisterSyncedTable(h.repo.DB(), c.Table, def, c.MTime); err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("schema %s: register: %v", c.Table, err),
		})
		return
	}

	// Cache for this session.
	h.syncedTables[c.Table] = &SyncedTable{
		Info: repo.TableInfo{
			Name:  c.Table,
			Def:   def,
			MTime: c.MTime,
		},
		Def: def,
	}
}

// handlePragmaXTableHash compares catalog hashes and emits xigots if they differ.
func (h *handler) handlePragmaXTableHash(table, clientHash string) {
	st, ok := h.syncedTables[table]
	if !ok {
		// Table not registered yet — client will send schema card.
		return
	}

	localHash, err := repo.CatalogHash(h.repo.DB(), table, st.Def)
	if err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("xtable-hash %s: %v", table, err),
		})
		return
	}

	// BUGGIFY: 3% chance corrupt catalog hash to force full xigot exchange.
	if h.buggify != nil && h.buggify.Check("handler.catalogHash.corrupt", 0.03) {
		localHash = "buggify-corrupt-hash"
	}

	if localHash == clientHash {
		return // already in sync
	}

	// Emit xigot for all rows.
	if err := h.emitXIGotsForTable(table, st); err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("xtable-hash %s: emit xigots: %v", table, err),
		})
	}
}

// handleXIGot processes a table sync igot card.
func (h *handler) handleXIGot(c *xfer.XIGotCard) error {
	if c == nil {
		panic("handler.handleXIGot: c must not be nil")
	}
	if !h.pullOK {
		return nil
	}

	// BUGGIFY: 5% chance ignore xigot to test multi-round convergence.
	if h.buggify != nil && h.buggify.Check("handler.handleXIGot.skip", 0.05) {
		return nil
	}

	st, ok := h.syncedTables[c.Table]
	if !ok {
		// Table not registered — skip silently (client may have newer schema).
		return nil
	}

	// Lookup local row.
	localRow, localMtime, err := repo.LookupXRow(h.repo.DB(), c.Table, st.Def, c.PKHash)
	if err != nil {
		return fmt.Errorf("handler.handleXIGot: lookup %s/%s: %w", c.Table, c.PKHash, err)
	}

	if localRow == nil {
		// Missing locally — request it.
		h.resp = append(h.resp, &xfer.XGimmeCard{
			Table:  c.Table,
			PKHash: c.PKHash,
		})
		return nil
	}

	// Compare mtimes: if local is newer, push it (or send xdelete if tombstone).
	if localMtime > c.MTime {
		if repo.IsTombstone(st.Def, localRow) {
			// BUGGIFY: 5% chance skip xdelete to test multi-round convergence.
			if h.buggify != nil && h.buggify.Check("handler.handleXIGot.skipXDelete", 0.05) {
				return nil
			}
			h.sendXDelete(c.Table, st, c.PKHash, localMtime, localRow)
			return nil
		}
		return h.sendXRow(c.Table, st, c.PKHash)
	}

	return nil
}

// handleXGimme processes a table sync gimme card.
func (h *handler) handleXGimme(c *xfer.XGimmeCard) error {
	if c == nil {
		panic("handler.handleXGimme: c must not be nil")
	}

	// BUGGIFY: 5% chance skip sending a row to test client retry.
	if h.buggify != nil && h.buggify.Check("handler.handleXGimme.skip", 0.05) {
		return nil
	}

	st, ok := h.syncedTables[c.Table]
	if !ok {
		// Table not registered — skip silently.
		return nil
	}

	return h.sendXRow(c.Table, st, c.PKHash)
}

// handleXRow processes a table sync row card.
func (h *handler) handleXRow(c *xfer.XRowCard) error {
	if c == nil {
		panic("handler.handleXRow: c must not be nil")
	}
	if !h.pushOK {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("xrow %s/%s rejected: no push card", c.Table, c.PKHash),
		})
		return nil
	}

	st, ok := h.syncedTables[c.Table]
	if !ok {
		// Table not registered — skip silently (client may have newer schema).
		return nil
	}

	// BUGGIFY: 3% chance reject a valid row to test client re-push.
	if h.buggify != nil && h.buggify.Check("handler.handleXRow.reject", 0.03) {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("buggify: rejected xrow %s/%s", c.Table, c.PKHash),
		})
		return nil
	}

	// BUGGIFY: 2% chance corrupt JSON payload to test unmarshal error path.
	if h.buggify != nil && h.buggify.Check("handler.handleXRow.corruptJSON", 0.02) && len(c.Content) > 0 {
		corrupted := make([]byte, len(c.Content))
		copy(corrupted, c.Content)
		corrupted[0] = '!'
		c = &xfer.XRowCard{Table: c.Table, PKHash: c.PKHash, MTime: c.MTime, Content: corrupted}
	}

	// Unmarshal and verify PK hash.
	row, ok := h.verifyXRowPKHash(c, st)
	if !ok {
		return nil // error card already appended
	}

	// Conflict resolution — decide whether to accept the incoming row.
	accept, err := h.resolveXRowConflict(c, st, row)
	if err != nil {
		return err
	}
	if !accept {
		return nil
	}

	// Upsert row.
	if err := repo.UpsertXRow(h.repo.DB(), c.Table, row, c.MTime); err != nil {
		return fmt.Errorf("handler.handleXRow: upsert %s/%s: %w", c.Table, c.PKHash, err)
	}

	h.xrowsRecvd++
	return nil
}

// verifyXRowPKHash unmarshals the row JSON and verifies the PK hash matches.
// Returns the row map and true on success, or nil and false if an error card was emitted.
// UseNumber is required to preserve large integers (>2^53) that float64 cannot represent exactly.
func (h *handler) verifyXRowPKHash(c *xfer.XRowCard, st *SyncedTable) (map[string]any, bool) {
	if c == nil {
		panic("handler.verifyXRowPKHash: c must not be nil")
	}
	if st == nil {
		panic("handler.verifyXRowPKHash: st must not be nil")
	}
	var row map[string]any
	dec := json.NewDecoder(bytesReader(c.Content))
	dec.UseNumber()
	if err := dec.Decode(&row); err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("xrow %s/%s: unmarshal: %v", c.Table, c.PKHash, err),
		})
		return nil, false
	}
	// Coerce json.Number values to native Go types based on column definitions.
	coerceJSONNumbers(row, st.Def)

	pkCols := extractPKColumns(st.Def)
	pkColDefs := extractPKColumnDefs(st.Def)
	pkValues := make(map[string]any)
	for _, col := range pkCols {
		pkValues[col] = row[col]
	}
	computedPK := repo.PKHash(pkColDefs, pkValues)
	if computedPK != c.PKHash {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("xrow %s/%s: pk hash mismatch", c.Table, c.PKHash),
		})
		return nil, false
	}

	return row, true
}

// resolveXRowConflict applies the table's conflict resolution policy.
// Returns true if the incoming row should be accepted, false to reject.
func (h *handler) resolveXRowConflict(c *xfer.XRowCard, st *SyncedTable, row map[string]any) (bool, error) {
	if c == nil {
		panic("handler.resolveXRowConflict: c must not be nil")
	}
	if st == nil {
		panic("handler.resolveXRowConflict: st must not be nil")
	}
	if row == nil {
		panic("handler.resolveXRowConflict: row must not be nil")
	}
	localRow, localMtime, err := repo.LookupXRow(h.repo.DB(), c.Table, st.Def, c.PKHash)
	if err != nil {
		return false, fmt.Errorf("handler.resolveXRowConflict: lookup %s/%s: %w", c.Table, c.PKHash, err)
	}

	switch st.Def.Conflict {
	case "mtime-wins":
		// Tie goes to the incoming row to ensure convergence — if both sides
		// have equal mtime, accepting the row is idempotent.
		if localRow != nil && localMtime > c.MTime {
			return false, nil
		}
	case "self-write":
		// Self-write allows a peer to modify only rows it originally created.
		// The _owner field is set on first write and immutable thereafter.
		if localRow != nil {
			localOwner, _ := localRow["_owner"].(string)
			if localOwner != "" && localOwner != h.user {
				return false, nil
			}
		}
		if h.user != "" {
			row["_owner"] = h.user
		}
	case "owner-write":
		// Owner-write is like self-write but the owner is the loginUser,
		// not the PK. Only the original writer can update.
		if localRow != nil {
			localOwner, _ := localRow["_owner"].(string)
			if localOwner != h.user {
				return false, nil
			}
		}
		if h.user != "" {
			row["_owner"] = h.user
		}
	}

	return true, nil
}

// sendXRow sends a single row to the client.
func (h *handler) sendXRow(table string, st *SyncedTable, pkHash string) error {
	if st == nil {
		panic("handler.sendXRow: st must not be nil")
	}
	if pkHash == "" {
		panic("handler.sendXRow: pkHash must not be empty")
	}
	row, mtime, err := repo.LookupXRow(h.repo.DB(), table, st.Def, pkHash)
	if err != nil {
		return fmt.Errorf("handler.sendXRow: lookup %s/%s: %w", table, pkHash, err)
	}
	if row == nil {
		return nil // Row not found — skip silently.
	}

	// Marshal row data.
	rowJSON, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("handler.sendXRow: marshal %s/%s: %w", table, pkHash, err)
	}

	h.resp = append(h.resp, &xfer.XRowCard{
		Table:   table,
		PKHash:  pkHash,
		MTime:   mtime,
		Content: rowJSON,
	})
	h.xrowsSent++
	return nil
}

// emitXIGots emits schema and xigot cards for all synced tables.
// Schema cards are sent so that pulling clients learn about new tables.
func (h *handler) emitXIGots() error {
	if h.syncedTables == nil {
		panic("handler.emitXIGots: syncedTables must not be nil")
	}
	for name, st := range h.syncedTables {
		// BUGGIFY: 5% chance skip schema card to test client retry on missing schema.
		if h.buggify != nil && h.buggify.Check("handler.emitXIGots.dropSchema", 0.05) {
			// Skip schema card — only emit xigots.
		} else {
			// Emit schema card so client can register the table if missing.
			defJSON, err := json.Marshal(st.Def)
			if err != nil {
				return fmt.Errorf("handler.emitXIGots: marshal def %s: %w", name, err)
			}
			schemaHash := hash.SHA1(defJSON)
			h.resp = append(h.resp, &xfer.SchemaCard{
				Table:   name,
				Version: 1,
				Hash:    schemaHash,
				MTime:   st.Info.MTime,
				Content: defJSON,
			})
		}

		if err := h.emitXIGotsForTable(name, st); err != nil {
			return err
		}
	}
	return nil
}

// emitXIGotsForTable emits xigot cards for all rows in a table.
func (h *handler) emitXIGotsForTable(table string, st *SyncedTable) error {
	if st == nil {
		panic("handler.emitXIGotsForTable: st must not be nil")
	}
	if table == "" {
		panic("handler.emitXIGotsForTable: table must not be empty")
	}
	rows, mtimes, err := repo.ListXRows(h.repo.DB(), table, st.Def)
	if err != nil {
		return fmt.Errorf("handler.emitXIGotsForTable: list %s: %w", table, err)
	}

	// BUGGIFY: 10% chance truncate xigot list to stress multi-round convergence.
	if h.buggify != nil && h.buggify.Check("handler.emitXIGots.truncate", 0.10) && len(rows) > 1 {
		rows = rows[:len(rows)/2]
		mtimes = mtimes[:len(mtimes)/2]
	}

	pkCols := extractPKColumns(st.Def)
	pkColDefs := extractPKColumnDefs(st.Def)

	for i, row := range rows {
		pkValues := make(map[string]any)
		for _, col := range pkCols {
			pkValues[col] = row[col]
		}
		pkHash := repo.PKHash(pkColDefs, pkValues)

		h.resp = append(h.resp, &xfer.XIGotCard{
			Table:  table,
			PKHash: pkHash,
			MTime:  mtimes[i],
		})
	}
	return nil
}

// handleXDelete processes a table sync deletion card.
func (h *handler) handleXDelete(c *xfer.XDeleteCard) error {
	if c == nil {
		panic("handler.handleXDelete: c must not be nil")
	}
	if !h.pushOK {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("xdelete %s/%s rejected: no push card", c.Table, c.PKHash),
		})
		return nil
	}

	st, ok := h.syncedTables[c.Table]
	if !ok {
		// Table not registered locally — skip silently. The client may have
		// a newer schema version that includes this table; we'll learn about
		// it when the schema card arrives in a future round.
		return nil
	}

	// BUGGIFY: 3% chance reject a valid xdelete to test client re-push.
	if h.buggify != nil && h.buggify.Check("handler.handleXDelete.reject", 0.03) {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("buggify: rejected xdelete %s/%s", c.Table, c.PKHash),
		})
		return nil
	}

	// Conflict resolution: check ownership for self-write/owner-write tables.
	if st.Def.Conflict == "self-write" || st.Def.Conflict == "owner-write" {
		localOwner, err := repo.LookupXRowOwner(h.repo.DB(), c.Table, st.Def, c.PKHash)
		if err != nil {
			return fmt.Errorf("handler.handleXDelete: lookup owner %s/%s: %w", c.Table, c.PKHash, err)
		}
		if localOwner != "" && localOwner != h.user {
			// Not the owner — reject deletion silently.
			return nil
		}
	}

	return applyXDeleteLocally(h.repo.DB(), c.Table, st.Def, c.PKHash, c.MTime, c.PKData)
}

// sendXDelete sends a tombstone deletion card to the client.
func (h *handler) sendXDelete(table string, st *SyncedTable, pkHash string, mtime int64, row map[string]any) {
	if st == nil {
		panic("handler.sendXDelete: st must not be nil")
	}
	if pkHash == "" {
		panic("handler.sendXDelete: pkHash must not be empty")
	}
	if row == nil {
		panic("handler.sendXDelete: row must not be nil")
	}
	pkCols := extractPKColumns(st.Def)
	pkValues := make(map[string]any)
	for _, col := range pkCols {
		pkValues[col] = row[col]
	}
	pkData, err := json.Marshal(pkValues)
	if err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("xdelete %s/%s: marshal PKData: %v", table, pkHash, err),
		})
		return
	}

	// BUGGIFY: 2% chance corrupt PKData to test receiver error handling.
	if h.buggify != nil && h.buggify.Check("handler.sendXDelete.corruptPKData", 0.02) && len(pkData) > 0 {
		corrupted := make([]byte, len(pkData))
		copy(corrupted, pkData)
		corrupted[0] = '!'
		pkData = corrupted
	}

	h.resp = append(h.resp, &xfer.XDeleteCard{
		Table:  table,
		PKHash: pkHash,
		MTime:  mtime,
		PKData: pkData,
	})
}

// extractPKColumns is defined in client_tablesync.go as a package-level
// function shared by both client and handler code.

// bytesReader wraps a byte slice in a bytes.Reader for use with json.NewDecoder.
func bytesReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}

// coerceJSONNumbers converts json.Number values in a row map to native Go types
// (int64 for "integer" columns, float64 for "real" columns) based on the
// declared column definitions. This is required after UseNumber decoding to
// preserve large integers (>2^53) that float64 cannot represent exactly.
// It modifies row in place.
func coerceJSONNumbers(row map[string]any, def repo.TableDef) {
	if row == nil {
		panic("coerceJSONNumbers: row must not be nil")
	}
	for _, col := range def.Columns {
		v, ok := row[col.Name]
		if !ok {
			continue
		}
		n, ok := v.(json.Number)
		if !ok {
			continue
		}
		switch col.Type {
		case "integer":
			if i, err := n.Int64(); err == nil {
				row[col.Name] = i
			}
		case "real":
			if f, err := n.Float64(); err == nil {
				row[col.Name] = f
			}
		default:
			// text, blob: leave as string representation of the number
			row[col.Name] = n.String()
		}
	}
}
