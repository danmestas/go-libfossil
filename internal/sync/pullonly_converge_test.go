package sync

// A pull-only session converges even when the client holds artifacts the
// server lacks (#277): the server does not gimme what a non-pushing client
// announces, and the client ignores gimmes it cannot satisfy.

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/testutil"
)

const pullOnlyMaxRounds = 5

func storeBlob(t *testing.T, r *repo.Repo, body string) string {
	t.Helper()
	_, uuid, err := blob.Store(r.DB(), []byte(body))
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	return uuid
}

func TestPullOnlyConvergesWhenServerLacksClientArtifacts(t *testing.T) {
	server := setupSyncTestRepo(t)
	client := setupSyncTestRepo(t)
	shared := "artifact held by both"
	storeBlob(t, server, shared)
	storeBlob(t, client, shared)
	serverOnly := storeBlob(t, server, "artifact only the server holds")
	clientOnly := []string{
		storeBlob(t, client, "client-only artifact one"),
		storeBlob(t, client, "client-only artifact two"),
	}

	rt := &recordingTransport{src: server}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Sync(ctx, client, rt, SyncOpts{
		Pull: true, ProjectCode: "pc",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if res.Rounds > pullOnlyMaxRounds {
		t.Errorf("Rounds = %d, want <= %d", res.Rounds, pullOnlyMaxRounds)
	}
	if res.FilesRecvd != 1 {
		t.Errorf("FilesRecvd = %d, want 1 (only the server-only artifact)", res.FilesRecvd)
	}
	if res.FilesSent != 0 {
		t.Errorf("FilesSent = %d, want 0", res.FilesSent)
	}
	if _, ok := blob.Exists(client.DB(), serverOnly); !ok {
		t.Error("server-only artifact was not pulled")
	}
	for _, u := range clientOnly {
		if _, ok := blob.Exists(server.DB(), u); ok {
			t.Errorf("pull-only sync pushed %s to the server", u)
		}
	}
	for i, req := range rt.requests {
		if n := len(cardsByType(req, xfer.CardFile)) +
			len(cardsByType(req, xfer.CardCFile)); n != 0 {
			t.Errorf("request %d carries %d file cards", i, n)
		}
		if n := len(cardsByType(rt.replies[i], xfer.CardGimme)); n != 0 {
			t.Errorf("reply %d carries %d gimme cards to a pull-only client", i, n)
		}
	}
}

// gimmeForeverTransport answers every request with a gimme for an artifact
// the client does not hold, as an older libfossil server would.
type gimmeForeverTransport struct{ uuid string }

func (g *gimmeForeverTransport) Exchange(
	_ context.Context, _ *xfer.Message,
) (*xfer.Message, error) {
	return &xfer.Message{Cards: []xfer.Card{&xfer.GimmeCard{UUID: g.uuid}}}, nil
}

func TestPullOnlyIgnoresServerGimmes(t *testing.T) {
	client := setupSyncTestRepo(t)
	storeBlob(t, client, "client-held artifact")
	tr := &gimmeForeverTransport{uuid: "dddddddddddddddddddddddddddddddddddddddd"}

	res, err := Sync(context.Background(), client, tr, SyncOpts{
		Pull: true, ProjectCode: "pc",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Rounds > pullOnlyMaxRounds {
		t.Errorf("Rounds = %d, want <= %d", res.Rounds, pullOnlyMaxRounds)
	}
	if res.FilesSent != 0 {
		t.Errorf("FilesSent = %d, want 0", res.FilesSent)
	}
}

func TestPullOnlyFromRealFossilWhenServerLacksClientArtifacts(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.fossil")
	if out, err := exec.Command(bin, "new", remote).CombinedOutput(); err != nil {
		t.Fatalf("fossil new: %v\n%s", err, out)
	}
	url := startFossilServer(t, remote)
	local := filepath.Join(dir, "local.fossil")
	if out, err := exec.Command(bin, "clone", url, local).CombinedOutput(); err != nil {
		t.Fatalf("fossil clone: %v\n%s", err, out)
	}
	r, err := repo.Open(local)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r.Close()
	storeBlob(t, r, "artifact the fossil server lacks")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Sync(ctx, r, &HTTPTransport{URL: url}, SyncOpts{
		Pull:        true,
		ProjectCode: getProjectCode(t, local),
		ServerCode:  getServerCode(t, local),
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.FilesSent != 0 {
		t.Errorf("FilesSent = %d, want 0", res.FilesSent)
	}
}
