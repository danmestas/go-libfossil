package sync

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestHandleSchemaCard(t *testing.T) {
	r := setupSyncTestRepo(t)
	def := repo.TableDef{
		Columns:  []repo.ColumnDef{{Name: "peer_id", Type: "text", PK: true}},
		Conflict: "mtime-wins",
	}
	defJSON, _ := json.Marshal(def)
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.SchemaCard{Table: "test_table", Version: 1, Hash: "abc", MTime: 100, Content: defJSON},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	_ = resp
	var count int
	if err := r.DB().QueryRow("SELECT count(*) FROM x_test_table").Scan(&count); err != nil {
		t.Fatalf("x_test_table should exist: %v", err)
	}
}

func TestHandleXIGotXGimmeXRow(t *testing.T) {
	r := setupSyncTestRepo(t)
	def := repo.TableDef{
		Columns:  []repo.ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "val", Type: "text"}},
		Conflict: "mtime-wins",
	}
	repo.EnsureSyncSchema(r.DB())
	repo.RegisterSyncedTable(r.DB(), "kv", def, 100)
	repo.UpsertXRow(r.DB(), "kv", map[string]any{"id": "local-key", "val": "local-val"}, 200)
	pkColDefs := []repo.ColumnDef{{Name: "id", Type: "text", PK: true}}
	localPK := repo.PKHash(pkColDefs, map[string]any{"id": "local-key"})
	remotePK := repo.PKHash(pkColDefs, map[string]any{"id": "remote-key"})
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.XIGotCard{Table: "kv", PKHash: remotePK, MTime: 300},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	gimmes := findCards[*xfer.XGimmeCard](resp)
	if len(gimmes) == 0 {
		t.Fatal("expected xgimme for missing remote row")
	}
	if gimmes[0].PKHash != remotePK {
		t.Fatalf("xgimme pk = %q, want %q", gimmes[0].PKHash, remotePK)
	}
	igots := findCards[*xfer.XIGotCard](resp)
	found := false
	for _, ig := range igots {
		if ig.PKHash == localPK {
			found = true
		}
	}
	if !found {
		t.Fatal("expected xigot for local row")
	}
}

func TestHandleXDelete(t *testing.T) {
	r := setupSyncTestRepo(t)
	def := repo.TableDef{
		Columns:  []repo.ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "val", Type: "text"}},
		Conflict: "mtime-wins",
	}
	repo.EnsureSyncSchema(r.DB())
	repo.RegisterSyncedTable(r.DB(), "kv", def, 100)
	// Seed a live row at mtime 1000.
	if err := repo.UpsertXRow(r.DB(), "kv", map[string]any{"id": "row1", "val": "hello"}, 1000); err != nil {
		t.Fatalf("UpsertXRow: %v", err)
	}

	pkColDefs := []repo.ColumnDef{{Name: "id", Type: "text", PK: true}}
	pkHash := repo.PKHash(pkColDefs, map[string]any{"id": "row1"})

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.XDeleteCard{
			Table:  "kv",
			PKHash: pkHash,
			MTime:  2000,
			PKData: []byte(`{"id":"row1"}`),
		},
	}}
	_, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// Verify the row is now a tombstone with mtime 2000.
	row, mtime, err := repo.LookupXRow(r.DB(), "kv", def, pkHash)
	if err != nil {
		t.Fatalf("LookupXRow: %v", err)
	}
	if row == nil {
		t.Fatal("expected tombstone row, got nil")
	}
	if !repo.IsTombstone(def, row) {
		t.Fatalf("expected tombstone, got row with val=%v", row["val"])
	}
	if mtime != 2000 {
		t.Fatalf("expected mtime=2000, got %d", mtime)
	}
}

func TestHandleXIGotEmitsXDeleteForTombstone(t *testing.T) {
	r := setupSyncTestRepo(t)
	def := repo.TableDef{
		Columns:  []repo.ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "val", Type: "text"}},
		Conflict: "mtime-wins",
	}
	repo.EnsureSyncSchema(r.DB())
	repo.RegisterSyncedTable(r.DB(), "kv", def, 100)
	// Seed a live row, then tombstone it at mtime 2000.
	if err := repo.UpsertXRow(r.DB(), "kv", map[string]any{"id": "row1", "val": "hello"}, 1000); err != nil {
		t.Fatalf("UpsertXRow: %v", err)
	}
	pkColDefs := []repo.ColumnDef{{Name: "id", Type: "text", PK: true}}
	pkHash := repo.PKHash(pkColDefs, map[string]any{"id": "row1"})
	if _, err := repo.DeleteXRowByPKHash(r.DB(), "kv", def, pkHash, 2000); err != nil {
		t.Fatalf("DeleteXRowByPKHash: %v", err)
	}

	// Client claims it has the row at old mtime 1000.
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.XIGotCard{Table: "kv", PKHash: pkHash, MTime: 1000},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// Response must contain an XDeleteCard (not an XRowCard) for this pkHash.
	xdeletes := findCards[*xfer.XDeleteCard](resp)
	if len(xdeletes) == 0 {
		t.Fatal("expected xdelete card in response for tombstoned row")
	}
	found := false
	for _, xd := range xdeletes {
		if xd.Table == "kv" && xd.PKHash == pkHash {
			found = true
			if xd.MTime != 2000 {
				t.Errorf("xdelete mtime = %d, want 2000", xd.MTime)
			}
		}
	}
	if !found {
		t.Fatalf("expected xdelete for pkHash %q, got: %+v", pkHash, xdeletes)
	}
	// Must NOT contain an XRowCard for the same pkHash.
	xrows := findCards[*xfer.XRowCard](resp)
	for _, xr := range xrows {
		if xr.Table == "kv" && xr.PKHash == pkHash {
			t.Fatalf("unexpected xrow card for tombstoned row %q", pkHash)
		}
	}
}

func TestHandleXDelete_SelfWriteOwnershipCheck(t *testing.T) {
	serverRepo := setupSyncTestRepo(t)
	pc := testProjectCode(t, serverRepo.DB())

	// Create users alice (owner) and bob (non-owner) with push+pull caps.
	if err := auth.CreateUser(serverRepo.DB(), pc, "alice", "alicesecret", "oi"); err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if err := auth.CreateUser(serverRepo.DB(), pc, "bob", "bobsecret", "oi"); err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	def := repo.TableDef{
		Columns:  []repo.ColumnDef{{Name: "peer_id", Type: "text", PK: true}, {Name: "status", Type: "text"}},
		Conflict: "self-write",
	}
	repo.EnsureSyncSchema(serverRepo.DB())
	repo.RegisterSyncedTable(serverRepo.DB(), "peers", def, 1000)

	// Insert row owned by "alice".
	repo.UpsertXRow(serverRepo.DB(), "peers", map[string]any{
		"peer_id": "alice", "status": "online", "_owner": "alice",
	}, 1000)

	pkColDefs := []repo.ColumnDef{{Name: "peer_id", Type: "text", PK: true}}
	pkHash := repo.PKHash(pkColDefs, map[string]any{"peer_id": "alice"})

	// "bob" tries to delete alice's row — should be rejected.
	aliceLoginCard := buildTestLoginCard("alice", "alicesecret", pc, []byte("dummy"))
	_ = aliceLoginCard // ensure we build both
	bobLoginCard := buildTestLoginCard("bob", "bobsecret", pc, []byte("dummy"))

	msg := &xfer.Message{
		Cards: []xfer.Card{
			bobLoginCard,
			&xfer.PushCard{ServerCode: "sc", ProjectCode: "pc"},
			&xfer.PullCard{ServerCode: "sc", ProjectCode: "pc"},
			&xfer.XDeleteCard{
				Table:  "peers",
				PKHash: pkHash,
				MTime:  2000,
				PKData: []byte(`{"peer_id":"alice"}`),
			},
		},
	}

	_, err := HandleSync(context.Background(), serverRepo, msg)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// Row should still be alive — deletion rejected.
	row, mtime, _ := repo.LookupXRow(serverRepo.DB(), "peers", def, pkHash)
	if row == nil {
		t.Fatal("row should still exist")
	}
	if repo.IsTombstone(def, row) {
		t.Error("row should NOT be tombstone — bob is not the owner")
	}
	if mtime != 1000 {
		t.Errorf("mtime should be unchanged: got %d, want 1000", mtime)
	}
}

func TestHandleXDelete_PKDataHashMismatch(t *testing.T) {
	serverRepo := setupSyncTestRepo(t)
	def := repo.TableDef{
		Columns:  []repo.ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "data", Type: "text"}},
		Conflict: "mtime-wins",
	}
	repo.EnsureSyncSchema(serverRepo.DB())
	repo.RegisterSyncedTable(serverRepo.DB(), "test_tbl", def, 1000)

	// Send xdelete with PKHash for "k1" but PKData for "INJECTED".
	pkColDefs := []repo.ColumnDef{{Name: "id", Type: "text", PK: true}}
	realHash := repo.PKHash(pkColDefs, map[string]any{"id": "k1"})

	msg := &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PushCard{ServerCode: "sc", ProjectCode: "pc"},
			&xfer.PullCard{ServerCode: "sc", ProjectCode: "pc"},
			&xfer.XDeleteCard{
				Table:  "test_tbl",
				PKHash: realHash,
				MTime:  2000,
				PKData: []byte(`{"id":"INJECTED"}`), // Mismatched!
			},
		},
	}

	// applyXDeleteLocally returns an error on hash mismatch — HandleSync propagates it.
	_, err := HandleSync(context.Background(), serverRepo, msg)
	if err == nil {
		t.Fatal("expected error for PKData hash mismatch, got nil")
	}

	// Should NOT have inserted a tombstone for "INJECTED".
	injectedHash := repo.PKHash(pkColDefs, map[string]any{"id": "INJECTED"})
	row, _, _ := repo.LookupXRow(serverRepo.DB(), "test_tbl", def, injectedHash)
	if row != nil {
		t.Error("should NOT have inserted tombstone for mismatched PKData")
	}

	// Should NOT have inserted a tombstone for "k1" either.
	row2, _, _ := repo.LookupXRow(serverRepo.DB(), "test_tbl", def, realHash)
	if row2 != nil {
		t.Error("should NOT have inserted any tombstone for mismatched PKData")
	}
}
