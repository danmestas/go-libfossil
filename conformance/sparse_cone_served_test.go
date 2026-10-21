package conformance

import (
	"context"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Section 6.3's acknowledgement: a cone-aware server sends pragma cone-served
// in the leading pragma run of each reply that applied the request's whole
// cone, and never in a reply that refused the request or served it densely.
// The requester counts acknowledged replies. The cases drive a real clone and
// pull, a peer that strips the cone, a dense request and a refused one.
func scConeServedCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.6.3.cone-served-aware", Req: "SC-6.3-2"}, scConeServedAware},
		{Case{ID: "sparsecone.6.3.cone-served-ignored", Req: "SC-6.3-2"}, scConeServedIgnored},
		{Case{ID: "sparsecone.6.3.cone-served-not-sent", Req: "SC-6.3-2"}, scConeServedNotSent},
		{Case{ID: "sparsecone.6.2.dense-inventory", Req: "SC-6.2-4"}, scDenseInventory},
	}
}

func TestSparseConeServed(t *testing.T) {
	for _, c := range scConeServedCases() {
		t.Run(c.ID, func(t *testing.T) {
			if err := c.run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// scConeServedPlacement counts the reply's cone-served pragmas and fails when
// one follows a card that is not a pragma.
func scConeServedPlacement(m *xfer.Message) (int, error) {
	n := 0
	leading := true
	for _, card := range m.Cards {
		p, ok := card.(*xfer.PragmaCard)
		if !ok {
			leading = false
			continue
		}
		if p.Name != "cone-served" {
			continue
		}
		if !leading || len(p.Values) != 0 {
			return 0, fmt.Errorf("cone-served outside the leading pragma run or with arguments")
		}
		n++
	}
	return n, nil
}

func scConeServedReplies(peer *scSyncPeer, want bool) error {
	for i, reply := range peer.replies {
		n, err := scConeServedPlacement(reply)
		if err != nil {
			return fmt.Errorf("reply %d: %w", i, err)
		}
		if want && n != 1 || !want && n != 0 {
			return fmt.Errorf("reply %d carries %d cone-served, want acknowledged=%v", i, n, want)
		}
	}
	return nil
}

func scConeServedAware(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	peer := &scSyncPeer{r: origin.r}
	r, cloned := scSyncClone(t, peer, sync.CloneOpts{
		StoreCone: scCone(t, []string{"src"}, nil), PullPhantoms: true,
	})
	if cloned.ConeServed == 0 || cloned.ConeServed != cloned.Rounds {
		return fmt.Errorf("clone acknowledged %d of %d rounds", cloned.ConeServed, cloned.Rounds)
	}
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if pulled.ConeServed == 0 || pulled.ConeServed != pulled.Rounds {
		return fmt.Errorf("pull acknowledged %d of %d rounds", pulled.ConeServed, pulled.Rounds)
	}
	return scConeServedReplies(peer, true)
}

// A peer that never sees the cone sends no acknowledgement; the requester
// still filters locally, which scSyncIgnored checks byte for byte.
func scConeServedIgnored(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	peer := &scSyncPeer{r: origin.r, ignore: true}
	r, cloned := scSyncClone(t, peer, sync.CloneOpts{
		StoreCone: scCone(t, []string{"src"}, nil), PullPhantoms: true,
	})
	if cloned.ConeServed != 0 || cloned.Elided == 0 {
		return fmt.Errorf("ignored clone served=%d elided=%d", cloned.ConeServed, cloned.Elided)
	}
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if pulled.ConeServed != 0 {
		return fmt.Errorf("ignored pull acknowledged %d rounds", pulled.ConeServed)
	}
	return scConeServedReplies(peer, false)
}

// A dense request and a refused cone request (one member past the supported
// count) are never acknowledged.
func scConeServedNotSent(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	over := make([]xfer.Card, 0, 257)
	for i := 0; i < 256; i++ {
		over = append(over, &xfer.PragmaCard{Name: "cone", Values: []string{fmt.Sprintf("p%03d", i)}})
	}
	for name, cards := range map[string][]xfer.Card{"dense": nil, "refused": over} {
		peer := &scSyncPeer{r: origin.r}
		req := &xfer.Message{Cards: append(cards, &xfer.CloneCard{Version: 3, SeqNo: 1})}
		if _, err := peer.Exchange(context.Background(), req); err != nil {
			return err
		}
		if err := scConeServedReplies(peer, false); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// Section 6.2: a cone request's pull inventory may be announced densely or
// filtered to the cone, and the requester relies on neither for correctness.
// The peer here adds an out-of-cone announcement to every pull reply, as a
// server announcing densely would. Announced with its check-in, the blob is
// still unclassified, so it is requested and filtered on arrival: it costs
// its payload but is never held. Announced after the check-in that classifies
// it, it is never requested.
type scDenseAnnouncer struct {
	*scSyncPeer
	uuid string
}

func (p scDenseAnnouncer) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := p.scSyncPeer.Exchange(ctx, req)
	if err != nil {
		return nil, err
	}
	resp.Cards = append(resp.Cards, &xfer.IGotCard{UUID: p.uuid})
	return resp, nil
}

func scDenseInventory(t *testing.T) error {
	origin := newSCSyncRepo(t, scSyncFiles())
	peer := &scSyncPeer{r: origin.r}
	r, _ := scSyncClone(t, peer, sync.CloneOpts{StoreCone: scCone(t, []string{"src"}, nil)})
	pull := func(p sync.Transport) error {
		res, err := sync.Sync(context.Background(), r, p, sync.SyncOpts{Pull: true})
		if err == nil && len(res.Errors) != 0 {
			err = fmt.Errorf("pull errors %q", res.Errors)
		}
		return err
	}
	files := scSyncFiles()
	files["zzz/with"] = "announced with its check-in\n"
	tip := origin.commit(t, files, origin.c1)
	with := hash.SHA1([]byte(files["zzz/with"]))
	if err := pull(scDenseAnnouncer{scSyncPeer: peer, uuid: with}); err != nil {
		return err
	}
	if err := scSyncHolding(r, files["zzz/with"], false); err != nil {
		return err
	}
	files["zzz/after"] = "announced after its check-in\n"
	origin.commit(t, files, tip)
	after := hash.SHA1([]byte(files["zzz/after"]))
	if err := pull(peer); err != nil {
		return err
	}
	peer.requests = nil
	if err := pull(scDenseAnnouncer{scSyncPeer: peer, uuid: after}); err != nil {
		return err
	}
	if len(peer.requests) == 0 {
		return fmt.Errorf("pull sent no request")
	}
	if err := scSyncNoGimme(peer.requests, after); err != nil {
		return err
	}
	return scSyncHolding(r, files["zzz/after"], false)
}
