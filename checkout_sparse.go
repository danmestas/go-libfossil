package libfossil

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
)

// ErrOutsideCone is wrapped by the refusal of a checkout operation that would
// act on a path its sparse cone does not cover: adding, removing, renaming,
// reverting or committing such a path, or a merge that would change one. The
// message names the path, the cone and the member that excluded it.
var ErrOutsideCone = checkout.ErrOutsideCone

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

// String lists the members in sorted order, an exclusion marked with '!'.
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
// untracked file.
func (c *Checkout) SetCone(next *Cone) (ConeChange, error) {
	res, err := c.inner.SetCone(unwrapCone(next))
	if err != nil {
		return ConeChange{}, fmt.Errorf("libfossil: set cone: %w", err)
	}
	return ConeChange{Materialized: res.Materialized, Dematerialized: res.Dematerialized}, nil
}
