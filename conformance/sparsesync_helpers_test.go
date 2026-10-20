package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// scSyncPeer records decoded wire transcripts, including requests an old peer
// ignores. Both directions cross the actual xfer encoder and decoder.
type scSyncPeer struct {
	r        *repo.Repo
	ignore   bool
	opts     sync.HandleOpts
	requests []*xfer.Message
	replies  []*xfer.Message
}

func scSyncWire(m *xfer.Message) (*xfer.Message, error) {
	data, err := m.Encode()
	if err != nil {
		return nil, err
	}
	return xfer.Decode(data, xfer.ContentTypeCompressed)
}

func (p *scSyncPeer) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	req, err := scSyncWire(req)
	if err != nil {
		return nil, err
	}
	p.requests = append(p.requests, req)
	if p.ignore {
		cards := make([]xfer.Card, 0, len(req.Cards))
		for _, card := range req.Cards {
			if c, ok := card.(*xfer.PragmaCard); ok && (c.Name == "cone" || c.Name == "cone-not") {
				continue
			}
			cards = append(cards, card)
		}
		req = &xfer.Message{Cards: cards}
	}
	resp, err := sync.HandleSyncWithOpts(ctx, p.r, req, p.opts)
	if err != nil {
		return nil, err
	}
	resp, err = scSyncWire(resp)
	if err != nil {
		return nil, err
	}
	p.replies = append(p.replies, resp)
	return resp, nil
}

func newSCSyncRepo(t *testing.T, files map[string]string) *scRepo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "origin.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	f := &scRepo{r: r, when: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	f.c1 = f.commit(t, files, 0)
	return f
}

func scSyncFiles() map[string]string {
	return map[string]string{
		"src/a": "inside\n", "src/shared": "shared\n",
		"zzz/shared": "shared\n", "zzz/b": "outside\n",
	}
}

func scSyncMember(value string) []xfer.Card {
	return []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{value}}}
}

func scSyncClone(
	t *testing.T, peer *scSyncPeer, opts sync.CloneOpts,
) (*repo.Repo, *sync.CloneResult) {
	t.Helper()
	r, result, err := sync.Clone(context.Background(),
		filepath.Join(t.TempDir(), "clone.fossil"), peer, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, result
}

func scSyncHolding(r *repo.Repo, text string, want bool) error {
	uuid := hash.SHA1([]byte(text))
	_, got, err := content.CheckAvailableByUUID(r.DB(), uuid)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("holding %s = %v, want %v", uuid, got, want)
	}
	return nil
}

func scSyncNoGimme(messages []*xfer.Message, uuid string) error {
	for _, m := range messages {
		for _, card := range m.Cards {
			if c, ok := card.(*xfer.GimmeCard); ok && c.UUID == uuid {
				return fmt.Errorf("requested elided artifact %s", uuid)
			}
		}
	}
	return nil
}

func scSyncPayloads(m *xfer.Message) map[string]string {
	out := map[string]string{}
	for _, card := range m.Cards {
		switch c := card.(type) {
		case *xfer.FileCard:
			out[c.UUID] = c.DeltaSrc
		case *xfer.CFileCard:
			out[c.UUID] = c.DeltaSrc
		}
	}
	return out
}

func scSyncRefused(m *xfer.Message, want string) error {
	var errors []string
	for _, card := range m.Cards {
		if c, ok := card.(*xfer.ErrorCard); ok {
			errors = append(errors, c.Message)
		}
	}
	if len(errors) != 1 || errors[0] != want || len(scSyncPayloads(m)) != 0 {
		return fmt.Errorf("refusal errors=%q payloads=%v; want one %q and no content",
			errors, scSyncPayloads(m), want)
	}
	return nil
}
