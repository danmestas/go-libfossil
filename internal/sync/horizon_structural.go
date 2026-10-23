package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// seedLogin is the U card of the seeds this origin mints: the origin's own
// label for the minting implementation, as zit's is "zit-serve", never a
// user's login, since a seed is a tag application no user made (§5.2.2).
const seedLogin = "go-libfossil"

// structuralServe is what an origin computed for one structural request
// (draft-fossil-horizon-00 §6.2): the manifests and nodes K's trees reach,
// the boundary hashes, and the seed for C.
type structuralServe struct {
	reach    map[string]bool
	boundary []string
	seedName string
	seedData []byte
	sendSeed bool // the client offered another name, or none
}

// prepareStructural places a structural request's horizon and mints its
// seed. A request whose C the store cannot place, or that has nothing to
// seed, is answered and advertised as the content horizon (§6.2.3): its
// cone is cut at the content depth, so the keep set holds every manifest.
func (h *handler) prepareStructural(ctx context.Context, cards []xfer.Card) error {
	if ctx == nil || h.cone == nil {
		panic("handler.prepareStructural: nil context or cone")
	}
	hz, ok := h.cone.Horizon()
	if !ok || hz.Depth() != cone.DepthStructural {
		return nil
	}
	q := h.repo.DB()
	k, reach, placed, err := h.structuralReach(ctx, hz)
	if err != nil {
		return err
	}
	var name string
	var data []byte
	if placed {
		var c libfossil.FslID
		if err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", hz.Hash()).Scan(&c); err != nil {
			return fmt.Errorf("handler: horizon C: %w", err)
		}
		name, data, err = seed.Mint(q, c, k, seedLogin)
		if errors.Is(err, seed.ErrNothingToSeed) {
			placed = false
		} else if err != nil {
			return err
		}
	}
	if !placed {
		h.cone = withDepth(h.cone, cone.DepthContent)
		return nil
	}
	boundary, err := boundaryOf(q, k, reach)
	if err != nil {
		return err
	}
	offered := requestSeedOffer(cards)
	h.seedOffered = offered
	h.structural = &structuralServe{reach: reach, boundary: boundary, seedName: name, seedData: data,
		sendSeed: !strings.EqualFold(offered, name)}
	return nil
}

// structuralReach places hz ahead of the request's transaction. Through the
// memo it reads in a transaction of its own, which writes nothing, so the
// view is of one committed snapshot and is published at once.
func (h *handler) structuralReach(ctx context.Context, hz cone.Horizon) ([]libfossil.FslID, map[string]bool, bool, error) {
	if h.memo == nil {
		return freshClassifier{}.reach(ctx, h.repo.DB(), hz)
	}
	var k []libfossil.FslID
	var reach map[string]bool
	var placed bool
	err := h.repo.DB().WithReadTx(func(tx *db.Tx) error {
		v, err := h.memo.View(ctx, tx)
		if err != nil {
			return err
		}
		if k, reach, placed, err = v.HorizonReach(ctx, tx, hz); err != nil {
			return err
		}
		h.structuralRead = v.Read()
		v.Publish()
		return nil
	})
	return k, reach, placed, err
}

// boundaryOf names the boundary hashes (§5.3): every check-in a P card of a
// held check-in names that is not itself held, sorted. A clone holds the
// manifests of K and those they reach (reach, by hash), so a baseline below
// C is held too, and its parents need labels: unlabelled, the clone would
// request them as transit and this origin, which withholds manifests below
// C, would never send them.
func boundaryOf(q db.Querier, k []libfossil.FslID, reach map[string]bool) ([]string, error) {
	if q == nil || len(k) == 0 {
		panic("sync.boundaryOf: nil querier or empty held set")
	}
	ids, err := json.Marshal(k)
	if err != nil {
		return nil, fmt.Errorf("handler: horizon boundary: %w", err)
	}
	hashes := make([]string, 0, len(reach))
	for h := range reach {
		hashes = append(hashes, h)
	}
	names, err := json.Marshal(hashes)
	if err != nil {
		return nil, fmt.Errorf("handler: horizon boundary: %w", err)
	}
	rows, err := q.Query(`WITH held(rid) AS (
			SELECT value FROM json_each(?1)
			UNION SELECT b.rid FROM blob b WHERE b.uuid IN (SELECT value FROM json_each(?2)))
		SELECT DISTINCT b.uuid FROM plink JOIN blob b ON b.rid=plink.pid
		WHERE plink.cid IN (SELECT rid FROM held)
		AND plink.pid NOT IN (SELECT rid FROM held)`, string(ids), string(names))
	if err != nil {
		return nil, fmt.Errorf("handler: horizon boundary: %w", err)
	}
	var out []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, uuid)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("handler: horizon boundary: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// emitHorizonServed appends a horizon reply's level (§6.2.5) and, at the
// structural level, every boundary label (§6.2.2: no later than the first
// manifest naming one, so at the head of every reply) and the seed, its
// pragma ahead of its full file card, when the client offered another name
// (§6.2.3). A requester who may not read is answered at the content level:
// the seed and labels are derived state of the history it may not read.
func (h *handler) emitHorizonServed() {
	if !h.horizonScoped() {
		panic("handler.emitHorizonServed: the request carries no horizon")
	}
	if h.structural != nil && !h.canRead() {
		h.structural = nil
		h.cone = withDepth(h.cone, cone.DepthContent)
	}
	level := cone.DepthContent
	if h.structural != nil {
		level = cone.DepthStructural
	}
	served := &xfer.PragmaCard{Name: horizonServedPragma, Values: []string{level.String()}}
	if h.historyURL != "" {
		served.Values = append(served.Values, h.historyURL)
	}
	h.resp = append(h.resp, served)
	// Horizon §6.2.6: the position this reply is served through follows,
	// as zit places it; the inventory values it, and reply drops it from a
	// reply that announced nothing.
	h.servedCursor = &xfer.PragmaCard{Name: horizonCursorPragma}
	h.resp = append(h.resp, h.servedCursor)
	if h.structural == nil {
		return
	}
	for _, uuid := range h.structural.boundary {
		h.resp = append(h.resp, &xfer.PragmaCard{Name: horizonBelowPragma, Values: []string{uuid}})
	}
	if h.structural.sendSeed {
		h.resp = append(h.resp, &xfer.PragmaCard{Name: horizonSeedPragma, Values: []string{h.structural.seedName}},
			&xfer.FileCard{UUID: h.structural.seedName, Content: h.structural.seedData})
	}
}

// withholds reports an artifact a structural request does not keep: a
// check-in manifest or sub-manifest node K's trees do not reach (§5.1).
func (s *structuralServe) withholds(uuid string, data []byte) bool {
	if s == nil || s.reach[uuid] || !deck.HasValidZ(data) {
		return false
	}
	d, err := deck.Parse(data)
	return err == nil && (d.Type == deck.Checkin || d.Type == deck.Submanifest)
}

// structuralClient is a structural store's side of one session
// (draft-fossil-horizon-00 §5.1, §5.3, §6.2.2, §6.2.3, §6.2.5). It is active
// while the store cone is a structural horizon the session has not fallen
// back from.
type structuralClient struct {
	active   bool
	verify   seed.Verifier
	expect   string // the seed name the current reply announced
	served   bool   // the current reply said horizon-served structural
	fallback bool   // the current reply's seed failed its provenance
}

func newStructuralClient(q db.Querier, verify seed.Verifier) (structuralClient, error) {
	c, err := content.StoreCone(q)
	if err != nil {
		return structuralClient{}, err
	}
	h, ok := horizonOf(c)
	return structuralClient{active: ok && h.Depth() == cone.DepthStructural, verify: verify}, nil
}

// level reads a horizon-served pragma as this store takes it up (§6.2.5):
// the structural level only while the store is structural, since a content
// store never enters it whatever an origin advertises.
func (c *structuralClient) level(p *xfer.PragmaCard) (cone.HorizonDepth, bool) {
	d, ok := servedLevel(p)
	if !ok || d == cone.DepthStructural && !c.active {
		return 0, false
	}
	return d, true
}

// beginReply forgets the previous reply's announcement: one is good for the
// reply that made it only.
func (c *structuralClient) beginReply() {
	c.expect, c.served, c.fallback = "", false, false
}

// pragma acts on a reply's horizon-served, horizon-below and horizon-seed
// pragmas; handled is false for any other card.
func (c *structuralClient) pragma(q db.Querier, p *xfer.PragmaCard) (handled bool, err error) {
	switch p.Name {
	case horizonServedPragma:
		if d, ok := servedLevel(p); ok && d == cone.DepthStructural {
			c.served = true
		}
		return false, nil
	case horizonBelowPragma:
		if c.active && len(p.Values) == 1 {
			return true, content.LabelBelow(q, p.Values)
		}
		return true, nil
	case horizonSeedPragma:
		if c.active && len(p.Values) == 1 && hash.IsValidHash(strings.ToLower(p.Values[0])) {
			c.expect = strings.ToLower(p.Values[0])
		}
		return true, nil
	}
	return false, nil
}

// takeSeed routes the file card the reply announced as the seed to the seed
// holding instead of the artifact store, and reports that it did (§6.2.3).
// Only a full-form card can be one. Bytes that do not hash to the name or do
// not parse as a control artifact are dropped; a seed the origin's key does
// not verify is dropped and the store falls back to the content horizon at
// the end of the reply (§5.2.4).
func (c *structuralClient) takeSeed(r *repo.Repo, uuid, deltaSrc string, payload []byte) (bool, error) {
	if r == nil {
		panic("sync.structuralClient.takeSeed: nil repo")
	}
	if !c.active || c.expect == "" || deltaSrc != "" || !strings.EqualFold(uuid, c.expect) {
		return false, nil
	}
	c.expect = ""
	err := r.WithTx(func(tx *db.Tx) error { return seed.Hold(tx, strings.ToLower(uuid), payload, c.verify) })
	switch {
	case errors.Is(err, seed.ErrNotSeed):
		return true, nil
	case errors.Is(err, seed.ErrProvenance):
		c.fallback = true
		return true, nil
	}
	return true, err
}

// endFirstReply enters the structural horizon only when the first reply
// said horizon-served structural and left a seed held (§5.1, §6.2.5); any
// other first reply makes the store a content-horizon store, which it
// reports. It reports whether the store fell back.
func (c *structuralClient) endFirstReply(r *repo.Repo) (bool, error) {
	if !c.active {
		return false, nil
	}
	held, err := seed.Held(r.DB())
	if err != nil {
		return false, err
	}
	if c.served && held != nil && !c.fallback {
		return false, nil
	}
	return true, c.fallBack(r)
}

// endReply falls back after a reply whose seed failed its provenance, and
// otherwise applies the held seed to what the reply linked. It reports
// whether the store fell back.
func (c *structuralClient) endReply(r *repo.Repo) (bool, error) {
	if !c.active {
		return false, nil
	}
	if c.fallback {
		return true, c.fallBack(r)
	}
	return c.apply(r)
}

// apply applies the held seed (seed.Apply), falling back when the origin's
// key no longer verifies it.
func (c *structuralClient) apply(r *repo.Repo) (bool, error) {
	if !c.active {
		return false, nil
	}
	held, err := seed.Held(r.DB())
	if err != nil || held == nil {
		return false, err
	}
	var applied bool
	if err := r.WithTx(func(tx *db.Tx) error {
		var err error
		applied, err = seed.Apply(tx, c.verify)
		return err
	}); err != nil {
		return false, err
	}
	if applied {
		return false, nil
	}
	return true, c.fallBack(r)
}

// fallBack makes the store a content-horizon store for good: the seed is
// discarded and the store re-derived without it, and the store cone is cut
// at the content depth, which drops the boundary labels, so the manifests
// below the horizon are wanted again.
func (c *structuralClient) fallBack(r *repo.Repo) error {
	if r == nil {
		panic("sync.structuralClient.fallBack: nil repo")
	}
	c.active = false
	return r.WithTx(func(tx *db.Tx) error {
		if err := seed.Discard(tx); err != nil {
			return err
		}
		sc, err := content.StoreCone(tx)
		if err != nil {
			return err
		}
		if _, ok := horizonOf(sc); !ok {
			return nil
		}
		return content.SetStoreCone(tx, withDepth(sc, cone.DepthContent))
	})
}
