package sync

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The draft defines request pragmas, but no capability advertisement spelling.
// Unknown pragmas remain ignored by peers that predate sparse transfer.
// coneServedPragma acknowledges a reply that applied the request's whole cone
// (draft-fossil-sparse-cone-00 §6.3). It travels only from server to client.
const coneServedPragma = "cone-served"

// horizonPragma carries a request's horizon, `pragma horizon HASH RULE`, and
// horizonServedPragma the level an origin serves it at, `pragma
// horizon-served LEVEL` (draft-fossil-horizon-00 §6.1, §6.2.5). This
// implementation serves and asks for the content level only.
const (
	horizonPragma       = "horizon"
	horizonServedPragma = "horizon-served"
)

// conePragmas spells c for a request: its path members, then its horizon.
// The horizon goes without horizon-structural whatever depth c records.
func conePragmas(c *cone.Cone) []xfer.Card {
	if c == nil {
		return nil
	}
	cards := make([]xfer.Card, 0, len(c.Include())+len(c.Exclude())+1)
	for _, m := range c.Include() {
		cards = append(cards, &xfer.PragmaCard{Name: "cone", Values: []string{deck.FossilEncode(m)}})
	}
	for _, m := range c.Exclude() {
		cards = append(cards, &xfer.PragmaCard{Name: "cone-not", Values: []string{deck.FossilEncode(m)}})
	}
	if h, ok := c.Horizon(); ok {
		cards = append(cards, &xfer.PragmaCard{Name: horizonPragma, Values: []string{h.Hash(), h.Rule().String()}})
	}
	return cards
}

// requestCone reads a request's cone: its path members and its horizon, at
// the content depth. A malformed horizon pragma is ignored, as zit ignores
// one, and the last well-formed one stands.
func requestCone(cards []xfer.Card) (*cone.Cone, error) {
	var include, exclude []string
	var horizon *cone.Horizon
	for _, card := range cards {
		p, ok := card.(*xfer.PragmaCard)
		if !ok {
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

// servesContentHorizon reports a horizon-served pragma at the content level,
// the only one this client asks for. Another level is not taken up: the
// client never enters the structural horizon (§6.2.5), and every reply is
// classified locally regardless, so this only reports the mode.
func servesContentHorizon(p *xfer.PragmaCard) bool {
	return len(p.Values) >= 1 && p.Values[0] == cone.DepthContent.String()
}
