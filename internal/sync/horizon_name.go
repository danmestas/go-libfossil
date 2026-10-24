package sync

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// horizonNamePragma asks for a horizon by name, `pragma horizon-name NAME
// RULE DEPTH`, in place of pragma horizon, and horizonResolvedPragma echoes
// what the origin resolved it to, `pragma horizon-resolved HASH STEPS`
// (draft-fossil-horizon-00 §6.1, §6.2.7, as zit 0.7.1's erratum adds them).
const (
	horizonNamePragma     = "horizon-name"
	horizonResolvedPragma = "horizon-resolved"
)

// errMalformedName answers a horizon-name pragma an origin cannot read, or a
// request carrying it beside pragma horizon or twice (§6.2.7).
const errMalformedName = "malformed horizon-name pragma"

// nameNotFound answers a horizon-name that resolves to no check-in.
func nameNotFound(name string) string { return "horizon name not found: " + name }

// resolveRequestName resolves a request's horizon-name pragma (§6.2.7). It
// returns cards with that pragma replaced by the pragma horizon it resolves
// to, and the horizon-resolved pragma to echo; a request without one comes
// back unchanged. A name the origin cannot resolve, or a malformed request,
// is a refusal message instead, which the caller sends once the requester's
// authority is known, since an error from a login or a capability check
// takes precedence over it.
func resolveRequestName(q db.Querier, cards []xfer.Card) (out []xfer.Card, echo *xfer.PragmaCard, refusal string, err error) {
	if q == nil {
		panic("sync.resolveRequestName: nil querier")
	}
	at, horizons := -1, 0
	for i, card := range cards {
		p, ok := card.(*xfer.PragmaCard)
		switch {
		case !ok:
		case p.Name == horizonPragma:
			horizons++
		case p.Name == horizonNamePragma && at >= 0:
			return cards, nil, errMalformedName, nil
		case p.Name == horizonNamePragma:
			at = i
		}
	}
	if at < 0 {
		return cards, nil, "", nil
	}
	p := cards[at].(*xfer.PragmaCard)
	name, rule, depth, ok := parseNamePragma(p)
	if !ok || horizons > 0 {
		return cards, nil, errMalformedName, nil
	}
	got, err := horizon.ResolveName(q, name, depth)
	if errors.Is(err, horizon.ErrNameNotFound) {
		return cards, nil, nameNotFound(name), nil
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("handler: %w", err)
	}
	out = slices.Clone(cards)
	out[at] = &xfer.PragmaCard{Name: horizonPragma, Values: []string{got.Hash, rule.String()}}
	echo = &xfer.PragmaCard{Name: horizonResolvedPragma,
		Values: []string{got.Hash, strconv.FormatUint(uint64(got.Steps), 10)}}
	return out, echo, "", nil
}

// parseNamePragma reads `pragma horizon-name NAME RULE DEPTH`: NAME a card
// token, RULE a horizon rule, DEPTH plain base-10 digits that fit 32 bits.
func parseNamePragma(p *xfer.PragmaCard) (name string, rule cone.HorizonRule, depth uint32, ok bool) {
	if len(p.Values) != 3 {
		return "", 0, 0, false
	}
	rule, err := cone.ParseHorizonRule(p.Values[1])
	if err != nil {
		return "", 0, 0, false
	}
	depth, ok = parseCount(p.Values[2])
	name = deck.FossilDecode(p.Values[0])
	return name, rule, depth, ok && name != ""
}

// parseCount reads plain base-10 digits that fit 32 bits; a sign, a space
// or an empty token is not a count.
func parseCount(s string) (uint32, bool) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}

// echoResolved appends the request's horizon-resolved pragma, which goes
// ahead of horizon-served and every file card (§6.2.7).
func (h *handler) echoResolved() {
	if h.resolved != nil {
		h.resp = append(h.resp, h.resolved)
	}
}

// nameAuthority reports a requester who may read or clone, with the
// capabilities nobody has and a request's one login card grants, as the
// card loop will find them. Only for such a requester is a horizon-name
// resolved (§6.2.7): for anyone else the request is served as if it named
// no horizon, so no reply to them depends on whether a name exists, and
// no walk back is made on their behalf.
func (h *handler) nameAuthority(cards []xfer.Card) bool {
	caps := h.caps
	var logins []*xfer.LoginCard
	for _, card := range cards {
		if c, ok := card.(*xfer.LoginCard); ok {
			logins = append(logins, c)
		}
	}
	// The loop refuses a second login card, granting nothing.
	if len(logins) == 1 && h.projectCode != "" {
		if u, err := auth.VerifyLogin(h.repo.DB(), h.projectCode, logins[0]); err == nil {
			caps += u.Cap
		}
	}
	return h.cloneRead || auth.CanRead(caps) || auth.CanClone(caps)
}

// refuseName ends a request whose horizon-name the origin refused with that
// one error card (§6.2.7), once the card loop has let the requester through:
// a login, clone or pull refusal is answered instead.
func (h *handler) refuseName() bool {
	if h.nameRefusal == "" || h.fatal || h.broke {
		return false
	}
	h.fail(h.nameRefusal)
	return true
}

// namedClone is a clone's side of a horizon asked for by name (§6.1, §6.3):
// what it asks for, until the origin resolves it or the clone must.
type namedClone struct {
	NamedHorizon
	include, exclude []string // the store cone's path members
	sent             bool     // the current request carried horizon-name
	content          bool     // a file card has arrived
	settled          bool     // resolved, or left to resolve locally
	local            bool     // the origin answered without resolving it
}

// newNamedClone is the named horizon opts asks for, nil for none.
func newNamedClone(opts CloneOpts) (*namedClone, error) {
	if opts.Named == nil {
		return nil, nil
	}
	if _, ok := horizonOf(opts.StoreCone); ok {
		return nil, fmt.Errorf("sync.Clone: a named horizon and a store cone horizon conflict")
	}
	if opts.Named.Name == "" {
		return nil, fmt.Errorf("sync.Clone: empty horizon name")
	}
	if _, err := cone.ParseHorizonRule(opts.Named.Rule.String()); err != nil {
		return nil, fmt.Errorf("sync.Clone: %w", err)
	}
	if _, err := cone.ParseHorizonDepth(opts.Named.Level.String()); err != nil {
		return nil, fmt.Errorf("sync.Clone: %w", err)
	}
	n := &namedClone{NamedHorizon: *opts.Named}
	if opts.StoreCone != nil {
		n.include, n.exclude = opts.StoreCone.Include(), opts.StoreCone.Exclude()
	}
	return n, nil
}

// structural reports a request for the structural horizon.
func (n *namedClone) structural() bool { return n.Level == cone.DepthStructural }

// noteContent records that a file card arrived: a horizon-resolved after it
// is refused.
func (n *namedClone) noteContent() {
	if n != nil {
		n.content = true
	}
}

// pathCone is the store cone a named clone holds until C is known: the
// requested path members alone, nil when there are none.
func (n *namedClone) pathCone() *cone.Cone {
	if len(n.include) == 0 {
		return nil
	}
	c, err := cone.New(n.include, n.exclude)
	if err != nil {
		panic("sync.namedClone.pathCone: " + err.Error())
	}
	return c
}

// at is the requested cone with its horizon at hash and level.
func (n *namedClone) at(hash string, level cone.HorizonDepth) (*cone.Cone, error) {
	nh, err := cone.NewHorizon(hash, n.Rule, level)
	if err != nil {
		return nil, err
	}
	return cone.NewWithHorizon(n.include, n.exclude, nh)
}

// pragmas are the horizon pragmas a clone request carries while the name is
// unsettled: horizon-name, and for a structural horizon the structural
// pragmas offering no seed. A cleanup request asks for nothing.
func (n *namedClone) pragmas(cleanup bool) []xfer.Card {
	if n == nil {
		return nil
	}
	n.sent = false
	if n.settled || cleanup {
		return nil
	}
	n.sent = true
	cards := []xfer.Card{&xfer.PragmaCard{Name: horizonNamePragma, Values: []string{deck.FossilEncode(n.Name),
		n.Rule.String(), strconv.FormatUint(uint64(n.Depth), 10)}}}
	if n.structural() {
		cards = append(cards, &xfer.PragmaCard{Name: horizonStructuralPragma},
			&xfer.PragmaCard{Name: horizonSeedPragma, Values: []string{noSeed}})
	}
	return cards
}

// resolved reads the origin's horizon-resolved (§6.2.7) and returns the
// store cone at C. The clone fails on one it did not ask for in the request
// this reply answers, a second one, one that follows content, one that
// names no full hash, or one that took more steps than asked.
func (n *namedClone) resolved(p *xfer.PragmaCard) (*cone.Cone, uint32, error) {
	if n == nil || !n.sent || n.settled || n.content {
		return nil, 0, fmt.Errorf("sync.Clone: unexpected %s pragma", horizonResolvedPragma)
	}
	if len(p.Values) != 2 {
		return nil, 0, fmt.Errorf("sync.Clone: malformed %s pragma", horizonResolvedPragma)
	}
	steps, ok := parseCount(p.Values[1])
	if !ok || steps > n.Depth {
		return nil, 0, fmt.Errorf("sync.Clone: %s took %q steps, asked for %d", horizonResolvedPragma, p.Values[1], n.Depth)
	}
	c, err := n.at(p.Values[0], n.Level)
	if err != nil {
		return nil, 0, fmt.Errorf("sync.Clone: %s: %w", horizonResolvedPragma, err)
	}
	n.settled = true
	return c, steps, nil
}

// endReply settles the name after the first reply that answers the clone,
// the first carrying the clone sequence (§6.3): one that carried no
// horizon-resolved came from a peer that ignored the pragma, and the clone
// resolves the name itself. A reply without the sequence, such as a private
// origin's challenge to a request without a login, settles nothing: the
// request that follows resends the name with the login, and is still the
// clone's first request in §6.1's sense, since no reply has answered it.
func (n *namedClone) endReply(msg *xfer.Message) {
	if n == nil || n.settled {
		return
	}
	if slices.ContainsFunc(msg.Cards, func(c xfer.Card) bool {
		_, ok := c.(*xfer.CloneSeqNoCard)
		return ok
	}) {
		n.settled, n.local = true, true
	}
}

// takeResolved makes the check-in the origin resolved the clone's horizon
// (§6.2.7, §6.3): the store records its hash, never the name.
func (cs *cloneSession) takeResolved(p *xfer.PragmaCard) error {
	c, steps, err := cs.named.resolved(p)
	if err != nil {
		return err
	}
	if err := content.SetStoreCone(cs.repo.DB(), c); err != nil {
		return err
	}
	h, _ := c.Horizon()
	cs.result.HorizonResolved, cs.result.HorizonSteps = h.Hash(), steps
	return nil
}

// settleNamed finishes a named clone once the transfer is done (§6.3). A
// name the origin resolved must have brought its check-in; one it did not
// is resolved over what the clone holds, at the content horizon.
func (cs *cloneSession) settleNamed() error {
	n := cs.named
	if n == nil {
		return nil
	}
	if !n.settled {
		return fmt.Errorf("sync.Clone: no reply settled the horizon name %q", n.Name)
	}
	q := cs.repo.DB()
	if !n.local {
		var size int64
		err := q.QueryRow("SELECT size FROM blob WHERE uuid=?", cs.result.HorizonResolved).Scan(&size)
		if errors.Is(err, sql.ErrNoRows) || err == nil && size < 0 {
			return fmt.Errorf("sync.Clone: the resolved check-in %s never arrived", cs.result.HorizonResolved)
		}
		return err
	}
	got, err := horizon.ResolveName(q, n.Name, n.Depth)
	if errors.Is(err, horizon.ErrNameNotFound) {
		return fmt.Errorf("sync.Clone: %s", nameNotFound(n.Name))
	}
	if err != nil {
		return fmt.Errorf("sync.Clone: %w", err)
	}
	c, err := n.at(got.Hash, cone.DepthContent)
	if err != nil {
		return fmt.Errorf("sync.Clone: %w", err)
	}
	if err := content.SetStoreCone(q, c); err != nil {
		return err
	}
	cs.result.HorizonResolved, cs.result.HorizonSteps, cs.result.HorizonResolvedLocally = got.Hash, got.Steps, true
	return nil
}
