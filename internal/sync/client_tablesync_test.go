package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/xfer"
)

// newTableSyncRepoPair creates server and client repos with sync schema initialized.
func newTableSyncRepoPair(t *testing.T) (server, client *repo.Repo) {
	t.Helper()
	dir := t.TempDir()

	sPath := filepath.Join(dir, "server.fossil")
	s, err := repo.Create(sPath, "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("server repo: %v", err)
	}
	repo.EnsureSyncSchema(s.DB())

	cPath := filepath.Join(dir, "client.fossil")
	c, err := repo.Create(cPath, "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("client repo: %v", err)
	}
	repo.EnsureSyncSchema(c.DB())

	t.Cleanup(func() {
		s.Close()
		c.Close()
	})
	return s, c
}

func TestTableSyncEndToEnd(t *testing.T) {
	serverRepo, clientRepo := newTableSyncRepoPair(t)

	// Define and register same table on both sides.
	def := repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "id", Type: "text", PK: true},
			{Name: "value", Type: "text"},
		},
		Conflict: "mtime-wins",
	}
	if err := repo.RegisterSyncedTable(serverRepo.DB(), "tasks", def, 100); err != nil {
		t.Fatalf("register server: %v", err)
	}
	if err := repo.RegisterSyncedTable(clientRepo.DB(), "tasks", def, 100); err != nil {
		t.Fatalf("register client: %v", err)
	}

	// Insert different rows on each side.
	if err := repo.UpsertXRow(serverRepo.DB(), "tasks", map[string]any{
		"id": "srv-1", "value": "from server",
	}, 200); err != nil {
		t.Fatalf("upsert server row: %v", err)
	}
	if err := repo.UpsertXRow(clientRepo.DB(), "tasks", map[string]any{
		"id": "cli-1", "value": "from client",
	}, 300); err != nil {
		t.Fatalf("upsert client row: %v", err)
	}

	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), serverRepo, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		return resp
	}}

	_, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true, Push: true, XTableSync: true,
		ProjectCode: "p", ServerCode: "s",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Verify both sides have both rows.
	// Client should have srv-1.
	clientRows, _, err := repo.ListXRows(clientRepo.DB(), "tasks", def)
	if err != nil {
		t.Fatalf("ListXRows client: %v", err)
	}
	if len(clientRows) != 2 {
		t.Fatalf("client rows = %d, want 2", len(clientRows))
	}

	// Server should have cli-1.
	serverRows, _, err := repo.ListXRows(serverRepo.DB(), "tasks", def)
	if err != nil {
		t.Fatalf("ListXRows server: %v", err)
	}
	if len(serverRows) != 2 {
		t.Fatalf("server rows = %d, want 2", len(serverRows))
	}

	// Check specific values.
	foundServerRow := false
	foundClientRow := false
	for _, row := range clientRows {
		id, _ := row["id"].(string)
		val, _ := row["value"].(string)
		if id == "srv-1" && val == "from server" {
			foundServerRow = true
		}
		if id == "cli-1" && val == "from client" {
			foundClientRow = true
		}
	}
	if !foundServerRow {
		t.Error("client missing server row (srv-1)")
	}
	if !foundClientRow {
		t.Error("client missing own row (cli-1)")
	}
}

func TestTableSyncDeletion(t *testing.T) {
	serverRepo, clientRepo := newTableSyncRepoPair(t)

	def := repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "id", Type: "text", PK: true},
			{Name: "value", Type: "text"},
		},
		Conflict: "mtime-wins",
	}
	if err := repo.RegisterSyncedTable(serverRepo.DB(), "tasks", def, 100); err != nil {
		t.Fatalf("register server: %v", err)
	}
	if err := repo.RegisterSyncedTable(clientRepo.DB(), "tasks", def, 100); err != nil {
		t.Fatalf("register client: %v", err)
	}

	// Seed same row on both sides at mtime 1000.
	row := map[string]any{"id": "row-1", "value": "hello"}
	if err := repo.UpsertXRow(serverRepo.DB(), "tasks", row, 1000); err != nil {
		t.Fatalf("upsert server row: %v", err)
	}
	if err := repo.UpsertXRow(clientRepo.DB(), "tasks", row, 1000); err != nil {
		t.Fatalf("upsert client row: %v", err)
	}

	// Delete on server at mtime 2000.
	pkColDefs := []repo.ColumnDef{{Name: "id", Type: "text", PK: true}}
	pkHash := repo.PKHash(pkColDefs, map[string]any{"id": "row-1"})
	deleted, err := repo.DeleteXRowByPKHash(serverRepo.DB(), "tasks", def, pkHash, 2000)
	if err != nil {
		t.Fatalf("DeleteXRowByPKHash: %v", err)
	}
	if !deleted {
		t.Fatal("expected row to be deleted on server")
	}

	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), serverRepo, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		return resp
	}}

	_, err = Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true, Push: true, XTableSync: true,
		ProjectCode: "p", ServerCode: "s",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Verify client's row is now a tombstone with mtime 2000.
	clientRow, clientMtime, err := repo.LookupXRow(clientRepo.DB(), "tasks", def, pkHash)
	if err != nil {
		t.Fatalf("LookupXRow client: %v", err)
	}
	if clientRow == nil {
		t.Fatal("client row not found after sync")
	}
	if !repo.IsTombstone(def, clientRow) {
		t.Errorf("client row is not a tombstone: %v", clientRow)
	}
	if clientMtime != 2000 {
		t.Errorf("client mtime = %d, want 2000", clientMtime)
	}
}

func TestTableSyncSchemaDeployment(t *testing.T) {
	serverRepo, clientRepo := newTableSyncRepoPair(t)

	// Register table ONLY on server.
	def := repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "key", Type: "text", PK: true},
			{Name: "data", Type: "text"},
		},
		Conflict: "mtime-wins",
	}
	if err := repo.RegisterSyncedTable(serverRepo.DB(), "settings", def, 100); err != nil {
		t.Fatalf("register server: %v", err)
	}

	// Insert a row on server.
	if err := repo.UpsertXRow(serverRepo.DB(), "settings", map[string]any{
		"key": "theme", "data": "dark",
	}, 200); err != nil {
		t.Fatalf("upsert server row: %v", err)
	}

	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), serverRepo, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		return resp
	}}

	_, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true, Push: true, XTableSync: true,
		ProjectCode: "p", ServerCode: "s",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Client should have the table created and the row.
	clientTables, err := repo.ListSyncedTables(clientRepo.DB())
	if err != nil {
		t.Fatalf("ListSyncedTables client: %v", err)
	}
	if len(clientTables) != 1 {
		t.Fatalf("client tables = %d, want 1", len(clientTables))
	}
	if clientTables[0].Name != "settings" {
		t.Fatalf("client table name = %q, want settings", clientTables[0].Name)
	}

	clientRows, _, err := repo.ListXRows(clientRepo.DB(), "settings", def)
	if err != nil {
		t.Fatalf("ListXRows client: %v", err)
	}
	if len(clientRows) != 1 {
		t.Fatalf("client rows = %d, want 1", len(clientRows))
	}
	if clientRows[0]["key"] != "theme" || clientRows[0]["data"] != "dark" {
		t.Errorf("client row = %v, want key=theme data=dark", clientRows[0])
	}
}
