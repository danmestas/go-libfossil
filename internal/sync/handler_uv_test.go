package sync

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func handleReq(t *testing.T, r *repo.Repo, cards ...xfer.Card) *xfer.Message {
	t.Helper()
	req := &xfer.Message{Cards: cards}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	return resp
}

func TestHandlerPragmaUVHash_Match(t *testing.T) {
	r := setupSyncTestRepo(t)
	if err := uv.EnsureSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	uv.Write(r.DB(), "test.txt", []byte("hello"), 100)

	h, _ := uv.ContentHash(r.DB())

	// Send matching hash — should get no uvigot back.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "uv-hash", Values: []string{h}},
	)

	for _, c := range resp.Cards {
		if _, ok := c.(*xfer.UVIGotCard); ok {
			t.Error("should not send uvigot when hashes match")
		}
	}
}

func TestHandlerPragmaUVHash_Mismatch(t *testing.T) {
	r := setupSyncTestRepo(t)
	if err := uv.EnsureSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	uv.Write(r.DB(), "a.txt", []byte("aaa"), 100)
	uv.Write(r.DB(), "b.txt", []byte("bbb"), 200)

	// Send wrong hash — should get uvigot for each file + pragma uv-push-ok.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "uv-hash", Values: []string{"wrong"}},
	)

	uvigots := cardsByType(resp, xfer.CardUVIGot)
	if len(uvigots) != 2 {
		t.Errorf("expected 2 uvigot cards, got %d", len(uvigots))
	}

	pragmas := cardsByType(resp, xfer.CardPragma)
	found := false
	for _, c := range pragmas {
		if c.(*xfer.PragmaCard).Name == "uv-push-ok" {
			found = true
		}
	}
	if !found {
		t.Error("expected pragma uv-push-ok")
	}
}

func TestHandlerUVGimme(t *testing.T) {
	r := setupSyncTestRepo(t)
	if err := uv.EnsureSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	uv.Write(r.DB(), "doc.txt", []byte("document content"), 100)

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.UVGimmeCard{Name: "doc.txt"},
	)

	uvfiles := cardsByType(resp, xfer.CardUVFile)
	if len(uvfiles) != 1 {
		t.Fatalf("expected 1 uvfile, got %d", len(uvfiles))
	}
	f := uvfiles[0].(*xfer.UVFileCard)
	if f.Name != "doc.txt" || string(f.Content) != "document content" {
		t.Errorf("uvfile = %+v", f)
	}
}

func TestHandlerUVFile_Accepted(t *testing.T) {
	r := setupSyncTestRepo(t)
	if err := uv.EnsureSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	resp := handleReq(t, r,
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.UVFileCard{
			Name:    "new.txt",
			MTime:   100,
			Hash:    "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d", // sha1("hello")
			Size:    5,
			Flags:   0,
			Content: []byte("hello"),
		},
	)

	// Should not have errors.
	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			t.Errorf("unexpected error: %s", e.Message)
		}
	}

	// Verify stored.
	content, mtime, hash, err := uv.Read(r.DB(), "new.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(content) != "hello" || mtime != 100 || hash == "" {
		t.Errorf("stored = content=%q mtime=%d hash=%q", content, mtime, hash)
	}
}

func TestHandlerUVFile_Rejected_NoPush(t *testing.T) {
	r := setupSyncTestRepo(t)
	if err := uv.EnsureSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	// No push card — uvfile should be rejected.
	resp := handleReq(t, r,
		&xfer.UVFileCard{
			Name:    "new.txt",
			MTime:   100,
			Hash:    "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d",
			Size:    5,
			Flags:   0,
			Content: []byte("hello"),
		},
	)

	errors := cardsByType(resp, xfer.CardError)
	if len(errors) == 0 {
		t.Error("expected error card for uvfile without push")
	}
}

func TestHandlerUVIGot_ServerPulls(t *testing.T) {
	r := setupSyncTestRepo(t)
	if err := uv.EnsureSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// Server has nothing — client announces a file via uvigot.

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.UVIGotCard{Name: "client.txt", MTime: 100, Hash: "abc", Size: 10},
	)

	gimmes := cardsByType(resp, xfer.CardUVGimme)
	if len(gimmes) != 1 {
		t.Fatalf("expected 1 uvgimme, got %d", len(gimmes))
	}
	if gimmes[0].(*xfer.UVGimmeCard).Name != "client.txt" {
		t.Errorf("wrong gimme name")
	}
}
