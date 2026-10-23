package sync

import (
	"bytes"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The draft defines request pragmas, but no capability advertisement spelling.
// Unknown pragmas remain ignored by peers that predate sparse transfer.
// coneServedPragma acknowledges a reply that applied the request's whole cone
// (draft-fossil-sparse-cone-00 §6.3). It travels only from server to client.
const coneServedPragma = "cone-served"

// horizonPragma carries a request's horizon, `pragma horizon HASH RULE`, and
// horizonServedPragma the level an origin serves it at, `pragma
// horizon-served LEVEL` (draft-fossil-horizon-00 §6.1, §6.2.5). A structural
// request adds horizonStructuralPragma and offers its held seed with
// horizonSeedPragma, `pragma horizon-seed HASH|-`; a reply names a seed it
// sends with the same pragma, and each boundary hash with horizonBelowPragma,
// `pragma horizon-below HASH` (§6.2.2, §6.2.3).
const (
	horizonPragma           = "horizon"
	horizonServedPragma     = "horizon-served"
	horizonStructuralPragma = "horizon-structural"
	horizonSeedPragma       = "horizon-seed"
	horizonBelowPragma      = "horizon-below"
)

// noSeed is the horizon-seed value of a client holding no seed.
const noSeed = "-"

// conePragmas spells c for a request: its path members, then its horizon,
// and for a structural horizon horizon-structural and the offered seed.
func conePragmas(c *cone.Cone, offer string) []xfer.Card {
	if c == nil {
		return nil
	}
	cards := make([]xfer.Card, 0, len(c.Include())+len(c.Exclude())+3)
	for _, m := range c.Include() {
		cards = append(cards, &xfer.PragmaCard{Name: "cone", Values: []string{deck.FossilEncode(m)}})
	}
	for _, m := range c.Exclude() {
		cards = append(cards, &xfer.PragmaCard{Name: "cone-not", Values: []string{deck.FossilEncode(m)}})
	}
	if h, ok := c.Horizon(); ok {
		cards = append(cards, &xfer.PragmaCard{Name: horizonPragma, Values: []string{h.Hash(), h.Rule().String()}})
		if h.Depth() == cone.DepthStructural {
			if offer == "" {
				panic("sync.conePragmas: a structural horizon needs a seed offer")
			}
			cards = append(cards, &xfer.PragmaCard{Name: horizonStructuralPragma},
				&xfer.PragmaCard{Name: horizonSeedPragma, Values: []string{offer}})
		}
	}
	return cards
}

// seedOffer is the horizon-seed value naming the seed q holds (§6.2.3): the
// SHA3-256 name of its bytes, or noSeed.
func seedOffer(q db.Querier) (string, error) {
	held, err := seed.Held(q)
	if err != nil || held == nil {
		return noSeed, err
	}
	return hash.SHA3(held), nil
}

// requestCone reads a request's cone: its path members and its horizon, at
// the structural depth when the request carries horizon-structural, in
// either order. A malformed horizon pragma is ignored, as zit ignores one,
// and the last well-formed one stands.
func requestCone(cards []xfer.Card) (*cone.Cone, error) {
	var include, exclude []string
	var horizon *cone.Horizon
	structural := false
	for _, card := range cards {
		p, ok := card.(*xfer.PragmaCard)
		if !ok {
			continue
		}
		if p.Name == horizonStructuralPragma {
			structural = true
			continue
		}
		if p.Name == horizonPragma {
			if h, ok := requestHorizon(p); ok {
				horizon = &h
			}
			continue
		}
		if p.Name != "cone" && p.Name != "cone-not" {
			continue
		}
		if len(p.Values) != 1 || len(p.Values[0]) > 2*cone.MaxMemberLen {
			return nil, fmt.Errorf("sparse cone: invalid member")
		}
		if len(include)+len(exclude) >= cone.MaxMembers {
			return nil, fmt.Errorf("sparse cone: too many members")
		}
		m := deck.FossilDecode(p.Values[0])
		if p.Name == "cone" {
			include = append(include, m)
		} else {
			exclude = append(exclude, m)
		}
	}
	if len(include)+len(exclude) == 0 && horizon == nil {
		return nil, nil
	}
	var c *cone.Cone
	var err error
	if horizon != nil {
		if structural {
			h, herr := cone.NewHorizon(horizon.Hash(), horizon.Rule(), cone.DepthStructural)
			if herr != nil {
				panic("sync.requestCone: a valid horizon failed to rebuild: " + herr.Error())
			}
			horizon = &h
		}
		c, err = cone.NewWithHorizon(include, exclude, *horizon)
	} else {
		c, err = cone.New(include, exclude)
	}
	if err != nil {
		return nil, fmt.Errorf("sparse cone: %w", err)
	}
	return c, nil
}

func requestHorizon(p *xfer.PragmaCard) (cone.Horizon, bool) {
	if len(p.Values) != 2 {
		return cone.Horizon{}, false
	}
	rule, err := cone.ParseHorizonRule(p.Values[1])
	if err != nil {
		return cone.Horizon{}, false
	}
	h, err := cone.NewHorizon(p.Values[0], rule, cone.DepthContent)
	return h, err == nil
}

// servedLevel reads a horizon-served pragma's level (§6.2.5); ok is false
// for a level this implementation does not know.
func servedLevel(p *xfer.PragmaCard) (cone.HorizonDepth, bool) {
	if len(p.Values) < 1 {
		return 0, false
	}
	d, err := cone.ParseHorizonDepth(p.Values[0])
	return d, err == nil
}

// requestSeedOffer is the seed a structural request says its client holds
// (§6.2.3), "" when it names none: the last well-formed horizon-seed wins.
func requestSeedOffer(cards []xfer.Card) string {
	offer := ""
	for _, card := range cards {
		p, ok := card.(*xfer.PragmaCard)
		if ok && p.Name == horizonSeedPragma && len(p.Values) == 1 && hash.IsValidHash(p.Values[0]) {
			offer = p.Values[0]
		}
	}
	return offer
}

// withDepth is c with its horizon cut at depth d.
func withDepth(c *cone.Cone, d cone.HorizonDepth) *cone.Cone {
	h, ok := c.Horizon()
	if !ok {
		panic("sync.withDepth: the cone has no horizon")
	}
	nh, err := cone.NewHorizon(h.Hash(), h.Rule(), d)
	if err != nil {
		panic("sync.withDepth: " + err.Error())
	}
	out, err := cone.NewWithHorizon(c.Include(), c.Exclude(), nh)
	if err != nil {
		panic("sync.withDepth: " + err.Error())
	}
	return out
}

// servedCluster reports a cluster artifact sent to a horizon store by an
// origin that serves its horizon (draft-fossil-horizon-00 §4.5). The store
// holds no cluster, and against such an origin it wants no name a cluster
// alone gives, so the card is dropped unstored. Only a full-form card is
// tested: a cluster's first card is an M card.
func servedCluster(served bool, deltaSrc string, payload []byte) bool {
	return served && deltaSrc == "" && isCluster(payload)
}

// isCluster reports a full artifact that parses as a cluster.
func isCluster(payload []byte) bool {
	if !bytes.HasPrefix(payload, []byte("M ")) {
		return false
	}
	d, err := deck.Parse(payload)
	return err == nil && d.Type == deck.Cluster
}
