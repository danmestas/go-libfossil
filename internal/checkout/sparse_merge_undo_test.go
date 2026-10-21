//go:build !js

package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	syncproto "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestLegacyDenseUndoWithoutConeStamp(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "updated\n"}), f.c1)
	co := f.open(t, nil, f.c1)
	if _, err := co.Update(UpdateOpts{TargetRID: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec("DELETE FROM vvar WHERE name='undo_sparse_cone'"); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(co.Dir(), "src/a.c")); err != nil || string(data) != sparseFiles["src/a.c"] {
		t.Fatalf("legacy dense undo bytes: %q, %v", data, err)
	}
}

func TestLegacySparseUndoAfterWideningRefused(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "legacy pending\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec("DELETE FROM vvar WHERE name='undo_sparse_cone'"); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	before, err := co.RecoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err == nil || !strings.Contains(err.Error(), "legacy sparse undo image") {
		t.Fatalf("unstamped legacy undo = %v", err)
	}
	after, err := co.RecoverySnapshot()
	if err != nil || before != after {
		t.Fatalf("legacy refusal changed checkout metadata: %q != %q: %v", before, after, err)
	}
	if data, err := os.ReadFile(filepath.Join(co.Dir(), "tools/t.c")); err != nil || string(data) != sparseFiles["tools/t.c"] {
		t.Fatalf("legacy refusal changed outside bytes: %q, %v", data, err)
	}
}

func TestSparseRevertUndoAfterWideningRefused(t *testing.T) {
	f := newSparseRepo(t, false)
	merged := maps(sparseFiles, map[string]string{"tools/t.c": "retained merge\n"})
	remote := f.commit(t, merged, f.c1)
	original := sparseCone(t)
	co := f.open(t, original, f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err == nil || !strings.Contains(err.Error(), "undo image") {
		t.Fatalf("undo of sparse revert after widening = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(co.Dir(), "tools/t.c")); err != nil || string(got) != sparseFiles["tools/t.c"] {
		t.Fatalf("refused undo changed baseline bytes: %q, %v", got, err)
	}
	if _, err := co.SetCone(original); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "restored merge", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, merged)
}

func TestSparseDenseUndoRequiresWidening(t *testing.T) {
	f := newSparseRepo(t, false)
	co := f.open(t, nil, f.c1)
	file := filepath.Join(co.Dir(), "tools/t.c")
	if err := os.WriteFile(file, []byte("saved dense edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(sparseCone(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("dense undo under narrow cone = %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused undo materialized outside file: %v", err)
	}
	var count int
	if err := co.db.QueryRow("SELECT count(*) FROM vfile").Scan(&count); err != nil || count != len(sparseCovered) {
		t.Fatalf("refused undo changed managed rows: %d, %v", count, err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "saved dense edit\n" {
		t.Fatalf("widening lost saved undo bytes: %q, %v", got, err)
	}
}

func TestSparseDenseRedoRequiresWidening(t *testing.T) {
	f := newSparseRepo(t, false)
	target := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "redo target\n"}), f.c1)
	co := f.open(t, nil, f.c1)
	if _, err := co.Update(UpdateOpts{TargetRID: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetCone(sparseCone(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Redo(); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("dense redo under narrow cone = %v", err)
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), "tools/t.c")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused redo materialized outside file: %v", err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(co.Dir(), "tools/t.c")); err != nil || string(got) != "redo target\n" {
		t.Fatalf("widening lost redo target: %q, %v", got, err)
	}
}

func TestSparseUpdateRetainsPrivateOutcome(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "remote\n"}), f.c1)
	target := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "target\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	rid, uuid, err := co.storeMergePayload([]byte("retained private result\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec("UPDATE sparse_pending SET mrid=? WHERE pathname='tools/t.c'", int64(rid)); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(UpdateOpts{TargetRID: target}); err != nil {
		t.Fatal(err)
	}
	if !content.IsPrivate(f.repo.DB(), int64(rid)) {
		t.Fatal("update published retained uncommitted bytes")
	}
	assertSparsePayloadSyncVisibility(t, f.repo, uuid, false)
	committed, _, err := co.Commit(CommitOpts{Message: "carried private result", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, committed, maps(sparseFiles, map[string]string{"src/a.c": "target\n", "tools/t.c": "retained private result\n"}))
	assertSparsePayloadSyncVisibility(t, f.repo, uuid, true)
}

func TestSparsePendingUndoAfterUpdate(t *testing.T) {
	f := newSparseRepo(t, false)
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "merged\n"}), f.c1)
	target := f.commit(t, maps(sparseFiles, map[string]string{"src/a.c": "target\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, err := co.Merge(MergeOpts{Version: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(UpdateOpts{TargetRID: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	rid, _, err := co.Commit(CommitOpts{Message: "restored pending", User: "test", Time: f.time.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckinContent(t, f.repo, rid, maps(sparseFiles, map[string]string{"tools/t.c": "merged\n"}))
}

func TestSparseMergeConflictReported(t *testing.T) {
	f := newSparseRepo(t, false)
	ours := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "ours\n"}), f.c1)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "theirs\n"}), f.c1)
	co := f.open(t, sparseCone(t), ours)
	if _, err := co.Merge(MergeOpts{Version: theirs}); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("outside both-changed merge = %v, want explicit widening refusal", err)
	}
	conflicts, err := co.Conflicts()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("outside conflicts = %q: %v", conflicts, err)
	}
}

func TestSparseBothChangedRequiresWideningWithoutSynthesis(t *testing.T) {
	f := newSparseRepo(t, false)
	base := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"}), f.c1)
	ours := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "ours\ntwo\nthree\nfour\nfive\nsix\nseven\n"}), base)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "one\ntwo\nthree\nfour\nfive\nsix\ntheirs\n"}), base)
	co := f.open(t, sparseCone(t), ours)
	var before int
	if err := f.repo.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: theirs}); !errors.Is(err, ErrOutsideCone) {
		t.Fatalf("expected widening refusal: %v", err)
	}
	var after, outside int
	if err := f.repo.DB().QueryRow("SELECT count(*) FROM blob").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("outside merge synthesized content: %d blobs became %d", before, after)
	}
	if err := co.db.QueryRow("SELECT count(*) FROM vfile WHERE pathname='tools/t.c'").Scan(&outside); err != nil || outside != 0 {
		t.Fatalf("outside managed rows = %d: %v", outside, err)
	}
	for _, table := range []string{"vmerge", "sparse_pending"} {
		var count int
		if err := co.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("refused merge left %s rows: %d, %v", table, count, err)
		}
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: theirs}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(co.Dir(), "tools/t.c"))
	if err != nil || string(data) != "ours\ntwo\nthree\nfour\nfive\nsix\ntheirs\n" {
		t.Fatalf("widened merge = %q: %v", data, err)
	}
	if conflicts, err := co.Conflicts(); err != nil || len(conflicts) != 0 {
		t.Fatalf("clean widened merge conflicts = %v: %v", conflicts, err)
	}
}

func TestSparseMergeFailureDoesNotPublishPayload(t *testing.T) {
	f := newSparseRepo(t, false)
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "theirs\n"}), f.c1)
	co := f.open(t, sparseCone(t), f.c1)
	if _, _, err := co.storeMergePayload([]byte("retained legacy uncommitted payload\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec(`CREATE TRIGGER reject_merge BEFORE INSERT ON sparse_pending
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
		t.Fatal("failure lost the retained private payload")
	}
	for _, uuid := range uuids {
		assertSparsePayloadSyncVisibility(t, f.repo, uuid, false)
	}
}

func TestSparseMergePayloadRetainsPublicPhantomVisibility(t *testing.T) {
	f := newSparseRepo(t, false)
	data := []byte("published history awaiting its bytes\n")
	remote := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": string(data)}), f.c1)
	rid, uuid, _, err := blob.StoreUnderPolicy(f.repo.DB(), data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.DB().Exec("UPDATE blob SET size=-1, content=NULL WHERE rid=?", int64(rid)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.DB().Exec("INSERT OR IGNORE INTO phantom(rid) VALUES(?)", int64(rid)); err != nil {
		t.Fatal(err)
	}
	co := f.open(t, sparseCone(t), f.c1)
	gotRID, gotUUID, err := co.storeMergePayload(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotRID != rid || gotUUID != uuid || content.IsPrivate(f.repo.DB(), int64(rid)) {
		t.Fatalf("filling public-history phantom changed identity or visibility: %d %s", gotRID, gotUUID)
	}
	assertSparsePayloadSyncVisibility(t, f.repo, uuid, true)
	if err := co.Revert(RevertOpts{}); err != nil {
		t.Fatal(err)
	}
	assertSparsePayloadSyncVisibility(t, f.repo, uuid, true)
	if _, err := co.db.Exec(`CREATE TRIGGER reject_public_merge BEFORE INSERT ON sparse_pending
	 BEGIN SELECT RAISE(ABORT, 'injected public phantom merge failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(MergeOpts{Version: remote}); err == nil || !strings.Contains(err.Error(), "injected public phantom merge failure") {
		t.Fatalf("merge failure = %v, want injected public phantom failure", err)
	}
	assertSparsePayloadSyncVisibility(t, f.repo, uuid, true)
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
	theirs := f.commit(t, maps(sparseFiles, map[string]string{"tools/t.c": "one\ntwo\nthree\nfour\nfive\nsix\ntheirs\n"}), base)
	co := f.open(t, sparseCone(t), base)
	if _, err := co.Merge(MergeOpts{Version: theirs}); err != nil {
		t.Fatal(err)
	}
	// A retained payload from an older writer must remain private through
	// undo/redo and publish only when a complete commit references it.
	stored, _, err := co.storeMergePayload([]byte("ours\ntwo\nthree\nfour\nfive\nsix\ntheirs\n"))
	if err != nil {
		t.Fatal(err)
	}
	payload := int64(stored)
	if _, err := co.db.Exec("UPDATE sparse_pending SET mrid=?, unresolved=0 WHERE pathname='tools/t.c'", payload); err != nil {
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
