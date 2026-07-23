package sync

import (
	"encoding/json"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// buildXTableCards emits schema, pragma xtable-hash, xigot, xgimme, and xrow
// cards for all registered synced tables.
func (s *session) buildXTableCards() ([]xfer.Card, error) {
	if err := repo.EnsureSyncSchema(s.repo.DB()); err != nil {
		return nil, fmt.Errorf("buildXTableCards: ensure schema: %w", err)
	}

	tables, err := repo.ListSyncedTables(s.repo.DB())
	if err != nil {
		return nil, fmt.Errorf("buildXTableCards: list tables: %w", err)
	}

	var cards []xfer.Card
	for _, info := range tables {
		schemaCards, err := s.buildTableSchemaCard(info)
		if err != nil {
			return nil, err
		}
		cards = append(cards, schemaCards...)

		igotCards, err := s.buildTableXIGotCards(info)
		if err != nil {
			return nil, err
		}
		cards = append(cards, igotCards...)

		sendCards, err := s.buildTableSendCards(info)
		if err != nil {
			return nil, err
		}
		cards = append(cards, sendCards...)
	}

	return cards, nil
}

// buildTableSchemaCard builds the schema and pragma xtable-hash cards for one table.
func (s *session) buildTableSchemaCard(info repo.TableInfo) ([]xfer.Card, error) {
	defJSON, err := json.Marshal(info.Def)
	if err != nil {
		return nil, fmt.Errorf("buildTableSchemaCard: marshal def %s: %w", info.Name, err)
	}
	schemaHash := hash.SHA1(defJSON)

	catHash, err := repo.CatalogHash(s.repo.DB(), info.Name, info.Def)
	if err != nil {
		return nil, fmt.Errorf("buildTableSchemaCard: catalog hash %s: %w", info.Name, err)
	}

	return []xfer.Card{
		&xfer.SchemaCard{
			Table:   info.Name,
			Version: 1,
			Hash:    schemaHash,
			MTime:   info.MTime,
			Content: defJSON,
		},
		&xfer.PragmaCard{
			Name:   "xtable-hash",
			Values: []string{info.Name, catHash},
		},
	}, nil
}

// buildTableXIGotCards builds xigot cards for all rows in one table.
func (s *session) buildTableXIGotCards(info repo.TableInfo) ([]xfer.Card, error) {
	rows, mtimes, err := repo.ListXRows(s.repo.DB(), info.Name, info.Def)
	if err != nil {
		return nil, fmt.Errorf("buildTableXIGotCards: list rows %s: %w", info.Name, err)
	}

	pkCols := extractPKColumns(info.Def)
	pkColDefs := extractPKColumnDefs(info.Def)
	var cards []xfer.Card
	for i, row := range rows {
		pkValues := make(map[string]any)
		for _, col := range pkCols {
			pkValues[col] = row[col]
		}
		pkHash := repo.PKHash(pkColDefs, pkValues)
		mtime := mtimes[i]

		// BUGGIFY: 2% chance send stale mtime to test server-side comparison.
		if s.opts.Buggify != nil && s.opts.Buggify.Check("client.buildXIGot.staleMtime", 0.02) {
			mtime = 1 // Ancient mtime — server should still converge.
		}

		cards = append(cards, &xfer.XIGotCard{
			Table:  info.Name,
			PKHash: pkHash,
			MTime:  mtime,
		})
	}
	return cards, nil
}

// buildTableSendCards builds xgimme and xrow cards for one table.
func (s *session) buildTableSendCards(info repo.TableInfo) ([]xfer.Card, error) {
	var cards []xfer.Card

	// Emit xgimme for rows we're requesting.
	if gimmes, ok := s.xTableGimmes[info.Name]; ok {
		for pkHash := range gimmes {
			cards = append(cards, &xfer.XGimmeCard{
				Table:  info.Name,
				PKHash: pkHash,
			})
		}
	}

	// Emit xrow or xdelete for rows queued to send.
	if sends, ok := s.xTableToSend[info.Name]; ok {
		for pkHash := range sends {
			row, mtime, err := repo.LookupXRow(s.repo.DB(), info.Name, info.Def, pkHash)
			if err != nil {
				return nil, fmt.Errorf("buildTableSendCards: lookup %s/%s: %w", info.Name, pkHash, err)
			}
			if row == nil {
				delete(sends, pkHash)
				continue
			}
			if repo.IsTombstone(info.Def, row) {
				// BUGGIFY: 3% chance skip sending xdelete to test re-queue next round.
				if s.opts.Buggify != nil && s.opts.Buggify.Check("client.buildTableSendCards.skipXDelete", 0.03) {
					delete(sends, pkHash)
					continue
				}
				// Send xdelete with PK data.
				pkCols := extractPKColumns(info.Def)
				pkValues := make(map[string]any)
				for _, col := range pkCols {
					pkValues[col] = row[col]
				}
				pkData, err := json.Marshal(pkValues)
				if err != nil {
					return nil, fmt.Errorf("buildTableSendCards: marshal pk %s/%s: %w", info.Name, pkHash, err)
				}
				cards = append(cards, &xfer.XDeleteCard{
					Table:  info.Name,
					PKHash: pkHash,
					MTime:  mtime,
					PKData: pkData,
				})
			} else {
				rowJSON, err := json.Marshal(row)
				if err != nil {
					return nil, fmt.Errorf("buildTableSendCards: marshal row %s/%s: %w", info.Name, pkHash, err)
				}
				cards = append(cards, &xfer.XRowCard{
					Table:   info.Name,
					PKHash:  pkHash,
					MTime:   mtime,
					Content: rowJSON,
				})
			}
			delete(sends, pkHash)
		}
	}

	return cards, nil
}

// processXTableCard dispatches incoming table sync cards to their handlers.
func (s *session) processXTableCard(card xfer.Card) error {
	if card == nil {
		panic("session.processXTableCard: card must not be nil")
	}
	switch c := card.(type) {
	case *xfer.SchemaCard:
		return s.handleXSchemaCard(c)
	case *xfer.XIGotCard:
		return s.handleXIGotResponse(c)
	case *xfer.XGimmeCard:
		return s.handleXGimmeResponse(c)
	case *xfer.XRowCard:
		return s.handleXRowResponse(c)
	case *xfer.XDeleteCard:
		return s.handleXDeleteResponse(c)
	}
	return nil
}

// handleXSchemaCard registers a table locally when received from server.
func (s *session) handleXSchemaCard(c *xfer.SchemaCard) error {
	if c == nil {
		panic("session.handleXSchemaCard: c must not be nil")
	}

	var def repo.TableDef
	if err := json.Unmarshal(c.Content, &def); err != nil {
		return fmt.Errorf("handleXSchemaCard: unmarshal %s: %w", c.Table, err)
	}

	if err := repo.EnsureSyncSchema(s.repo.DB()); err != nil {
		return fmt.Errorf("handleXSchemaCard: ensure schema: %w", err)
	}

	if err := repo.RegisterSyncedTable(s.repo.DB(), c.Table, def, c.MTime); err != nil {
		return fmt.Errorf("handleXSchemaCard: register %s: %w", c.Table, err)
	}
	return nil
}

// handleXIGotResponse processes an xigot from the server response.
// If missing locally, add to gimmes. If local is newer, add to sends.
func (s *session) handleXIGotResponse(c *xfer.XIGotCard) error {
	if c == nil {
		panic("session.handleXIGotResponse: c must not be nil")
	}

	if err := repo.EnsureSyncSchema(s.repo.DB()); err != nil {
		return fmt.Errorf("handleXIGotResponse: ensure schema: %w", err)
	}

	def, err := s.getXTableDef(c.Table)
	if err != nil {
		return fmt.Errorf("handleXIGotResponse: get table def %s: %w", c.Table, err)
	}
	if def == nil {
		// Table not registered locally — skip (schema card should come first).
		return nil
	}

	localRow, localMtime, err := repo.LookupXRow(s.repo.DB(), c.Table, *def, c.PKHash)
	if err != nil {
		return fmt.Errorf("handleXIGotResponse: lookup %s/%s: %w", c.Table, c.PKHash, err)
	}

	if localRow == nil {
		// Missing locally — request it.
		if s.xTableGimmes[c.Table] == nil {
			s.xTableGimmes[c.Table] = make(map[string]bool)
		}
		s.xTableGimmes[c.Table][c.PKHash] = true
		return nil
	}

	// Local is newer — queue for sending.
	if localMtime > c.MTime {
		if s.xTableToSend[c.Table] == nil {
			s.xTableToSend[c.Table] = make(map[string]bool)
		}
		s.xTableToSend[c.Table][c.PKHash] = true
	}

	return nil
}

// handleXGimmeResponse processes an xgimme from the server response.
func (s *session) handleXGimmeResponse(c *xfer.XGimmeCard) error {
	if c == nil {
		panic("session.handleXGimmeResponse: c must not be nil")
	}
	if s.xTableToSend[c.Table] == nil {
		s.xTableToSend[c.Table] = make(map[string]bool)
	}
	s.xTableToSend[c.Table][c.PKHash] = true
	return nil
}

// handleXRowResponse processes an xrow from the server response.
func (s *session) handleXRowResponse(c *xfer.XRowCard) error {
	if c == nil {
		panic("session.handleXRowResponse: c must not be nil")
	}

	// BUGGIFY: 5% chance drop received row to test re-gimme next round.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("client.handleXRowResponse.drop", 0.05) {
		return nil
	}

	if err := repo.EnsureSyncSchema(s.repo.DB()); err != nil {
		return fmt.Errorf("handleXRowResponse: ensure schema: %w", err)
	}

	def, err := s.getXTableDef(c.Table)
	if err != nil {
		return fmt.Errorf("handleXRowResponse: get table def %s: %w", c.Table, err)
	}

	var row map[string]any
	rowDec := json.NewDecoder(bytesReader(c.Content))
	rowDec.UseNumber()
	if err := rowDec.Decode(&row); err != nil {
		return fmt.Errorf("handleXRowResponse: unmarshal %s/%s: %w", c.Table, c.PKHash, err)
	}
	if def != nil {
		coerceJSONNumbers(row, *def)
	}

	if err := repo.UpsertXRow(s.repo.DB(), c.Table, row, c.MTime); err != nil {
		return fmt.Errorf("handleXRowResponse: upsert %s/%s: %w", c.Table, c.PKHash, err)
	}

	// Remove from gimmes.
	if gimmes, ok := s.xTableGimmes[c.Table]; ok {
		delete(gimmes, c.PKHash)
	}

	return nil
}

// handleXDeleteResponse processes an xdelete from the server response.
func (s *session) handleXDeleteResponse(c *xfer.XDeleteCard) error {
	if c == nil {
		panic("session.handleXDeleteResponse: c must not be nil")
	}

	// BUGGIFY: 5% chance drop received xdelete to test re-send next round.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("client.handleXDeleteResponse.drop", 0.05) {
		return nil
	}

	if err := repo.EnsureSyncSchema(s.repo.DB()); err != nil {
		return fmt.Errorf("handleXDeleteResponse: ensure schema: %w", err)
	}

	def, err := s.getXTableDef(c.Table)
	if err != nil {
		return fmt.Errorf("handleXDeleteResponse: get table def %s: %w", c.Table, err)
	}
	if def == nil {
		return nil
	}

	if err := applyXDeleteLocally(s.repo.DB(), c.Table, *def, c.PKHash, c.MTime, c.PKData); err != nil {
		return err
	}

	// Remove from gimmes if present.
	if gimmes, ok := s.xTableGimmes[c.Table]; ok {
		delete(gimmes, c.PKHash)
	}

	return nil
}

// extractPKColumns returns the names of all primary key columns from a TableDef.
// This is a package-level function shared by both client and handler.
func extractPKColumns(def repo.TableDef) []string {
	if len(def.Columns) == 0 {
		panic("extractPKColumns: def.Columns must not be empty")
	}
	var pkCols []string
	for _, col := range def.Columns {
		if col.PK {
			pkCols = append(pkCols, col.Name)
		}
	}
	return pkCols
}

// extractPKColumnDefs returns the ColumnDef of all PK columns from a TableDef.
func extractPKColumnDefs(def repo.TableDef) []repo.ColumnDef {
	if len(def.Columns) == 0 {
		panic("extractPKColumnDefs: def.Columns must not be empty")
	}
	var pkCols []repo.ColumnDef
	for _, col := range def.Columns {
		if col.PK {
			pkCols = append(pkCols, col)
		}
	}
	return pkCols
}
