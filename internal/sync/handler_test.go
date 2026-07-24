package sync

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// findCards returns all cards of type T from a message.
func findCards[T xfer.Card](msg *xfer.Message) []T {
	var out []T
	for _, c := range msg.Cards {
		if tc, ok := c.(T); ok {
			out = append(out, tc)
		}
	}
	return out
}

// storeTestBlob stores a blob and returns its UUID.
func storeTestBlob(t *testing.T, r *repo.Repo, data []byte) string {
	t.Helper()
	uuid := hash.SHA1(data)
	if err := storeReceivedFile(r, uuid, "", data, nil); err != nil {
		t.Fatalf("storeReceivedFile: %v", err)
	}
	return uuid
}

func testProjectCode(t *testing.T, d *db.DB) string {
	t.Helper()
	var code string
	if err := d.QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&code); err != nil {
		t.Fatalf("project-code: %v", err)
	}
	return code
}

func buildTestLoginCard(user, password, projectCode string, payload []byte) *xfer.LoginCard {
	nonce := testSHA1Hex(payload)
	shared := testSHA1Hex([]byte(projectCode + "/" + user + "/" + password))
	sig := testSHA1Hex([]byte(nonce + shared))
	return &xfer.LoginCard{User: user, Nonce: nonce, Signature: sig}
}

func testSHA1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}


func TestHandlePull(t *testing.T) {
	r := setupSyncTestRepo(t)
	uuid := storeTestBlob(t, r, []byte("pull me"))

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	igots := findCards[*xfer.IGotCard](resp)
	found := false
	for _, ig := range igots {
		if ig.UUID == uuid {
			found = true
		}
	}
	if !found {
		t.Fatalf("pull response missing igot for %s", uuid)
	}
}

func TestHandleIGotGimme(t *testing.T) {
	r := setupSyncTestRepo(t)
	unknownUUID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.IGotCard{UUID: unknownUUID},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	gimmes := findCards[*xfer.GimmeCard](resp)
	found := false
	for _, g := range gimmes {
		if g.UUID == unknownUUID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected gimme for unknown UUID")
	}
}

// TestHandleIGotWithoutPushOrPull verifies the server does not gimme when
// the client is neither pushing nor pulling. Without pushOK there is no
// mandate for the client to send files, so issuing a gimme is wasted.
func TestHandleIGotWithoutPushOrPull(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.IGotCard{UUID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	gimmes := findCards[*xfer.GimmeCard](resp)
	if len(gimmes) > 0 {
		t.Fatal("should not gimme without push or pull card")
	}
}

// TestHandleIGotWithPushOnly verifies the server emits gimme cards when
// the client is push-only. This is the multi-round push-without-pull case
// — without it, push-only clients exit after one round even when the
// server is missing artifacts the client has just announced.
func TestHandleIGotWithPushOnly(t *testing.T) {
	r := setupSyncTestRepo(t)
	unknownUUID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.IGotCard{UUID: unknownUUID},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	gimmes := findCards[*xfer.GimmeCard](resp)
	found := false
	for _, g := range gimmes {
		if g.UUID == unknownUUID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected gimme for unknown UUID under push-only")
	}
}

func TestHandleGimme(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("gimme this")
	uuid := storeTestBlob(t, r, data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.GimmeCard{UUID: uuid},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	files := findCards[*xfer.FileCard](resp)
	found := false
	for _, f := range files {
		if f.UUID == uuid && string(f.Content) == string(data) {
			found = true
		}
	}
	if !found {
		t.Fatal("expected file card with correct content")
	}
}

func TestHandleGimmeMissing(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.GimmeCard{UUID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	files := findCards[*xfer.FileCard](resp)
	if len(files) > 0 {
		t.Fatal("should not return file for missing blob")
	}
}

func TestHandlePushFile(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("push this")
	uuid := hash.SHA1(data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}

	_, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatal("pushed blob not stored")
	}
}

func TestHandleFileWithoutPush(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("no push card")
	uuid := hash.SHA1(data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) == 0 {
		t.Fatal("expected error for file without push")
	}
}

func TestHandleClone(t *testing.T) {
	r := setupSyncTestRepo(t)
	stored := map[string]bool{}
	for i := range 5 {
		data := []byte(fmt.Sprintf("clone test %d", i))
		uuid := storeTestBlob(t, r, data)
		stored[uuid] = true
	}

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// §7.2: clone sends cfile, not file — no plain file cards should appear.
	if plain := findCards[*xfer.FileCard](resp); len(plain) > 0 {
		t.Fatalf("clone response carried %d 'file' cards; §7.2 requires 'cfile'", len(plain))
	}
	files := findCards[*xfer.CFileCard](resp)
	for _, f := range files {
		delete(stored, f.UUID)
	}
	if len(stored) > 0 {
		t.Fatalf("clone missing blobs: %v", stored)
	}
}

// TestHandleClonePushCardTrailsCloneSeqno pins the card order a clone response
// must use to keep a real fossil client from re-transferring the whole
// repository (issue #138). Canonical's client re-issues `clone 3 SEQNO` every
// time it sees a server `push` card while its cloneSeqno is still > 0
// (fossil-scm xfer.c:2706), and canonical's server emits `push` *after* the
// terminal `clone_seqno 0` (xfer.c:1571 then 1577). If go-libfossil emits
// `push` before `clone_seqno`, the client queues one more `clone 3 SEQNO`
// before it learns the cursor reached 0, and the server serves the entire
// content a second time -- the measured 2.06x end-to-end blow-up. So in a clone
// response the CloneSeqNoCard must precede the PushCard.
func TestHandleClonePushCardTrailsCloneSeqno(t *testing.T) {
	r := setupSyncTestRepo(t)
	for i := range 5 {
		storeTestBlob(t, r, []byte(fmt.Sprintf("clone order test %d", i)))
	}

	req := &xfer.Message{Cards: []xfer.Card{&xfer.CloneCard{Version: 3, SeqNo: 1}}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	seqnoIdx, pushIdx := -1, -1
	for i, c := range resp.Cards {
		switch c.(type) {
		case *xfer.CloneSeqNoCard:
			seqnoIdx = i
		case *xfer.PushCard:
			pushIdx = i
		}
	}
	if seqnoIdx < 0 {
		t.Fatal("clone response missing clone_seqno card")
	}
	if pushIdx < 0 {
		t.Fatal("clone response missing push card")
	}
	if pushIdx < seqnoIdx {
		t.Fatalf("push card at index %d precedes clone_seqno at index %d; "+
			"a real fossil client re-clones the whole repo when push arrives "+
			"before the terminal clone_seqno (issue #138)", pushIdx, seqnoIdx)
	}
}

// similarCloneArtifacts returns two bodies sharing almost all their text, so
// a delta between them is well under content.Deltify's policy threshold and
// far smaller than either full body.
func similarCloneArtifacts() ([]byte, []byte) {
	var a, b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&a, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
		if i == 100 {
			fmt.Fprintf(&b, "line %04d: CHANGED FOR THE DELTA TEST\n", i)
		} else {
			fmt.Fprintf(&b, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
		}
	}
	return []byte(a.String()), []byte(b.String())
}

// cloneContentCard normalizes the two wire cards a clone uses to carry an
// artifact -- a compressed cfile (full content) and an uncompressed file
// (delta content) -- into the fields the #141 ordering tests reason about.
type cloneContentCard struct {
	uuid     string
	deltaSrc string
	content  []byte
}

// cloneContentCards flattens a clone response into the artifact-bearing cards
// in wire order. A clone emits full content as a cfile and delta content as an
// uncompressed file card (matching canonical fossil's send_delta_native, whose
// receive path re-frames the delta into fossil's on-disk blob format -- a
// cfile would be stored verbatim and fail to decompress on a real client).
func cloneContentCards(resp *xfer.Message) []cloneContentCard {
	var out []cloneContentCard
	for _, c := range resp.Cards {
		switch cf := c.(type) {
		case *xfer.CFileCard:
			out = append(out, cloneContentCard{cf.UUID, cf.DeltaSrc, cf.Content})
		case *xfer.FileCard:
			out = append(out, cloneContentCard{cf.UUID, cf.DeltaSrc, cf.Content})
		}
	}
	return out
}

// TestEmitCloneBatchSendsDeltifiedRowsAsDelta pins the #141 send behavior: a
// deltified row goes on the wire as a delta card (reclaiming the bandwidth the
// old expand-before-send path gave up), but its source is emitted first so the
// delta never forward-references a card that has not gone out yet.
//
// content.Deltify deltifies the OLDER artifact against the NEWER one, so the
// predecessor's DeltaSource is the (higher-rid) successor. emitCloneBatch must
// therefore emit the successor's full card BEFORE the predecessor's delta card,
// even though that is out of ascending-rid order. That ordering is a real
// invariant of the send path -- a delta must never forward-reference a card the
// receiver has not yet seen -- and TestEmitCloneBatchSourcePrecedesDelta guards
// it across a deeper chain. It is necessary but not sufficient for a real fossil
// 2.28 client, whose clone is still unusable because full content rides a
// compressed cfile that go-libfossil frames as bare zlib (separate, pre-existing
// bug #152); see TestCloneRealFossilWithDeltaChain, which skips against #152.
func TestEmitCloneBatchSendsDeltifiedRowsAsDelta(t *testing.T) {
	r := setupSyncTestRepo(t)

	predecessor, successor := similarCloneArtifacts()
	var oldUUID, newUUID string
	if err := r.WithTx(func(tx *db.Tx) error {
		oldRid, u1, err := blob.Store(tx, predecessor)
		if err != nil {
			return err
		}
		newRid, u2, err := blob.Store(tx, successor)
		if err != nil {
			return err
		}
		oldUUID, newUUID = u1, u2
		saved, err := content.Deltify(tx, oldRid, newRid)
		if err != nil {
			return err
		}
		if saved <= 0 {
			t.Fatalf("test setup: Deltify saved %d bytes, want > 0", saved)
		}
		return nil
	}); err != nil {
		t.Fatalf("deltify setup: %v", err)
	}

	req := &xfer.Message{Cards: []xfer.Card{&xfer.CloneCard{Version: 1}}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// Locate the predecessor delta and the successor source, and the positions
	// they occupy in wire order.
	var delta, source *cloneContentCard
	deltaPos, sourcePos := -1, -1
	for i, cc := range cloneContentCards(resp) {
		cc := cc
		switch cc.uuid {
		case oldUUID:
			delta, deltaPos = &cc, i
		case newUUID:
			source, sourcePos = &cc, i
		}
	}
	if delta == nil || source == nil {
		t.Fatalf("expected content cards for both predecessor and successor; got delta=%v source=%v",
			delta != nil, source != nil)
	}
	if delta.deltaSrc != newUUID {
		t.Errorf("predecessor DeltaSrc = %q, want %q (the successor it deltas against)",
			delta.deltaSrc, newUUID)
	}
	if source.deltaSrc != "" {
		t.Errorf("successor DeltaSrc = %q, want empty (it is the full-content chain tip)", source.deltaSrc)
	}
	if sourcePos > deltaPos {
		t.Errorf("source card at position %d must precede the delta card at position %d",
			sourcePos, deltaPos)
	}
	if len(delta.content) >= len(predecessor) {
		t.Errorf("delta payload = %d bytes, want < %d (the un-deltified original) -- a delta must be smaller",
			len(delta.content), len(predecessor))
	}
}

// TestEmitCloneBatchSourcePrecedesDelta asserts the airtight #141 invariant on
// a deeper corpus: every content card that carries a DeltaSrc is preceded, in
// wire order, by a card that supplies that source UUID. A delta whose source
// has not already been sent would create an unfillable phantom on a real fossil
// client, so this is a correctness guard, not a bandwidth one.
//
// Method: build six revisions of one file so content.Deltify produces a
// multi-link chain, clone in a single round, and walk the response cards
// tracking which UUIDs have been supplied so far.
func TestEmitCloneBatchSourcePrecedesDelta(t *testing.T) {
	r := setupSyncTestRepo(t)

	base := make([]byte, 4096)
	for i := range base {
		base[i] = byte(i % 251)
	}
	var parent libfossil.FslID
	for c := 0; c < 6; c++ {
		body := append([]byte(nil), base...)
		body[c] = byte(200 + c)
		body = append(body, []byte(fmt.Sprintf("\nrevision marker %d\n", c))...)
		mid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Comment: fmt.Sprintf("checkin %d", c),
			User:    "testuser",
			Parent:  parent,
			Files:   []manifest.File{{Name: "big.bin", Content: body}},
		})
		if err != nil {
			t.Fatalf("Checkin %d: %v", c, err)
		}
		parent = mid
	}
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	var deltaCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM delta").Scan(&deltaCount); err != nil {
		t.Fatalf("count deltas: %v", err)
	}
	if deltaCount == 0 {
		t.Fatal("fixture bug: no deltas created -- this test needs a deltified chain to be meaningful")
	}

	req := &xfer.Message{Cards: []xfer.Card{&xfer.CloneCard{Version: 3, SeqNo: 1}}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	sawDelta := false
	supplied := map[string]bool{}
	for _, cc := range cloneContentCards(resp) {
		if cc.deltaSrc != "" {
			sawDelta = true
			if !supplied[cc.deltaSrc] {
				t.Errorf("delta card %s names source %s that no earlier card supplied",
					cc.uuid, cc.deltaSrc)
			}
		}
		supplied[cc.uuid] = true
	}
	if !sawDelta {
		t.Error("expected at least one delta card in the clone of a deltified chain, got none")
	}
}

// cloneRound runs one clone round at the given cursor and returns the cfile
// cards it produced, their total payload size, and the cursor the server
// reported back. A zero cursor back means the snapshot is exhausted.
func cloneRound(t *testing.T, r *repo.Repo, seqno int) (files []*xfer.CFileCard, payload int, next int) {
	t.Helper()
	req := &xfer.Message{Cards: []xfer.Card{&xfer.CloneCard{Version: 3, SeqNo: seqno}}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("clone round at seqno %d: %v", seqno, err)
	}
	files = findCards[*xfer.CFileCard](resp)
	for _, f := range files {
		payload += len(f.Content)
	}
	seqnos := findCards[*xfer.CloneSeqNoCard](resp)
	if len(seqnos) != 1 {
		t.Fatalf("clone round at seqno %d: got %d clone_seqno cards, want 1", seqno, len(seqnos))
	}
	return files, payload, seqnos[0].SeqNo
}

// TestHandleClonePaginationBoundedByBytes proves the clone batch is bounded
// by output size and not by artifact count (issue #88). Method: store enough
// large artifacts that one round's payload must exceed the byte budget if the
// bound were a count, then check the first round stopped near the budget
// instead of at any fixed number of artifacts.
func TestHandleClonePaginationBoundedByBytes(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Each artifact is a fixed 1/8 of the budget, so a byte bound admits
	// 8 of them per round and a count bound would admit all 40.
	const artifactSize = DefaultCloneBatchBytes / 8
	const artifacts = 40
	for i := range artifacts {
		data := make([]byte, artifactSize)
		copy(data, fmt.Sprintf("large blob %d", i))
		storeTestBlob(t, r, data)
	}

	files, payload, next := cloneRound(t, r, 1)
	if next == 0 {
		t.Fatalf("round 1 drained the whole snapshot: %d files, %d payload bytes", len(files), payload)
	}
	// The artifact that crosses the budget is still sent whole, so one
	// artifact of overshoot is the contract, not a tolerance.
	if payload > DefaultCloneBatchBytes+artifactSize {
		t.Errorf("round 1 payload = %d bytes, want <= %d",
			payload, DefaultCloneBatchBytes+artifactSize)
	}
	if len(files) >= artifacts {
		t.Errorf("round 1 sent %d of %d artifacts; batch was not size-bounded", len(files), artifacts)
	}
}

// TestHandleClonePaginationNotCappedByCount proves a corpus of small
// artifacts is not chopped at a fixed count. 2,000 tiny artifacts are far
// below the byte budget, so the whole snapshot must drain in one round.
// Under the old 200-artifact bound this needed ten rounds, which is the
// arithmetic behind #88's ~20,000-artifact ceiling.
func TestHandleClonePaginationNotCappedByCount(t *testing.T) {
	r := setupSyncTestRepo(t)

	const artifacts = 2000
	for i := range artifacts {
		storeTestBlob(t, r, []byte(fmt.Sprintf("page blob %d", i)))
	}

	files, payload, next := cloneRound(t, r, 1)
	if payload >= DefaultCloneBatchBytes {
		t.Fatalf("test corpus is not below the budget: %d payload bytes", payload)
	}
	if next != 0 {
		t.Errorf("round 1 returned cursor %d after %d files; want 0 (exhausted)", next, len(files))
	}
	if len(files) < artifacts {
		t.Errorf("round 1 sent %d files, want all %d", len(files), artifacts)
	}
}

// TestHandleClonePaginationOversizedArtifact pins the liveness edge: a single
// artifact larger than the entire round budget must still be sent, or the
// cursor never advances past it and the clone stalls to MaxRounds.
func TestHandleClonePaginationOversizedArtifact(t *testing.T) {
	r := setupSyncTestRepo(t)

	oversized := make([]byte, DefaultCloneBatchBytes+1024)
	copy(oversized, "oversized blob")
	storeTestBlob(t, r, oversized)
	storeTestBlob(t, r, []byte("trailing blob"))

	files, payload, next := cloneRound(t, r, 1)
	if len(files) != 1 {
		t.Fatalf("round 1 sent %d files, want exactly the one oversized artifact", len(files))
	}
	if payload != len(oversized) {
		t.Errorf("round 1 payload = %d bytes, want %d", payload, len(oversized))
	}
	if next == 0 {
		t.Fatal("round 1 reported exhaustion but the trailing artifact was never sent")
	}

	files2, _, next2 := cloneRound(t, r, next)
	if len(files2) != 1 || next2 != 0 {
		t.Fatalf("round 2: %d files, cursor %d; want 1 file and exhaustion", len(files2), next2)
	}
}

// TestHandleClonePagination walks a two-round clone to completion, pinning
// the cursor round trip. The client echoes the server's cursor back on the
// clone card itself; clone_seqno is server-to-client only (issue #74).
func TestHandleClonePagination(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Two artifacts, each the size of the whole budget: the first fills it
	// and the second is owed to a following round.
	const artifactSize = DefaultCloneBatchBytes
	for i := range 2 {
		data := make([]byte, artifactSize)
		copy(data, fmt.Sprintf("paged blob %d", i))
		storeTestBlob(t, r, data)
	}

	files1, _, next := cloneRound(t, r, 1)
	if len(files1) != 1 {
		t.Fatalf("page 1: got %d files, want 1", len(files1))
	}
	if next == 0 {
		t.Fatal("page 1: expected a continuation cursor, got exhaustion")
	}

	files2, _, next2 := cloneRound(t, r, next)
	if len(files2) != 1 {
		t.Fatalf("page 2: got %d files, want 1", len(files2))
	}
	if next2 != 0 {
		t.Fatalf("page 2: expected clone_seqno 0 (completion), got %d", next2)
	}
}

func TestHandleReqConfig(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.ReqConfigCard{Name: "project-code"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	configs := findCards[*xfer.ConfigCard](resp)
	found := false
	for _, c := range configs {
		if c.Name == "project-code" && len(c.Content) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected config card for project-code")
	}
}

func TestHandlePushFileStoreFails(t *testing.T) {
	r := setupSyncTestRepo(t)
	// File with valid push but bad hash → storeReceivedFile returns error → ErrorCard
	badUUID := "cccccccccccccccccccccccccccccccccccccccc"
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: badUUID, Content: []byte("wrong hash content")},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) == 0 {
		t.Fatal("expected error card for bad hash")
	}
}

// TestHandlePushDeltaBeforeBaseDoesNotErrorCard is the push-path acceptance
// test for issue #57 (folded into #53): a client pushing a delta whose base
// the server has never seen is normal transfer steady state, not a fault.
// The server must store it (see storeDeltaAgainstPhantomBase) and continue,
// rather than emitting an ErrorCard.
func TestHandlePushDeltaBeforeBaseDoesNotErrorCard(t *testing.T) {
	r := setupSyncTestRepo(t)

	base := []byte("push delta-before-base test, base content long enough to compress")
	target := []byte("push delta-before-base test, base content long enough to compress, v2")
	deltaBytes := delta.Create(base, target)
	baseUUID := hash.SHA1(base)
	targetUUID := hash.SHA1(target)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.CFileCard{UUID: targetUUID, DeltaSrc: baseUUID, Content: deltaBytes},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	if errs := findCards[*xfer.ErrorCard](resp); len(errs) != 0 {
		t.Fatalf("unexpected error card(s) for delta-before-base push: %v", errs)
	}

	targetRid, ok := blob.Exists(r.DB(), targetUUID)
	if !ok {
		t.Fatal("target blob missing after push")
	}
	var size int64
	r.DB().QueryRow("SELECT size FROM blob WHERE rid=?", targetRid).Scan(&size)
	if size < 0 {
		t.Fatalf("target size = %d, want >= 0 (target must not be phantomized)", size)
	}

	if _, available := content.AvailableByUUID(r.DB(), targetUUID); available {
		t.Fatal("target reported available before its base ever arrived")
	}
}

// alwaysAtSite is a BuggifyChecker that fires only for one named site.
type alwaysAtSite string

func (s alwaysAtSite) Check(site string, _ float64) bool { return string(s) == site }

// TestHandleFileMissingAfterStore exercises the regression path from
// issue #14: blob.Exists returning ok=false after a successful
// storeReceivedFile must not reach content.MakePublic / MakePrivate
// (which panic on rid <= 0). The handler should emit an ErrorCard
// and continue processing.
func TestHandleFileMissingAfterStore(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("payload that stores cleanly")
	uuid := hash.SHA1(data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	opts := HandleOpts{Buggify: alwaysAtSite("handler.handleFile.missingAfterStore")}
	resp, err := HandleSyncWithOpts(context.Background(), r, req, opts)
	if err != nil {
		t.Fatalf("HandleSyncWithOpts: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) != 1 {
		t.Fatalf("error cards = %d, want 1", len(errs))
	}
	if want := "missing after store"; !strings.Contains(errs[0].Message, want) {
		t.Fatalf("error card message = %q, want containing %q", errs[0].Message, want)
	}
}

func TestHandleCFileCard(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("cfile content")
	uuid := hash.SHA1(data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.CFileCard{UUID: uuid, Content: data, USize: len(data)},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}
	_, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatal("cfile blob not stored")
	}
}

func TestHandleLoginAndPragma(t *testing.T) {
	r := setupSyncTestRepo(t)
	storeTestBlob(t, r, []byte("login pragma test"))

	// Pragma cards should be accepted (no login needed for pragma processing)
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "client-version", Values: []string{"22800"}},
		&xfer.PragmaCard{Name: "unknown-pragma", Values: []string{"ignored"}},
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}
	igots := findCards[*xfer.IGotCard](resp)
	if len(igots) == 0 {
		t.Fatal("expected igot cards after pragma+pull")
	}
}

// TestHandleCloneCursorPastEnd checks that a cursor beyond the last rid
// yields no files. It no longer sends a clone_seqno card — the cursor rides
// on the clone card — so the old name no longer described it.
func TestHandleCloneCursorPastEnd(t *testing.T) {
	r := setupSyncTestRepo(t)
	// Store blobs, then clone with a seqno that skips them
	for i := range 3 {
		data := []byte(fmt.Sprintf("seqno test %d", i))
		storeTestBlob(t, r, data)
	}

	// Get all blobs to find the max rid
	req1 := &xfer.Message{Cards: []xfer.Card{&xfer.CloneCard{Version: 1}}}
	resp1, _ := HandleSync(context.Background(), r, req1)
	files1 := findCards[*xfer.CFileCard](resp1)

	// Now clone with seqno past all blobs — should get nothing
	req2 := &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1, SeqNo: 9999},
	}}
	resp2, err := HandleSync(context.Background(), r, req2)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	files2 := findCards[*xfer.CFileCard](resp2)
	if len(files2) > 0 {
		t.Fatalf("expected no files with high seqno, got %d", len(files2))
	}

	_ = files1 // used for context
}

func TestHandleEmptyRequest(t *testing.T) {
	r := setupSyncTestRepo(t)
	req := &xfer.Message{Cards: nil}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if len(resp.Cards) != 0 {
		t.Fatalf("expected empty response for empty request, got %d cards", len(resp.Cards))
	}
}

func TestHandleReqConfigMissing(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.ReqConfigCard{Name: "nonexistent-config"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	configs := findCards[*xfer.ConfigCard](resp)
	if len(configs) > 0 {
		t.Fatal("should not return config for nonexistent key")
	}
}

// TestHandleSyncNoSpuriousGimmeForReceivedFile verifies that HandleSync
// does NOT emit a GimmeCard for a blob that was delivered as a FileCard
// in the same request. Regression test for the igot-before-file bug:
// if IGotCard is processed before FileCard, blob.Exists returns false
// and a spurious GimmeCard is emitted, causing infinite sync loops.
func TestHandleSyncNoSpuriousGimmeForReceivedFile(t *testing.T) {
	r := setupSyncTestRepo(t)
	defer r.Close()

	// Create a blob to push to the handler.
	content := []byte("test content for spurious gimme check")
	uuid := hash.SHA1(content)

	// Build a request with BOTH IGotCard and FileCard for the same blob.
	// This mimics what a sync client sends when pushing a new blob:
	// it announces via igot AND delivers the file in the same round.
	req := &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PushCard{
				ServerCode:  "test-server",
				ProjectCode: "test-project",
			},
			&xfer.PullCard{
				ServerCode:  "test-server",
				ProjectCode: "test-project",
			},
			&xfer.IGotCard{UUID: uuid},
			&xfer.FileCard{UUID: uuid, Content: content},
		},
	}

	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// Check response for spurious gimme.
	for _, card := range resp.Cards {
		if g, ok := card.(*xfer.GimmeCard); ok && g.UUID == uuid {
			t.Errorf("HandleSync emitted GimmeCard for %s which was delivered as FileCard in the same request — this causes infinite sync loops", uuid[:12])
		}
	}

	// Verify the blob was actually stored.
	if _, exists := blob.Exists(r.DB(), uuid); !exists {
		t.Errorf("blob %s was not stored by HandleSync", uuid[:12])
	}

	// Server should NOT emit igot for this blob because the client
	// announced it via igot — remoteHas filtering suppresses it.
	for _, card := range resp.Cards {
		if ig, ok := card.(*xfer.IGotCard); ok && ig.UUID == uuid {
			t.Errorf("HandleSync should not emit IGotCard for %s — client already announced it via igot", uuid[:12])
		}
	}
}

func TestHandlerEmitsPushCardOnClone(t *testing.T) {
	r := setupSyncTestRepo(t)

	var projectCode, serverCode string
	r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projectCode)
	r.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&serverCode)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	pushCards := findCards[*xfer.PushCard](resp)
	if len(pushCards) != 1 {
		t.Fatalf("PushCard count = %d, want 1", len(pushCards))
	}
	if pushCards[0].ProjectCode != projectCode {
		t.Errorf("ProjectCode = %q, want %q", pushCards[0].ProjectCode, projectCode)
	}
	if pushCards[0].ServerCode != serverCode {
		t.Errorf("ServerCode = %q, want %q", pushCards[0].ServerCode, serverCode)
	}
}

func TestHandlerNoPushCardOnPull(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// Server must NOT emit PushCard on sync/pull — real Fossil treats
	// server-sent "push" as unknown command during sync.
	pushCards := findCards[*xfer.PushCard](resp)
	if len(pushCards) != 0 {
		t.Fatalf("PushCard count = %d, want 0 (push is clone-only)", len(pushCards))
	}
}

// TestEmitIGots_OnlyUnclustered verifies that after clustering, emitIGots
// returns only unclustered entries (not all blobs in the repo).
func TestEmitIGots_AllBlobs(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 200 blobs — above ClusterThreshold (100).
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("blob-%04d", i))
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
	}

	// Pre-cluster so we have known state before the handler runs.
	n, err := content.GenerateClusters(r.DB())
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least 1 cluster to be created")
	}

	// Send a pull request — handler emits igots for ALL non-phantom blobs.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
	)

	igots := cardsByType(resp, xfer.CardIGot)

	var totalBlobs int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&totalBlobs)

	if len(igots) != totalBlobs {
		t.Fatalf("igots = %d, total blobs = %d; emitIGots should send all blobs",
			len(igots), totalBlobs)
	}
}

// TestPragmaReqClusters verifies that pragma req-clusters causes the handler
// to emit igot cards for cluster artifacts via sendAllClusters.
func TestPragmaReqClusters(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 200 blobs — above ClusterThreshold (100).
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("cluster-blob-%04d", i))
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
	}

	// Pre-cluster so we have known state.
	n, err := content.GenerateClusters(r.DB())
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 1 {
		t.Fatalf("clusters = %d, want 1", n)
	}

	resp, err := HandleSync(context.Background(), r, &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
			&xfer.PragmaCard{Name: "req-clusters"},
		},
	})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	igots := cardsByType(resp, xfer.CardIGot)

	// emitIGots sends all blobs; sendAllClusters may add cluster igots
	// (deduplication happens client-side). Total should include all blobs.
	var totalBlobs int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&totalBlobs)

	if len(igots) < totalBlobs {
		t.Fatalf("igots = %d, total blobs = %d; should include at least all blobs",
			len(igots), totalBlobs)
	}
}

// TestPragmaReqClusters_OldClusters verifies that all blobs are advertised
// even when the unclustered table is empty (all blobs have been clustered).
func TestPragmaReqClusters_OldClusters(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 200 blobs and cluster them.
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("old-cluster-blob-%04d", i))
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
	}

	n, err := content.GenerateClusters(r.DB())
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 1 {
		t.Fatalf("first pass clusters = %d, want 1", n)
	}

	// Manually remove everything from unclustered to simulate old clusters
	// that have been clustered in a future pass.
	if _, err := r.DB().Exec("DELETE FROM unclustered"); err != nil {
		t.Fatalf("clearing unclustered: %v", err)
	}

	resp, err := HandleSync(context.Background(), r, &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
			&xfer.PragmaCard{Name: "req-clusters"},
		},
	})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	igots := cardsByType(resp, xfer.CardIGot)

	// emitIGots sends ALL blobs regardless of unclustered status.
	// sendAllClusters adds cluster igots (may duplicate).
	var totalBlobs int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&totalBlobs)

	if len(igots) < totalBlobs {
		t.Fatalf("igots = %d, total blobs = %d; should include all blobs",
			len(igots), totalBlobs)
	}
}

func TestHandlePushRequiresAuth(t *testing.T) {
	r := setupSyncTestRepo(t)
	// Delete nobody so anonymous push is rejected
	r.DB().Exec("DELETE FROM user WHERE login='nobody'")

	data := []byte("auth test")
	uuid := hash.SHA1(data)
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) == 0 {
		t.Fatal("expected error for unauthorized push")
	}
	if _, ok := blob.Exists(r.DB(), uuid); ok {
		t.Fatal("blob should not be stored without push capability")
	}
}

func TestHandlePullRequiresAuth(t *testing.T) {
	r := setupSyncTestRepo(t)
	storeTestBlob(t, r, []byte("pull auth test"))
	// Delete nobody so anonymous pull is rejected
	r.DB().Exec("DELETE FROM user WHERE login='nobody'")

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) == 0 {
		t.Fatal("expected error for unauthorized pull")
	}
	igots := findCards[*xfer.IGotCard](resp)
	if len(igots) > 0 {
		t.Fatal("should not emit igots without pull capability")
	}
}

func TestHandleAuthenticatedPush(t *testing.T) {
	r := setupSyncTestRepo(t)
	pc := testProjectCode(t, r.DB())
	// Delete nobody, create a user with push caps
	r.DB().Exec("DELETE FROM user WHERE login='nobody'")
	auth.CreateUser(r.DB(), pc, "pusher", "secret", "oi")

	data := []byte("authed push")
	uuid := hash.SHA1(data)

	// Build a valid login card — nonce is SHA1 of the non-login card payload
	loginCard := buildTestLoginCard("pusher", "secret", pc, []byte("dummy"))

	req := &xfer.Message{Cards: []xfer.Card{
		loginCard,
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}
	if _, ok := blob.Exists(r.DB(), uuid); !ok {
		t.Fatal("authenticated push should store blob")
	}
}

func TestHandleNobodyPullOnly(t *testing.T) {
	r := setupSyncTestRepo(t)
	storeTestBlob(t, r, []byte("nobody test"))
	// Set nobody to pull-only
	r.DB().Exec("UPDATE user SET cap='o' WHERE login='nobody'")

	// Pull should work
	pullReq := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	pullResp, _ := HandleSync(context.Background(), r, pullReq)
	igots := findCards[*xfer.IGotCard](pullResp)
	if len(igots) == 0 {
		t.Fatal("nobody with 'o' cap should allow pull")
	}

	// Push should fail
	data := []byte("nobody push attempt")
	uuid := hash.SHA1(data)
	pushReq := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	pushResp, _ := HandleSync(context.Background(), r, pushReq)
	errs := findCards[*xfer.ErrorCard](pushResp)
	if len(errs) == 0 {
		t.Fatal("nobody with 'o' cap should reject push")
	}
}

func TestEmitIGots_ExcludesShunAndPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)

	_, normalUUID, err := blob.Store(r.DB(), []byte("normal-blob-content"))
	if err != nil {
		t.Fatalf("Store normal: %v", err)
	}
	_, shunnedUUID, err := blob.Store(r.DB(), []byte("shunned-blob-content"))
	if err != nil {
		t.Fatalf("Store shunned: %v", err)
	}
	privRid, _, err := blob.Store(r.DB(), []byte("private-blob-content"))
	if err != nil {
		t.Fatalf("Store private: %v", err)
	}

	if _, err := r.DB().Exec("INSERT INTO shun(uuid, mtime) VALUES(?, 0)", shunnedUUID); err != nil {
		t.Fatalf("shun: %v", err)
	}
	if _, err := r.DB().Exec("INSERT INTO private(rid) VALUES(?)", privRid); err != nil {
		t.Fatalf("private: %v", err)
	}

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	igotUUIDs := make(map[string]bool)
	for _, c := range resp.Cards {
		if ig, ok := c.(*xfer.IGotCard); ok {
			igotUUIDs[ig.UUID] = true
		}
	}

	if !igotUUIDs[normalUUID] {
		t.Error("normal blob missing from igots")
	}

	if igotUUIDs[shunnedUUID] {
		t.Error("shunned blob appeared in igots")
	}

	var privUUID string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", privRid).Scan(&privUUID); err != nil {
		t.Fatalf("query privUUID: %v", err)
	}
	if igotUUIDs[privUUID] {
		t.Error("private blob appeared in igots")
	}
}

func TestHandlerPragmaSendPrivate_Accepted(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
	)
	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			if e.Message == "not authorized to sync private content" {
				t.Error("should not get auth error with 'x' capability")
			}
		}
	}
}

func TestHandlerPragmaSendPrivate_Rejected(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oi' WHERE login='nobody'")
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
	)
	errors := findCards[*xfer.ErrorCard](resp)
	found := false
	for _, e := range errors {
		if e.Message == "not authorized to sync private content" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'not authorized to sync private content' error")
	}
}

func TestHandlerPrivateCardAccepted(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")
	data := []byte("private blob data")
	uuid := hash.SHA1(data)

	resp := handleReq(t, r,
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PrivateCard{},
		&xfer.FileCard{UUID: uuid, Content: data},
	)

	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			t.Errorf("unexpected error: %s", e.Message)
		}
	}

	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatal("blob not stored")
	}
	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Error("blob should be marked private")
	}
}

func TestHandlerPrivateCardRejected(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oi' WHERE login='nobody'")
	data := []byte("private blob rejected")
	uuid := hash.SHA1(data)

	resp := handleReq(t, r,
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PrivateCard{},
		&xfer.FileCard{UUID: uuid, Content: data},
	)

	errors := findCards[*xfer.ErrorCard](resp)
	found := false
	for _, e := range errors {
		if e.Message == "not authorized to sync private content" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'not authorized to sync private content' error")
	}
}

func TestHandlerPublicFileClearsPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("was private now public")
	uuid := hash.SHA1(data)

	// Pre-store as private.
	storeReceivedFile(r, uuid, "", data, nil)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))
	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Fatal("precondition: blob should be private")
	}

	// Push same blob WITHOUT private card — should clear private.
	resp := handleReq(t, r,
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.FileCard{UUID: uuid, Content: data},
	)

	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			t.Errorf("unexpected error: %s", e.Message)
		}
	}

	if content.IsPrivate(r.DB(), int64(rid)) {
		t.Error("blob should no longer be private after public file push")
	}
}

func TestEmitIGotsExcludesPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)
	pubUUID := storeTestBlob(t, r, []byte("public blob"))
	privData := []byte("private blob for exclusion")
	privUUID := storeTestBlob(t, r, privData)
	privRid, _ := blob.Exists(r.DB(), privUUID)
	content.MakePrivate(r.DB(), int64(privRid))

	// Pull without send-private pragma — private blob should be excluded.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
	)

	igotUUIDs := make(map[string]bool)
	for _, c := range resp.Cards {
		if ig, ok := c.(*xfer.IGotCard); ok {
			igotUUIDs[ig.UUID] = true
		}
	}
	if !igotUUIDs[pubUUID] {
		t.Error("public blob missing from igots")
	}
	if igotUUIDs[privUUID] {
		t.Error("private blob should be excluded from igots without send-private")
	}
}

func TestEmitIGotsIncludesPrivateWhenAuthorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")

	pubUUID := storeTestBlob(t, r, []byte("public blob auth"))
	privData := []byte("private blob auth")
	privUUID := storeTestBlob(t, r, privData)
	privRid, _ := blob.Exists(r.DB(), privUUID)
	content.MakePrivate(r.DB(), int64(privRid))

	// Pull WITH send-private pragma and x capability.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
	)

	pubFound := false
	privFound := false
	for _, c := range resp.Cards {
		if ig, ok := c.(*xfer.IGotCard); ok {
			if ig.UUID == pubUUID {
				pubFound = true
			}
			if ig.UUID == privUUID {
				if !ig.IsPrivate {
					t.Error("private blob igot should have IsPrivate=true")
				}
				privFound = true
			}
		}
	}
	if !pubFound {
		t.Error("public blob missing from igots")
	}
	if !privFound {
		t.Error("private blob should be included when send-private is authorized")
	}
}

func TestHandlerIGotPrivate_Authorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")
	unknownUUID := "dddddddddddddddddddddddddddddddddddddd"

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
		&xfer.IGotCard{UUID: unknownUUID, IsPrivate: true},
	)

	gimmes := findCards[*xfer.GimmeCard](resp)
	found := false
	for _, g := range gimmes {
		if g.UUID == unknownUUID {
			found = true
		}
	}
	if !found {
		t.Error("authorized private igot should produce gimme")
	}
}

func TestHandlerIGotPrivate_Unauthorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oi' WHERE login='nobody'")
	unknownUUID := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.IGotCard{UUID: unknownUUID, IsPrivate: true},
	)

	gimmes := findCards[*xfer.GimmeCard](resp)
	for _, g := range gimmes {
		if g.UUID == unknownUUID {
			t.Error("unauthorized private igot should NOT produce gimme")
		}
	}
}

func TestHandlerIGotDoesNotChangeServerPrivateStatus(t *testing.T) {
	r := setupSyncTestRepo(t)
	// Store a blob and mark it private.
	data := []byte("igot does not change server private status")
	uuid := storeTestBlob(t, r, data)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))
	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Fatal("precondition: blob should be private")
	}

	// Client sends a public igot for the existing blob.
	// Server is authoritative — this should NOT change the server's private status.
	handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.IGotCard{UUID: uuid, IsPrivate: false},
	)

	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Error("server private status should not be changed by client igot")
	}
}

func TestHandleGimmePrivate_Authorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")
	data := []byte("gimme private blob")
	uuid := storeTestBlob(t, r, data)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
		&xfer.GimmeCard{UUID: uuid},
	)

	// Should get PrivateCard followed by FileCard.
	files := findCards[*xfer.FileCard](resp)
	found := false
	for _, f := range files {
		if f.UUID == uuid {
			found = true
		}
	}
	if !found {
		t.Error("authorized gimme for private blob should return file")
	}
	privCards := findCards[*xfer.PrivateCard](resp)
	if len(privCards) == 0 {
		t.Error("expected PrivateCard prefix before private file")
	}
}

func TestHandleGimmePrivate_Unauthorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oi' WHERE login='nobody'")
	data := []byte("gimme private unauthorized")
	uuid := storeTestBlob(t, r, data)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.GimmeCard{UUID: uuid},
	)

	files := findCards[*xfer.FileCard](resp)
	for _, f := range files {
		if f.UUID == uuid {
			t.Error("unauthorized gimme for private blob should NOT return file")
		}
	}
}

func TestEmitCloneBatchSkipsPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)
	pubData := []byte("clone public blob")
	pubUUID := storeTestBlob(t, r, pubData)
	privData := []byte("clone private blob")
	privUUID := storeTestBlob(t, r, privData)
	privRid, _ := blob.Exists(r.DB(), privUUID)
	content.MakePrivate(r.DB(), int64(privRid))

	resp := handleReq(t, r,
		&xfer.CloneCard{Version: 1},
	)

	files := findCards[*xfer.CFileCard](resp)
	fileUUIDs := make(map[string]bool)
	for _, f := range files {
		fileUUIDs[f.UUID] = true
	}
	if !fileUUIDs[pubUUID] {
		t.Error("public blob missing from clone batch")
	}
	if fileUUIDs[privUUID] {
		t.Error("private blob should be excluded from clone batch without send-private")
	}
}

func TestEmitCloneBatchIncludesPrivateWhenAuthorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='gx' WHERE login='nobody'")
	pubData := []byte("clone pub auth")
	pubUUID := storeTestBlob(t, r, pubData)
	privData := []byte("clone priv auth")
	privUUID := storeTestBlob(t, r, privData)
	privRid, _ := blob.Exists(r.DB(), privUUID)
	content.MakePrivate(r.DB(), int64(privRid))

	resp := handleReq(t, r,
		&xfer.CloneCard{Version: 1},
		&xfer.PragmaCard{Name: "send-private"},
	)

	files := findCards[*xfer.CFileCard](resp)
	fileUUIDs := make(map[string]bool)
	for _, f := range files {
		fileUUIDs[f.UUID] = true
	}
	if !fileUUIDs[pubUUID] {
		t.Error("public blob missing from clone batch")
	}
	if !fileUUIDs[privUUID] {
		t.Error("private blob should be included when send-private authorized")
	}
	privCards := findCards[*xfer.PrivateCard](resp)
	if len(privCards) == 0 {
		t.Error("expected PrivateCard prefix for private blob in clone batch")
	}
}

func TestHandlerIGotFiltersEmit(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 3 blobs with known UUIDs.
	data1 := []byte("igot-filter-blob-one")
	data2 := []byte("igot-filter-blob-two")
	data3 := []byte("igot-filter-blob-three")
	uuid1 := storeTestBlob(t, r, data1)
	uuid2 := storeTestBlob(t, r, data2)
	uuid3 := storeTestBlob(t, r, data3)

	// Client announces it already has blob 1 and blob 2.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.IGotCard{UUID: uuid1},
		&xfer.IGotCard{UUID: uuid2},
	)

	igots := findCards[*xfer.IGotCard](resp)
	igotUUIDs := make(map[string]bool)
	for _, ig := range igots {
		igotUUIDs[ig.UUID] = true
	}

	// Server should NOT echo back blobs the client already has.
	if igotUUIDs[uuid1] {
		t.Errorf("server should not emit igot for uuid1 (%s) — client already has it", uuid1)
	}
	if igotUUIDs[uuid2] {
		t.Errorf("server should not emit igot for uuid2 (%s) — client already has it", uuid2)
	}

	// Server SHOULD emit igot for blob 3, which the client didn't announce.
	if !igotUUIDs[uuid3] {
		t.Errorf("server should emit igot for uuid3 (%s) — client doesn't have it", uuid3)
	}
}

func TestSendAllClustersExcludesPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 200 blobs so clustering triggers.
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("cluster-priv-blob-%04d", i))
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
	}

	n, err := content.GenerateClusters(r.DB())
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least 1 cluster")
	}

	// Mark the cluster artifact itself as private.
	var clusterRid int
	err = r.DB().QueryRow(`
		SELECT tx.rid FROM tagxref tx
		WHERE tx.tagid = 7
		LIMIT 1`,
	).Scan(&clusterRid)
	if err != nil {
		t.Fatalf("find cluster rid: %v", err)
	}
	content.MakePrivate(r.DB(), int64(clusterRid))

	resp, err := HandleSync(context.Background(), r, &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
			&xfer.PragmaCard{Name: "req-clusters"},
		},
	})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// The private cluster should not appear in igots.
	var clusterUUID string
	r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", clusterRid).Scan(&clusterUUID)

	for _, c := range resp.Cards {
		if ig, ok := c.(*xfer.IGotCard); ok && ig.UUID == clusterUUID {
			t.Error("private cluster blob should be excluded from sendAllClusters")
		}
	}
}

func TestHandleGimmeWithContentCache(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("cached gimme blob")
	uuid := storeTestBlob(t, r, data)

	cache := content.NewCache(1 << 20)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.GimmeCard{UUID: uuid},
	}}
	resp, err := HandleSyncWithOpts(context.Background(), r, req, HandleOpts{
		ContentCache: cache,
	})
	if err != nil {
		t.Fatalf("HandleSyncWithOpts: %v", err)
	}

	files := findCards[*xfer.FileCard](resp)
	found := false
	for _, f := range files {
		if f.UUID == uuid && string(f.Content) == string(data) {
			found = true
		}
	}
	if !found {
		t.Fatal("expected file card with correct content via cached handler")
	}

	// Cache should have been populated.
	stats := cache.Stats()
	if stats.Misses == 0 {
		t.Fatal("expected at least 1 cache miss (the initial expand)")
	}

	// Second request for the same blob should hit cache.
	resp2, err := HandleSyncWithOpts(context.Background(), r, req, HandleOpts{
		ContentCache: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	files2 := findCards[*xfer.FileCard](resp2)
	found2 := false
	for _, f := range files2 {
		if f.UUID == uuid && string(f.Content) == string(data) {
			found2 = true
		}
	}
	if !found2 {
		t.Fatal("expected file card on second request (cache hit path)")
	}

	stats2 := cache.Stats()
	if stats2.Hits == 0 {
		t.Fatal("expected cache hit on second handler call")
	}
}

func TestSyncRoundTripWithContentCache(t *testing.T) {
	server := setupSyncTestRepo(t)
	client := setupSyncTestRepo(t)

	// Store a blob on the server.
	data := []byte("sync round trip cached data")
	serverUUID := storeTestBlob(t, server, data)

	cache := content.NewCache(1 << 20)

	transport := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, err := HandleSyncWithOpts(context.Background(), server, req, HandleOpts{
				ContentCache: cache,
			})
			if err != nil {
				t.Fatalf("HandleSyncWithOpts: %v", err)
			}
			return resp
		},
	}

	result, err := Sync(context.Background(), client, transport, SyncOpts{
		Pull:         true,
		ContentCache: cache,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.FilesRecvd == 0 {
		t.Fatal("expected to receive files")
	}

	// Verify client has the blob.
	rid, ok := blob.Exists(client.DB(), serverUUID)
	if !ok {
		t.Fatal("blob not found on client after sync")
	}
	got, err := content.Expand(client.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}

	// Cache should have recorded activity.
	stats := cache.Stats()
	if stats.Hits+stats.Misses == 0 {
		t.Fatal("cache was never used during sync")
	}
}

func TestHandlerIGotFiltersPrivateEmit(t *testing.T) {
	r := setupSyncTestRepo(t)
	// Grant private sync capability.
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")

	// Store a blob and mark it private.
	data := []byte("igot-filter-private-blob")
	uuid := storeTestBlob(t, r, data)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))

	// Store a second private blob the client does NOT have.
	data2 := []byte("igot-filter-private-blob-two")
	uuid2 := storeTestBlob(t, r, data2)
	rid2, _ := blob.Exists(r.DB(), uuid2)
	content.MakePrivate(r.DB(), int64(rid2))

	// Client announces it has the first private blob.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
		&xfer.IGotCard{UUID: uuid, IsPrivate: true},
	)

	igots := findCards[*xfer.IGotCard](resp)
	for _, ig := range igots {
		if ig.UUID == uuid {
			t.Errorf("server should not emit private igot for %s — client already has it", uuid)
		}
	}

	// The second private blob should still appear.
	found := false
	for _, ig := range igots {
		if ig.UUID == uuid2 && ig.IsPrivate {
			found = true
		}
	}
	if !found {
		t.Errorf("server should emit private igot for %s — client doesn't have it", uuid2)
	}
}

// TestHandleCloneZeroSeqNoIsFatal pins §8.1: an otherwise authorized clone
// request with exactly three tokens, VERSION >= 2, and a digit-only SEQNO of
// zero or less clears accumulated output, emits `invalid clone sequence
// number`, and parses no later request card. Before the presence flag existed
// the server could not see this case at all — `clone` and `clone 3 0` both
// decoded to SeqNo 0 — and it silently clamped the cursor to 1 instead.
func TestHandleCloneZeroSeqNoIsFatal(t *testing.T) {
	r := setupSyncTestRepo(t)
	for i := range 3 {
		storeTestBlob(t, r, []byte(fmt.Sprintf("fatal seqno %d", i)))
	}

	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 3, SeqNo: 0, SeqNoIsDecimal: true},
		&xfer.ReqConfigCard{Name: "project-code"}, // must not be parsed
	}})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) != 1 || errs[0].Message != "invalid clone sequence number" {
		t.Fatalf("error cards = %v, want exactly [invalid clone sequence number]", errs)
	}
	// Accumulated output is cleared: nothing but the error survives.
	if len(resp.Cards) != 1 {
		t.Errorf("resp has %d cards, want 1 (output must be cleared)", len(resp.Cards))
	}
	if n := len(findCards[*xfer.CFileCard](resp)); n != 0 {
		t.Errorf("sent %d cfile cards on a fatal clone request, want 0", n)
	}
	if n := len(findCards[*xfer.ConfigCard](resp)); n != 0 {
		t.Errorf("later reqconfig card was parsed; §8.1 forbids it")
	}
}

// TestHandleCloneNonFatalSeqNoForms pins the cases §8.1 explicitly withholds
// the fatal from: wrong arity, a VERSION below 2, and a SEQNO that fails
// digit-only recognition. Each must clone normally from the first rid.
func TestHandleCloneNonFatalSeqNoForms(t *testing.T) {
	tests := []struct {
		name string
		card *xfer.CloneCard
	}{
		{"bare clone", &xfer.CloneCard{}},
		{"version below 2", &xfer.CloneCard{Version: 1, SeqNo: 0, SeqNoIsDecimal: true}},
		{"non-digit seqno", &xfer.CloneCard{Version: 3, SeqNo: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupSyncTestRepo(t)
			storeTestBlob(t, r, []byte("non-fatal clone form "+tt.name))

			resp, err := HandleSync(context.Background(), r, &xfer.Message{
				Cards: []xfer.Card{tt.card},
			})
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			for _, e := range findCards[*xfer.ErrorCard](resp) {
				if e.Message == "invalid clone sequence number" {
					t.Fatalf("§8.1 fatal applied to a case it excludes")
				}
			}
			if n := len(findCards[*xfer.CFileCard](resp)); n == 0 {
				t.Errorf("no files sent; request should clone from the first rid")
			}
		})
	}
}

// alwaysAtSites is a BuggifyChecker that fires for any of several named sites.
type alwaysAtSites []string

func (s alwaysAtSites) Check(site string, _ float64) bool {
	for _, want := range s {
		if want == site {
			return true
		}
	}
	return false
}

// TestEmitCloneBatchCursorAlwaysAdvances pins the liveness invariant behind
// emitCloneBatch's `count > 1` truncate guard. The cursor is inclusive now
// (`rid >= cursor`), so dropping the only card in a batch would report
// nextRID == lastSentRID == the cursor we entered with, and the client would
// re-request the same batch until MaxRounds. Under the old exclusive
// semantics `count > 0` was safe, which is why this is a constraint rather
// than a chaos-injection detail.
//
// smallBatch shrinks the round budget to one byte, which yields exactly
// one artifact per round, so every batch is the count == 1 case, and
// truncate is forced alongside it so the guard is the only thing preventing
// a non-advancing cursor. Each round must strictly advance or report 0.
func TestEmitCloneBatchCursorAlwaysAdvances(t *testing.T) {
	r := setupSyncTestRepo(t)
	for i := range 5 {
		storeTestBlob(t, r, []byte(fmt.Sprintf("advance %d", i)))
	}

	bug := alwaysAtSites{
		"handler.emitCloneBatch.smallBatch",
		"clone.emitCloneBatch.truncate",
	}

	cursor := 1
	for round := 0; round < 50; round++ {
		resp, err := HandleSyncWithOpts(context.Background(), r, &xfer.Message{
			Cards: []xfer.Card{&xfer.CloneCard{Version: 3, SeqNo: cursor, SeqNoIsDecimal: true}},
		}, HandleOpts{Buggify: bug})
		if err != nil {
			t.Fatalf("round %d (cursor %d): %v", round, cursor, err)
		}

		seqnos := findCards[*xfer.CloneSeqNoCard](resp)
		if len(seqnos) != 1 {
			t.Fatalf("round %d: got %d clone_seqno cards, want 1", round, len(seqnos))
		}
		next := seqnos[0].SeqNo
		if next == 0 {
			return // Exhausted, as expected.
		}
		if next <= cursor {
			t.Fatalf("round %d: cursor did not advance (%d -> %d); clone would stall to MaxRounds",
				round, cursor, next)
		}
		cursor = next
	}
	t.Fatalf("cursor never reached exhaustion in 50 rounds (last %d)", cursor)
}
