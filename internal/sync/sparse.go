package sync

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The draft defines request pragmas, but no capability advertisement spelling.
// Unknown pragmas remain ignored by peers that predate sparse transfer.
func conePragmas(c *cone.Cone) []xfer.Card {
	if c == nil {
		return nil
	}
	cards := make([]xfer.Card, 0, len(c.Include())+len(c.Exclude()))
	for _, m := range c.Include() {
		cards = append(cards, &xfer.PragmaCard{Name: "cone", Values: []string{deck.FossilEncode(m)}})
	}
	for _, m := range c.Exclude() {
		cards = append(cards, &xfer.PragmaCard{Name: "cone-not", Values: []string{deck.FossilEncode(m)}})
	}
	return cards
}

func requestCone(cards []xfer.Card) (*cone.Cone, error) {
	var include, exclude []string
	for _, card := range cards {
		p, ok := card.(*xfer.PragmaCard)
		if !ok || (p.Name != "cone" && p.Name != "cone-not") {
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
	if len(include)+len(exclude) == 0 {
		return nil, nil
	}
	c, err := cone.New(include, exclude)
	if err != nil {
		return nil, fmt.Errorf("sparse cone: %w", err)
	}
	return c, nil
}
