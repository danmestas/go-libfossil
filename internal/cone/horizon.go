package cone

import (
	"errors"
	"fmt"
	"strings"
)

// serialVersionHorizon is the version byte of the form for cones carrying a
// horizon member (draft-fossil-horizon-00 §3.3). A distinct version, so a
// reader predating horizons refuses the cone rather than reads its lead-in
// as path members.
const serialVersionHorizon = 3

// ErrInvalidHorizon: a horizon hash is not 40 or 64 hex digits, or a rule
// or depth is not one of the defined values.
var ErrInvalidHorizon = errors.New("cone: invalid horizon")

// HorizonRule is the held-set rule a horizon names (§3.1). The values are
// the serialized bytes; never renumber.
type HorizonRule uint8

const (
	// RuleDescendants holds C and every descendant of C: the default.
	RuleDescendants HorizonRule = 0
	// RuleLeaves holds the descendants plus every open leaf's ancestry back
	// to its nearest common ancestor with C.
	RuleLeaves HorizonRule = 1
)

// HorizonDepth is which cut a horizon takes (§1.1). The values are the
// serialized bytes; never renumber.
type HorizonDepth uint8

const (
	// DepthContent elides only blobs below the horizon (§4).
	DepthContent HorizonDepth = 0
	// DepthStructural elides manifests and nodes below it too (§5).
	DepthStructural HorizonDepth = 1
)

func (r HorizonRule) valid() bool  { return r == RuleDescendants || r == RuleLeaves }
func (d HorizonDepth) valid() bool { return d == DepthContent || d == DepthStructural }

// String names the rule as the operator spells it.
func (r HorizonRule) String() string {
	switch r {
	case RuleDescendants:
		return "descendants"
	case RuleLeaves:
		return "leaves"
	}
	return fmt.Sprintf("rule(%d)", uint8(r))
}

// String names the depth as the operator spells it.
func (d HorizonDepth) String() string {
	switch d {
	case DepthContent:
		return "content"
	case DepthStructural:
		return "structural"
	}
	return fmt.Sprintf("depth(%d)", uint8(d))
}

// ParseHorizonRule reads a rule by name.
func ParseHorizonRule(name string) (HorizonRule, error) {
	for _, r := range []HorizonRule{RuleDescendants, RuleLeaves} {
		if r.String() == name {
			return r, nil
		}
	}
	return 0, fmt.Errorf("%w: unknown rule %q", ErrInvalidHorizon, name)
}

// ParseHorizonDepth reads a depth by name.
func ParseHorizonDepth(name string) (HorizonDepth, error) {
	for _, d := range []HorizonDepth{DepthContent, DepthStructural} {
		if d.String() == name {
			return d, nil
		}
	}
	return 0, fmt.Errorf("%w: unknown depth %q", ErrInvalidHorizon, name)
}

// Horizon is the temporal member of a cone (§3.1): a check-in C by full
// artifact hash, the rule deriving the held set from it, and the depth of
// the cut. It is a comparable value; build one with NewHorizon.
type Horizon struct {
	hash  string // 40 or 64 lower-case hex digits
	rule  HorizonRule
	depth HorizonDepth
}

// NewHorizon validates and builds a horizon. The hash is 40 (SHA-1) or 64
// (SHA-3) hex digits; case is not an error, the stored form is lower-case.
func NewHorizon(hash string, rule HorizonRule, depth HorizonDepth) (Horizon, error) {
	if len(hash) != 40 && len(hash) != 64 {
		return Horizon{}, fmt.Errorf("%w: hash length %d", ErrInvalidHorizon, len(hash))
	}
	for i := 0; i < len(hash); i++ {
		if !isHexDigit(hash[i]) {
			return Horizon{}, fmt.Errorf("%w: hash is not hex", ErrInvalidHorizon)
		}
	}
	if !rule.valid() || !depth.valid() {
		return Horizon{}, fmt.Errorf("%w: %v, %v", ErrInvalidHorizon, rule, depth)
	}
	return Horizon{hash: strings.ToLower(hash), rule: rule, depth: depth}, nil
}

func isHexDigit(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

// Hash is C's full artifact hash, lower-case.
func (h Horizon) Hash() string { return h.hash }

// Rule is the held-set rule.
func (h Horizon) Rule() HorizonRule { return h.rule }

// Depth is the depth of the cut.
func (h Horizon) Depth() HorizonDepth { return h.depth }

// String renders the horizon for diagnostics.
func (h Horizon) String() string {
	return fmt.Sprintf("horizon %s %s %s", h.hash, h.rule, h.depth)
}

// NewWithHorizon is New for a cone that also carries a horizon. With no
// inclusions the cone is horizon-only: path-dense, covering every path.
func NewWithHorizon(include, exclude []string, h Horizon) (*Cone, error) {
	if len(h.hash) != 40 && len(h.hash) != 64 {
		return nil, fmt.Errorf("%w: horizon not built by NewHorizon", ErrInvalidHorizon)
	}
	return build(include, exclude, &h)
}

// Horizon returns the cone's horizon member, if it has one.
func (c *Cone) Horizon() (Horizon, bool) {
	if c.horizon == nil {
		return Horizon{}, false
	}
	return *c.horizon, true
}

// pathDense reports a horizon-only cone: no path members, so every path is
// covered and only the horizon narrows what is held.
func (c *Cone) pathDense() bool {
	return len(c.include) == 0
}
