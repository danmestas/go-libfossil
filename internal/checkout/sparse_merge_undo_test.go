//go:build !js

package checkout

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	syncproto "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestSparseMergeConflictReported(t *testing.T) {
	f := newSparseRepo(t, false)
	ours := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "ours\n"}), f.c1)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "theirs\n"}), f.c1)
	co := f.open(t, sparseCone(t), ours)
	if _, err := co.Merge(MergeOpts{Version: theirs}); err != nil {
		t.Fatal(err)
	}
	conflicts, err := co.Conflicts()
	if err != nil || len(conflicts) != 1 || conflicts[0] != "tools/t.c" {
		t.Fatalf("outside conflicts = %q: %v", conflicts, err)
	}
}

func TestSparseMergeFailureDoesNotPublishPayload(t *testing.T) {
	f := newSparseRepo(t, false)
	ours := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "ours\n"}), f.c1)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "theirs\n"}), f.c1)
	co := f.open(t, sparseCone(t), ours)
	if _, err := co.db.Exec(`CREATE TRIGGER reject_merge BEFORE UPDATE OF mrid ON vfile
	 BEGIN SELECT RAISE(ABORT, 'injected merge failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: theirs}); err == nil {
		t.Fatal("merge succeeded despite injected metadata failure")
	}
	var parents int
	if err := co.db.QueryRow("SELECT count(*) FROM vmerge").Scan(&parents); err != nil || parents != 0 {
		t.Fatalf("failed merge left %d parent records: %v", parents, err)
	}
	rows, err := f.repo.DB().Query("SELECT b.uuid FROM private p JOIN blob b ON b.rid=p.rid")
	if err != nil {
		t.Fatal(err)
	}
	var uuids []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			t.Fatal(err)
		}
		uuids = append(uuids, uuid)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(uuids) == 0 {
		t.Fatal("injected failure did not exercise synthesized payload storage")
	}
	for _, uuid := range uuids {
		assertSparsePayloadSyncVisibility(t, f.repo, uuid, false)
	}
}

func TestSparseMergePayloadRetainsPublicVisibility(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, sparseCone(t), f.c1)
	data := []byte("existing public merge result\n")
	rid, uuid, _, err := blob.StoreUnderPolicy(f.repo.DB(), data)
	if err != nil {
		t.Fatal(err)
	}
	gotRID, gotUUID, err := co.storeMergePayload(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotRID != rid || gotUUID != uuid || content.IsPrivate(f.repo.DB(), int64(rid)) {
		t.Fatalf("existing public artifact changed identity or visibility: %d %s", gotRID, gotUUID)
	}
	assertSparsePayloadSyncVisibility(t, f.repo, uuid, true)
}

func TestSparseMergeSynthesizedPayloadPrivate(t *testing.T) {
	f := newSparseRepo(t, false)
	base := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"}), f.c1)
	ours := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "ours\ntwo\nthree\nfour\nfive\nsix\nseven\n"}), base)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "one\ntwo\nthree\nfour\nfive\nsix\ntheirs\n"}), base)
	co := f.open(t, sparseCone(t), ours)
	if _, err := co.Merge(MergeOpts{Version: theirs}); err != nil {
		t.Fatal(err)
	}
	var payload int64
	if err := co.db.QueryRow("SELECT mrid FROM vfile WHERE pathname='tools/t.c'").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !content.IsPrivate(f.repo.DB(), payload) {
		t.Fatal("uncommitted synthesized payload is publicly visible")
	}
	uuid, err := blobUUID(f.repo.DB(), libfossil.FslID(payload))
	if err != nil {
		t.Fatal(err)
	}
	assertSparsePayloadSyncVisibility(t, f.repo, uuid, false)
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if !content.IsPrivate(f.repo.DB(), payload) {
		t.Fatal("undo published uncommitted synthesized payload")
	}
	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := co.Commit(CommitOpts{Message: "publish merged payload", User: "test", Time: f.time.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if content.IsPrivate(f.repo.DB(), payload) {
		t.Fatal("committed synthesized payload remains private")
	}
	assertSparsePayloadSyncVisibility(t, f.repo, uuid, true)
}

func assertSparsePayloadSyncVisibility(t *testing.T, r *repo.Repo, uuid string, visible bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen, requested := false, false
	transport := &syncproto.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		for _, card := range req.Cards {
			switch card := card.(type) {
			case *xfer.IGotCard:
				seen = seen || card.UUID == uuid
			case *xfer.FileCard:
				seen = seen || card.UUID == uuid
			case *xfer.CFileCard:
				seen = seen || card.UUID == uuid
			}
		}
		if !requested {
			requested = true
			return &xfer.Message{Cards: []xfer.Card{&xfer.GimmeCard{UUID: uuid}}}
		}
		return &xfer.Message{}
	}}
	if _, err := syncproto.Sync(ctx, r, transport, syncproto.SyncOpts{Push: true}); err != nil {
		t.Fatal(err)
	}
	if seen != visible {
		t.Fatalf("ordinary sync visibility=%v, want %v", seen, visible)
	}
}

func TestSparseMergeDoesNotFollowOutsideSymlink(t *testing.T) {
	for _, tree := range []bool{false, true} {
		f := newSparseRepo(t, tree)
		remoteFiles := maps(sparseFiles, map[string]string{"tools/t.c": "remote\n", "tools/new.c": "new\n"})
		remote := f.commit(t, remoteFiles, f.c1)
		co := f.open(t, sparseCone(t), f.c1)
		outside := t.TempDir()
		sentinel := filepath.Join(outside, "t.c")
		if err := os.WriteFile(sentinel, []byte("do not touch\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(co.Dir(), "tools")); err != nil {
			t.Fatal(err)
		}
		if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
			t.Fatal(err)
		}
		if err := co.Revert(RevertOpts{}); err != nil {
			t.Fatal(err)
		}
		if _, err := co.Undo(); err != nil {
			t.Fatal(err)
		}
		rid, _, err := co.Commit(CommitOpts{Message: "record-only symlink", User: "test", Time: f.time.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		assertCheckinContent(t, f.repo, rid, remoteFiles)
		data, err := os.ReadFile(sentinel)
		if err != nil || string(data) != "do not touch\n" {
			t.Fatalf("outside sentinel changed: %q, %v", data, err)
		}
		if _, err := os.Lstat(filepath.Join(outside, "new.c")); !os.IsNotExist(err) {
			t.Fatalf("outside added path materialized: %v", err)
		}
	}
}

func TestSparseMergeCancel(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "remote\n", "tools/new.c": "new\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{Paths: []string{"tools/t.c"}}); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "cancel merge", User: "test", AllowEmpty: true, Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, sparseFiles)
}

func TestSparseMergeUndoRedo(t *testing.T) {
	f := newSparseRepo(t, false)
	remoteFiles := maps(sparseFiles, map[string]string{"tools/t.c": "remote\n", "src/a.c": "remote source\n"})
	remote := f.commit(t, remoteFiles, f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	var parents int
	if err := co.db.QueryRow("SELECT count(*) FROM vmerge").Scan(&parents); err != nil || parents != 0 {
		t.Fatalf("undo merge parents=%d: %v", parents, err)
	}
	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "redo merge", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, remoteFiles)
}
