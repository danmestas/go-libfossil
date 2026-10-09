package sync

// The clone and sync exchanges against draft-fossil-sync-protocol-00
// (#255): clone replies carry only cfile cards, a clone session sends
// nothing but at most an operation-less cleanup after clone_seqno 0, every
// request ends with a randomness comment, and every completed reply ends
// with the timestamp comment.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// recordingTransport serves requests with HandleSync on src and keeps
// every request and reply.
type recordingTransport struct {
	src      *repo.Repo
	requests []*xfer.Message
	replies  []*xfer.Message
}

func (rt *recordingTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := HandleSync(ctx, rt.src, req)
	if err != nil {
		return nil, err
	}
	rt.requests = append(rt.requests, req)
	rt.replies = append(rt.replies, resp)
	return resp, nil
}

// deltaHistoryRepo commits several close versions of one file with delta
// storage, so the older versions are stored as deltas.
func deltaHistoryRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r := setupSyncTestRepo(t)
	var parent libfossil.FslID
	for v := 0; v < 5; v++ {
		var b strings.Builder
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&b, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
			if i == 50+v {
				fmt.Fprintf(&b, "version %d\n", v)
			}
		}
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "f.txt", Content: []byte(b.String())}},
			Comment: fmt.Sprintf("v%d", v),
			User:    "testuser",
			Parent:  parent,
			Delta:   true,
			Time:    time.Date(2026, 1, 1, v, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		parent = rid
	}
	return r
}

func lastCardIsComment(m *xfer.Message, prefix string) bool {
	if len(m.Cards) == 0 {
		return false
	}
	c, ok := m.Cards[len(m.Cards)-1].(*xfer.CommentCard)
	return ok && strings.HasPrefix(c.Text, prefix)
}

func hasCard[T xfer.Card](m *xfer.Message) bool {
	for _, c := range m.Cards {
		if _, ok := c.(T); ok {
			return true
		}
	}
	return false
}

func TestCloneExchangeFollowsDraft(t *testing.T) {
	rt := &recordingTransport{src: deltaHistoryRepo(t)}
	dst := filepath.Join(t.TempDir(), "clone.fossil")
	r, _, err := Clone(context.Background(), dst, rt, CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer r.Close()

	deltas, exhausted := 0, false
	for i, req := range rt.requests {
		if exhausted && (hasCard[*xfer.CloneCard](req) || hasCard[*xfer.PullCard](req) ||
			hasCard[*xfer.PushCard](req)) {
			t.Errorf("request %d carries an operation after clone_seqno 0", i)
		}
		if !lastCardIsComment(req, "") {
			t.Errorf("request %d does not end with a randomness comment", i)
		}
		reply := rt.replies[i]
		if !lastCardIsComment(reply, "timestamp ") {
			t.Errorf("reply %d does not end with the timestamp comment", i)
		}
		for _, c := range reply.Cards {
			switch c := c.(type) {
			case *xfer.FileCard:
				t.Errorf("reply %d carries a file card in a clone (%s)", i, c.UUID)
			case *xfer.CFileCard:
				if c.DeltaSrc != "" {
					deltas++
				}
			case *xfer.CloneSeqNoCard:
				exhausted = exhausted || c.SeqNo == 0
			}
		}
	}
	if deltas == 0 {
		t.Fatal("test setup: the clone carried no delta, so it proves nothing about deltas")
	}
	if !exhausted {
		t.Fatal("the clone never received clone_seqno 0")
	}
	if n := len(rt.requests); n > 2 {
		t.Errorf("clone made %d requests; a one-batch clone is the clone request and a cleanup", n)
	}
}

// Every sync request ends with randomness, with or without a login card;
// it used to appear only on requests that carried a login.
func TestSyncRequestsEndWithRandomness(t *testing.T) {
	src := deltaHistoryRepo(t)
	dst := setupSyncTestRepo(t)
	projectCode, err := src.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	rt := &recordingTransport{src: src}
	if _, err := Sync(context.Background(), dst, rt, SyncOpts{
		Pull: true, ProjectCode: projectCode,
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(rt.requests) == 0 {
		t.Fatal("Sync sent no request")
	}
	for i, req := range rt.requests {
		if !lastCardIsComment(req, "") {
			t.Errorf("pull request %d does not end with a randomness comment", i)
		}
		if !lastCardIsComment(rt.replies[i], "timestamp ") {
			t.Errorf("reply %d does not end with the timestamp comment", i)
		}
	}
}
