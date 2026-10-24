package libfossil

import (
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
)

// ErrOutsideCone is wrapped by the refusal of a checkout operation that would
// act on a path its sparse cone does not cover: adding, removing, renaming,
// reverting or committing such a path, or a merge that would change one. The
// message names the path, the cone and the member that excluded it.
var ErrOutsideCone = checkout.ErrOutsideCone

// ErrHorizonCheckout is returned when a checkout is given a cone that carries a
// horizon. A content horizon (draft-fossil-horizon-00 §3.2) is a property of
// the repository's store, not of a checkout: set it on the store cone with
// Repo.SetStoreCone.
var ErrHorizonCheckout = errors.New("libfossil: a horizon is a property of the repository's store, not of a checkout")

// checkoutCone refuses a cone with a horizon and unwraps one without.
func checkoutCone(c *Cone) (*cone.Cone, error) {
	if c != nil {
		if _, ok := c.inner.Horizon(); ok {
			return nil, ErrHorizonCheckout
		}
	}
	return unwrapCone(c), nil
}

// Cone is a sparse checkout's cone (draft-fossil-sparse-cone-00 §3): a set of
// repository paths, each a directory subtree or a single file, included or
// excluded. The longest member covering a path decides whether the cone
// covers it, so an exclusion carves a part out of an inclusion, and a deeper
// inclusion re-includes part of an exclusion. Members are literal prefixes,
// never patterns. A Cone is immutable; With and Without return new ones.
type Cone struct {
	inner *cone.Cone
}

// NewCone builds a cone from inclusion and exclusion members. Members are
// validated as tracked paths, sorted, and reduced to the minimal set with the
// same coverage, so input order never matters. It refuses an empty
// inclusion list, more than 255 members, an exclusion no inclusion covers,
// and a path given with both polarities.
func NewCone(include, exclude []string) (*Cone, error) {
	inner, err := cone.New(include, exclude)
	if err != nil {
		return nil, fmt.Errorf("libfossil: cone: %w", err)
	}
	return &Cone{inner: inner}, nil
}

// HorizonRule is the held-set rule a horizon names (draft-fossil-horizon-00
// §3.1).
type HorizonRule int

const (
	// HorizonDescendants holds the check-in and every descendant of it. It is
	// the zero value.
	HorizonDescendants HorizonRule = iota
	// HorizonLeaves holds the descendants plus every open leaf's ancestry back
	// to its nearest common ancestor with the check-in. Choose it knowingly
	// (§3.1): it is what lets a client merge across live branches offline, and
	// it holds every descendant of where each childless check-in's history
	// meets the check-in's. On a repository with old unmerged branches that is
	// nearly the whole history: 20,907 of 21,008 check-ins of fossil-scm.org's.
	HorizonLeaves
)

// String names the rule as the operator spells it.
func (r HorizonRule) String() string {
	return cone.HorizonRule(r).String()
}

// Horizon is the temporal member of a store cone (draft-fossil-horizon-00
// §3.1): blobs only check-ins outside the held set name are elided. The zero
// value of Structural is the content horizon (§4), which keeps every manifest.
type Horizon struct {
	// Checkin is the check-in's full artifact hash, 40 or 64 hex digits; as
	// CloneOpts.Horizon it may also be a name the origin resolves.
	Checkin string
	Rule    HorizonRule
	// Structural takes the structural cut (§5): the manifests below the
	// check-in are elided too, and a seed the origin mints stands in for the
	// branch and tag state they carried. A clone or pull whose origin does
	// not serve it falls back to the content horizon and reports so.
	Structural bool
}

// NewHorizonCone builds a cone with a horizon and optional path members. With
// no include members the cone is horizon-only: it covers every path and only
// the horizon narrows what is held. Checkin is normalized to lower case.
func NewHorizonCone(include, exclude []string, h Horizon) (*Cone, error) {
	depth := cone.DepthContent
	if h.Structural {
		depth = cone.DepthStructural
	}
	ih, err := cone.NewHorizon(h.Checkin, cone.HorizonRule(h.Rule), depth)
	if err != nil {
		return nil, fmt.Errorf("libfossil: cone: %w", err)
	}
	inner, err := cone.NewWithHorizon(include, exclude, ih)
	if err != nil {
		return nil, fmt.Errorf("libfossil: cone: %w", err)
	}
	return &Cone{inner: inner}, nil
}

// Horizon returns the cone's horizon, if it has one.
func (c *Cone) Horizon() (Horizon, bool) {
	h, ok := c.inner.Horizon()
	if !ok {
		return Horizon{}, false
	}
	return Horizon{Checkin: h.Hash(), Rule: HorizonRule(h.Rule()), Structural: h.Depth() == cone.DepthStructural}, true
}

// WithHorizon returns the cone with its horizon replaced by h, keeping the path
// members; nil drops the horizon. Dropping the horizon of a horizon-only cone
// is an error, since nothing is left: make the store dense with
// Repo.SetStoreCone(nil).
func (c *Cone) WithHorizon(h *Horizon) (*Cone, error) {
	if h == nil {
		inner, err := cone.New(c.inner.Include(), c.inner.Exclude())
		if err != nil {
			return nil, fmt.Errorf("libfossil: cone: drop horizon: %w", err)
		}
		return &Cone{inner: inner}, nil
	}
	return NewHorizonCone(c.inner.Include(), c.inner.Exclude(), *h)
}

// wrapCone is nil for nil, the dense checkout's cone.
func wrapCone(inner *cone.Cone) *Cone {
	if inner == nil {
		return nil
	}
	return &Cone{inner: inner}
}

// unwrapCone is nil for nil.
func unwrapCone(c *Cone) *cone.Cone {
	if c == nil {
		return nil
	}
	return c.inner
}

// Include returns the inclusion members, sorted.
func (c *Cone) Include() []string { return c.inner.Include() }

// Exclude returns the exclusion members, sorted.
func (c *Cone) Exclude() []string { return c.inner.Exclude() }

// Covers reports whether the cone covers path.
func (c *Cone) Covers(path string) bool { return c.inner.Covers(path) }

// Decide reports whether the cone covers path and the member that decided:
// the longest member covering it, of either polarity, or "" when none does.
// Under longest match the deciding member is often not the first one listed,
// so naming it is what explains a verdict.
func (c *Cone) Decide(path string) (covered bool, member string) {
	d := c.inner.Decide(path)
	return d.Covered, d.Member
}

// Digest is the SHA3-256 of the cone's canonical serialization, in hex: equal
// for equal coverage, and the same value zit computes.
func (c *Cone) Digest() string { return c.inner.Digest() }

// String lists the members in sorted order, an exclusion marked with '!'. A
// horizon leads, as "horizon <check-in> <rule> content".
func (c *Cone) String() string { return c.inner.String() }

// With returns the cone with more members: the incremental form of widening
// (or, with exclusions, of narrowing).
func (c *Cone) With(include, exclude []string) (*Cone, error) {
	inner, err := c.inner.With(include, exclude)
	if err != nil {
		return nil, fmt.Errorf("libfossil: cone: %w", err)
	}
	return &Cone{inner: inner}, nil
}

// Without returns the cone with every member equal to one of paths removed,
// of either polarity. A path that is not a member removes nothing.
func (c *Cone) Without(paths []string) (*Cone, error) {
	inner, err := c.inner.Without(paths)
	if err != nil {
		return nil, fmt.Errorf("libfossil: cone: %w", err)
	}
	return &Cone{inner: inner}, nil
}

// ConeChange reports what Checkout.SetCone did to the working directory.
type ConeChange struct {
	Materialized   []string // newly covered files written, sorted
	Dematerialized []string // newly uncovered files removed, sorted
}

// Cone returns the checkout's cone, or nil when the checkout is dense.
func (c *Checkout) Cone() *Cone {
	return wrapCone(c.inner.Cone())
}

// SetCone replaces the checkout's cone; nil makes it dense. Files of the
// checked-out version that the new cone covers and the old one did not are
// written; files it no longer covers are removed from the working directory
// (the repository keeps them). It refuses, changing nothing, when a file
// leaving the cone has local changes or a file entering it would overwrite an
// untracked file. A cone with a horizon is refused with ErrHorizonCheckout.
func (c *Checkout) SetCone(next *Cone) (ConeChange, error) {
	inner, err := checkoutCone(next)
	if err != nil {
		return ConeChange{}, err
	}
	res, err := c.inner.SetCone(inner)
	if err != nil {
		return ConeChange{}, fmt.Errorf("libfossil: set cone: %w", err)
	}
	return ConeChange{Materialized: res.Materialized, Dematerialized: res.Dematerialized}, nil
}
