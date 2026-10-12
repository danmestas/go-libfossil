package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

var kvDef = repo.TableDef{
	Columns:  []repo.ColumnDef{{Name: "id", Type: "text", PK: true}, {Name: "val", Type: "text"}},
	Conflict: "mtime-wins",
}

// kvPKHash is the PK hash of the row the hostile payloads below claim to be.
func kvPKHash(id string) string {
	return repo.PKHash([]repo.ColumnDef{kvDef.Columns[0]}, map[string]any{"id": id})
}

// setupKVRepo returns a repo with the "kv" table registered for sync. It takes
// a testing.TB so a fuzz target can build one repo for all its inputs: a fuzz
// input's TempDir holds a '#', which SQLite reads as the start of a URI fragment.
func setupKVRepo(t testing.TB) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "kv.fossil"), "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	if err := repo.EnsureSyncSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSyncSchema: %v", err)
	}
	if err := repo.RegisterSyncedTable(r.DB(), "kv", kvDef, 100); err != nil {
		t.Fatalf("RegisterSyncedTable: %v", err)
	}
	return r
}

// hostileRowPayloads are xrow and xdelete payloads a peer can send that are
// not a JSON object of the table's columns with scalar values. The last two
// name a column the table does not declare; UpsertXRow writes row keys into
// its SQL, so such a key is a statement fragment unless it is refused.
var hostileRowPayloads = []string{
	`null`,
	`[]`,
	`"id"`,
	`17`,
	`{"id":"row1"} {"id":"row2"}`,
	`{"id":{"nested":1}}`,
	`{"id":["a"]}`,
	`{"id":"row1","val":{"nested":1}}`,
	`{"id":"row1","nosuch":"x"}`,
	`{"id":"row1","val, mtime) VALUES('inj','x',7) --":"x"}`,
}

// TestHandleXRowRejectsHostilePayload checks that a server handed an xrow
// payload that is not a row of the table answers with an error card and
// stores nothing. Method: push each hostile payload under a PK hash that
// matches its claimed id. JSON null decoded to a nil map, which reached
// coerceJSONNumbers' assertion and panicked the handler.
func TestHandleXRowRejectsHostilePayload(t *testing.T) {
	for _, payload := range hostileRowPayloads {
		t.Run(payload, func(t *testing.T) {
			r := setupKVRepo(t)
			req := &xfer.Message{Cards: []xfer.Card{
				&xfer.PushCard{ServerCode: "s", ProjectCode: testPC},
				&xfer.XRowCard{Table: "kv", PKHash: kvPKHash("row1"), MTime: 300,
					Content: []byte(payload)},
			}}
			resp, err := HandleSync(context.Background(), r, req)
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			if len(findCards[*xfer.ErrorCard](resp)) == 0 {
				t.Error("no error card for a payload that is not a row")
			}
			assertKVEmpty(t, r)
		})
	}
}

// TestHandleXDeleteRejectsHostilePKData is the xdelete counterpart: PKData
// for a row the server lacks becomes a tombstone, so it is decoded the same
// way and must fail the same way.
func TestHandleXDeleteRejectsHostilePKData(t *testing.T) {
	for _, payload := range hostileRowPayloads {
		t.Run(payload, func(t *testing.T) {
			r := setupKVRepo(t)
			req := &xfer.Message{Cards: []xfer.Card{
				&xfer.PushCard{ServerCode: "s", ProjectCode: testPC},
				&xfer.XDeleteCard{Table: "kv", PKHash: kvPKHash("row1"), MTime: 300,
					PKData: []byte(payload)},
			}}
			// A bad PKData fails the request, as a PKData hash mismatch does;
			// what matters is that it neither panics nor stores a tombstone.
			_, _ = HandleSync(context.Background(), r, req)
			assertKVEmpty(t, r)
		})
	}
}

// TestClientRejectsHostileRowPayload checks the client side: an xrow or
// xdelete from a hostile server is an error for that card, not a panic in
// the syncing process, and stores nothing.
func TestClientRejectsHostileRowPayload(t *testing.T) {
	for _, payload := range hostileRowPayloads {
		t.Run(payload, func(t *testing.T) {
			r := setupKVRepo(t)
			s := newSession(r, SyncOpts{})
			xrow := &xfer.XRowCard{Table: "kv", PKHash: kvPKHash("row1"), MTime: 300,
				Content: []byte(payload)}
			if err := s.handleXRowResponse(xrow); err == nil {
				t.Error("handleXRowResponse accepted a payload that is not a row")
			}
			xdel := &xfer.XDeleteCard{Table: "kv", PKHash: kvPKHash("row1"), MTime: 300,
				PKData: []byte(payload)}
			if err := s.handleXDeleteResponse(xdel); err == nil {
				t.Error("handleXDeleteResponse accepted PKData that is not a row")
			}
			assertKVEmpty(t, r)
		})
	}
}

func assertKVEmpty(t *testing.T, r *repo.Repo) {
	t.Helper()
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM x_kv").Scan(&n); err != nil {
		t.Fatalf("count x_kv: %v", err)
	}
	if n != 0 {
		t.Fatalf("x_kv holds %d rows after a rejected payload", n)
	}
}

// FuzzTableSyncPayload checks that no xrow or xdelete payload a peer sends
// panics the server handler or the client, whatever its JSON shape.
func FuzzTableSyncPayload(f *testing.F) {
	for _, p := range hostileRowPayloads {
		f.Add([]byte(p))
	}
	f.Add([]byte(`{"id":"row1","val":"v"}`))
	f.Add([]byte(`{"id":12345678901234567890}`))
	r := setupKVRepo(f)
	f.Fuzz(func(t *testing.T, payload []byte) {
		if _, err := r.DB().Exec("DELETE FROM x_kv"); err != nil {
			t.Fatalf("reset x_kv: %v", err)
		}
		pk := kvPKHash("row1")
		req := &xfer.Message{Cards: []xfer.Card{
			&xfer.PushCard{ServerCode: "s", ProjectCode: testPC},
			&xfer.XRowCard{Table: "kv", PKHash: pk, MTime: 300, Content: payload},
			&xfer.XDeleteCard{Table: "kv", PKHash: pk, MTime: 400, PKData: payload},
		}}
		// Only a panic fails: an error is the handler refusing the payload.
		_, _ = HandleSync(context.Background(), r, req)
		s := newSession(r, SyncOpts{})
		_ = s.handleXRowResponse(
			&xfer.XRowCard{Table: "kv", PKHash: pk, MTime: 500, Content: payload})
		_ = s.handleXDeleteResponse(
			&xfer.XDeleteCard{Table: "kv", PKHash: pk, MTime: 600, PKData: payload})
	})
}
