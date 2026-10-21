package content

import (
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// referenceDeck is a classification projection, not a serializable artifact.
// In particular, W bodies and M inventories must die with the parser's result;
// retaining one complete deck per wiki/forum/cluster scales with irrelevant
// content instead of with the facts used by classification.
func referenceDeck(d *deck.Deck) *deck.Deck {
	if d == nil {
		panic("content.referenceDeck: nil deck")
	}
	// Copy retained tokens so a substring cannot pin a discarded large card.
	for i := range d.F {
		d.F[i] = deck.FileCard{Name: strings.Clone(d.F[i].Name), UUID: strings.Clone(d.F[i].UUID)}
	}
	for i := range d.S {
		d.S[i] = deck.SubmanifestCard{Name: strings.Clone(d.S[i].Name), UUID: strings.Clone(d.S[i].UUID)}
	}
	for i := range d.P {
		d.P[i] = strings.Clone(d.P[i])
	}
	for i := range d.Q {
		d.Q[i] = deck.CherryPick{
			Target: strings.Clone(d.Q[i].Target), Baseline: strings.Clone(d.Q[i].Baseline),
		}
	}
	for i := range d.T {
		d.T[i] = deck.TagCard{UUID: strings.Clone(d.T[i].UUID)}
	}
	var attachment *deck.AttachmentCard
	if d.A != nil && d.A.Source != "" {
		attachment = &deck.AttachmentCard{Source: strings.Clone(d.A.Source)}
	}
	*d = deck.Deck{Type: d.Type, A: attachment, B: strings.Clone(d.B), F: d.F, G: strings.Clone(d.G),
		I: strings.Clone(d.I), P: d.P, Q: d.Q, S: d.S, T: d.T}
	return d
}
