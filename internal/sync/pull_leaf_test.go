package sync

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// handlerTransport answers each round with the go-libfossil handler serving
// src, in process.
func handlerTransport(t *testing.T, src *repo.Repo) *MockTransport {
	t.Helper()
	return &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), src, req)
		if err != nil {
			t.Errorf("HandleSync: %v", err)
			return &xfer.Message{}
		}
		return resp
	}}
}

// TestPullRepairsLeavesOfCascadeLinks pulls a check-in onto a parent the
// puller already holds. It arrives as an igot phantom filled the next round,
// so the phantom-fill cascade links it and the session's closing sweep finds
// nothing left to link. The leaf and tag-propagation repair must still run
// once for what the cascade linked (repo-state-01 Section 7.3, RS-7.3-3 and
// RS-7.3-4): the child replaces its parent as the leaf, and the parent's
// propagated trunk tag reaches it.
func TestPullRepairsLeavesOfCascadeLinks(t *testing.T) {
	content := []byte("pulled file\n")
	when := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dst := setupSyncTestRepo(t)
	fileUUID := storeAndLink(t, dst, content)
	rootBytes, _ := buildCheckinManifest(t, "root", when, fileUUID, content, nil)
	rootUUID := storeAndLink(t, dst, rootBytes)
	childBytes := buildChildManifest(t, when.Add(time.Minute), fileUUID, content, rootUUID)
	src := setupSyncTestRepo(t)
	storeAndLink(t, src, content, rootBytes)
	childUUID := storeAndLink(t, src, childBytes)

	res, err := Sync(context.Background(), dst, handlerTransport(t, src),
		SyncOpts{Pull: true, ProjectCode: testPC})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.ArtifactsLinked != 0 {
		t.Fatalf("closing sweep linked %d; the case needs the cascade to link the child",
			res.ArtifactsLinked)
	}
	if got := leafUUIDs(t, dst); len(got) != 1 || got[0] != childUUID {
		t.Fatalf("leaf rows = %v, want only the child %s", got, childUUID)
	}
	var n int
	if err := dst.DB().QueryRow(`SELECT count(*) FROM tagxref JOIN tag USING(tagid)
		JOIN blob ON blob.rid=tagxref.rid
		WHERE tagname='branch' AND uuid=? AND tagtype=2`, childUUID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("child's propagated branch rows = %d, want 1", n)
	}
}

// storeAndLink stores each artifact in r, crosslinks them, and returns the
// name r gave the last.
func storeAndLink(t *testing.T, r *repo.Repo, artifacts ...[]byte) string {
	t.Helper()
	var uuid string
	for _, data := range artifacts {
		var err error
		if _, uuid, err = blob.Store(r.DB(), data); err != nil {
			t.Fatalf("blob.Store: %v", err)
		}
	}
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	return uuid
}

// buildChildManifest builds a check-in with one parent and no tags of its
// own: it inherits the parent's branch by propagation.
func buildChildManifest(t *testing.T, when time.Time, fileUUID string, fileContent []byte,
	parent string) []byte {
	t.Helper()
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "child",
		D:    when,
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
		U:    deck.User("tester"),
		P:    []string{parent},
	}
	rHash, err := d.ComputeR(func(string) ([]byte, error) { return fileContent, nil })
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	d.R = rHash
	data, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}

func leafUUIDs(t *testing.T, r *repo.Repo) []string {
	t.Helper()
	rows, err := r.DB().Query("SELECT uuid FROM leaf JOIN blob USING(rid) ORDER BY uuid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var uuids []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			t.Fatal(err)
		}
		uuids = append(uuids, uuid)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return uuids
}

// TestPushRepairsLeavesOfCascadeLinks is the server side of
// TestPullRepairsLeavesOfCascadeLinks: the server holds the parent and a
// phantom for the child, so the pushed child fills the phantom and the
// handler's cascade links it before its closing sweep, which links nothing.
func TestPushRepairsLeavesOfCascadeLinks(t *testing.T) {
	content := []byte("pushed file\n")
	when := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	srv := setupPushableTestRepo(t)
	fileUUID := storeAndLink(t, srv, content)
	rootBytes, _ := buildCheckinManifest(t, "root", when, fileUUID, content, nil)
	rootUUID := storeAndLink(t, srv, rootBytes)
	childBytes := buildChildManifest(t, when.Add(time.Minute), fileUUID, content, rootUUID)
	childUUID := hash.SHA3(childBytes) // a new repository's policy names by SHA3
	if _, err := blob.StorePhantom(srv.DB(), childUUID); err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: testPC},
		&xfer.FileCard{UUID: childUUID, Content: childBytes},
	}}
	if _, err := HandleSync(context.Background(), srv, req); err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if got := leafUUIDs(t, srv); len(got) != 1 || got[0] != childUUID {
		t.Fatalf("leaf rows = %v, want only the child %s", got, childUUID)
	}
}
