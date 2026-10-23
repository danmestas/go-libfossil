package sync

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// seedSigning is the signed seed of one structural reply (draft-fossil-
// horizon-00 §5.2.4), made in three steps so a request that may not read
// costs its origin no signature and no write:
//
//   - before the request's transaction, the seed's kept signature is looked
//     up, which only reads (newSeedSigning);
//   - in it, once the requester may read, the reply gets the seed's pragma
//     and file card with neither filled in (emitHorizonServed);
//   - after it, outside any transaction, the seed is signed if no kept
//     signature serves, the signature is kept, and the two cards are filled,
//     or dropped when the client offered the seed's name (finishSeedSigning).
type seedSigning struct {
	signer   *clearsign.Signer
	self     *clearsign.Key // the origin's own horizon-seed-key, when set
	unsigned string         // the unsigned seed's name
	data     []byte         // the unsigned seed
	kept     *seed.Kept
	offered  string // the seed name the request offered as held
	pragma   *xfer.PragmaCard
	file     *xfer.FileCard
}

// newSeedSigning returns the signing of the unsigned seed data, named name,
// when q's clearsign setting is on, or nil.
func newSeedSigning(q db.Querier, name string, data []byte, offered string) (*seedSigning, error) {
	signer, err := clearsign.FromSettings(q)
	if err != nil || signer == nil {
		return nil, err
	}
	self, err := clearsign.SeedKey(q)
	if err != nil {
		return nil, err
	}
	kept, err := seed.KeptSignature(q, name, signer.Command)
	if err != nil {
		return nil, err
	}
	return &seedSigning{signer: signer, self: self, unsigned: name, data: data, kept: kept, offered: offered}, nil
}

// placeholder appends the seed's pragma and file card to resp, to be filled
// once the seed is signed.
func (s *seedSigning) placeholder(resp []xfer.Card) []xfer.Card {
	if s.pragma != nil {
		panic("seedSigning.placeholder: the reply already holds the seed's cards")
	}
	s.pragma = &xfer.PragmaCard{Name: horizonSeedPragma}
	s.file = &xfer.FileCard{}
	return append(resp, s.pragma, s.file)
}

// finishSeedSigning signs the seed a structural reply placed, after the
// request's transaction, and fills or drops its cards. A request that failed
// or rolled back sends no seed.
func (h *handler) finishSeedSigning(ctx context.Context) error {
	if h.structural == nil || h.structural.signing == nil || h.structural.signing.pragma == nil {
		return nil
	}
	s := h.structural.signing
	if !slices.Contains(h.resp, xfer.Card(s.pragma)) {
		return nil // the reply was reset after the cards were placed
	}
	if h.fatal || h.aborted {
		h.resp = dropCards(h.resp, s.pragma, s.file)
		return nil
	}
	name, signed, err := s.sign(ctx, h.repo, h.clock.Now())
	if err != nil {
		return err
	}
	if strings.EqualFold(s.offered, name) {
		h.resp = dropCards(h.resp, s.pragma, s.file)
		return nil
	}
	s.pragma.Values = []string{name}
	s.file.UUID, s.file.Content = name, signed
	return nil
}

// sign returns the signed seed's name and bytes: the kept signature when
// there is one the origin's own key, if set, still verifies, else a new one.
// Keeping is best effort: a store that cannot write, a read-only mirror say,
// signs afresh for every request, and a clone re-holds the seed whenever its
// name changes.
func (s *seedSigning) sign(ctx context.Context, r *repo.Repo, now time.Time) (string, []byte, error) {
	if s.kept != nil && (s.self == nil || s.self.Verify(ctx, s.kept.Signed) == nil) {
		if s.kept.Stale(now) {
			// Best effort: a failed touch only lets the row age toward eviction.
			_, _ = seed.Keep(r, s.unsigned, s.signer.Command, s.kept.Signed, false, now)
		}
		return hash.SHA3(s.kept.Signed), s.kept.Signed, nil
	}
	signed, err := s.signer.Sign(ctx, s.data)
	if err != nil {
		return "", nil, err
	}
	if kept, err := seed.Keep(r, s.unsigned, s.signer.Command, signed, s.kept != nil, now); err == nil {
		signed = kept
	}
	return hash.SHA3(signed), signed, nil
}

// dropCards returns resp without the cards drop names, by identity.
func dropCards(resp []xfer.Card, drop ...xfer.Card) []xfer.Card {
	return slices.DeleteFunc(resp, func(c xfer.Card) bool { return slices.Contains(drop, c) })
}
