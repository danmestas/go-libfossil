package sync

import (
	"context"
	"github.com/danmestas/go-libfossil/internal/content"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/shun"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// TestCloneShunRules pins fossil's shun handling during a clone: a shunned
// cfile is dropped (xfer_accept_compressed_file checks the shun list
// always), a shunned file card is kept (xfer_accept_file skips the check for
// a clone), and a shunned delta basis never becomes a phantom to ask for.
func TestCloneShunRules(t *testing.T) {
	r := setupSyncTestRepo(t)
	linker, err := manifest.NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	cs := &cloneSession{
		repo: r, env: simio.RealEnv(), linker: linker, seqno: 1,
		phantoms: map[string]bool{}, availableDeltaSources: map[string]struct{}{},
		basis: content.NewUncheckedCache(receiveBasisBytes),
	}
	cs.obs = resolveObserver(nil)

	fileData := []byte("a shunned file card")
	cfileData := []byte("a shunned cfile card")
	base := []byte("a shunned basis, long enough for a delta to be worth it")
	target := []byte("a shunned basis, long enough for a delta to be worth it, v2")
	deltaBytes, err := delta.Create(base, target)
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	fileUUID, cfileUUID, baseUUID := hash.SHA1(fileData), hash.SHA1(cfileData), hash.SHA1(base)
	for _, u := range []string{fileUUID, cfileUUID, baseUUID} {
		if err := shun.Add(r.DB(), u, ""); err != nil {
			t.Fatalf("shun.Add: %v", err)
		}
	}

	msg := &xfer.Message{Cards: []xfer.Card{
		&xfer.FileCard{UUID: fileUUID, Content: fileData},
		&xfer.CFileCard{UUID: cfileUUID, Content: cfileData},
		&xfer.CFileCard{UUID: hash.SHA1(target), DeltaSrc: baseUUID, Content: deltaBytes},
	}}
	if err := cs.processResponse(context.Background(), 1, msg); err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if _, ok := blob.Exists(r.DB(), fileUUID); !ok {
		t.Error("a shunned file card was dropped during a clone")
	}
	if _, ok := blob.Exists(r.DB(), cfileUUID); ok {
		t.Error("a shunned cfile was stored during a clone")
	}
	if cs.phantoms[baseUUID] {
		t.Error("a shunned delta basis became a phantom to ask for")
	}
	// The delta on the shunned basis can never be read, so it is never
	// judged not a seed (horizon §5.2.3) and is not counted either.
	if cs.result.BlobsRecvd != 1 {
		t.Errorf("BlobsRecvd = %d, want 1 (the shunned cfile and the unreadable delta not counted)",
			cs.result.BlobsRecvd)
	}
}
