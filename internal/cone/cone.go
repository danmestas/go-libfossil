// Package cone is the cone specification of draft-fossil-sparse-cone-00 §3:
// a finite, normalized set of repository-relative path prefixes, each an
// inclusion or an exclusion, that decides which files a sparse checkout
// materializes and tracks.
//
// A cone covers a path when the most specific (longest) member covering the
// path is an inclusion (§3.3). Members are literal path prefixes naming a
// directory subtree or a single file with equal standing; patterns are not
// supported (§3.4). The absent cone, a nil *Cone, means dense operation
// and never reaches this package.
//
// Members are kept sorted and minimal: a member whose nearest covering member
// has the same polarity is dropped (§3.2), which makes the serialized form
// canonical, so two cones with the same coverage serialize identically and
// Digest is a cache key. The serialized form is byte-for-byte zit's
// (src/cone.zig), the other implementation of the draft.
package cone

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// MaxMembers is the cap on inclusions and exclusions together. §3.2 requires
// at least 64 and recommends 255; 255 is the serialized count byte's range.
const MaxMembers = 255

// MaxMemberLen bounds one member, as zit bounds a tracked path, so a cone can
// never carry a longer name than an F card would (§8).
const MaxMemberLen = 4096

// serialVersionExclusions is the version byte of the form for cones carrying
// exclusions. The version-1 form has no version byte: its first byte is a
// nonzero member count, so the 0x00 lead-in of later versions never
// collides with it.
const serialVersionExclusions = 2

// Compile-time checks: the count byte holds MaxMembers, the u16 length
// prefix holds MaxMemberLen, and §3.2's floor of 64 members is met.
const (
	_ = uint8(MaxMembers)
	_ = uint16(MaxMemberLen)
	_ = uint(MaxMembers - 64)
)

var (
	// ErrInvalidMember: a member fails tracked-path validation (§3.2), is
	// longer than MaxMemberLen, or a serialized cone is malformed or not
	// canonical.
	ErrInvalidMember = errors.New("cone: invalid member")
	// ErrReservedMember: a member names one of fossil's reserved checkout
	// files.
	ErrReservedMember = errors.New("cone: reserved member")
	// ErrBounds: no inclusion, or more than MaxMembers members.
	ErrBounds = errors.New("cone: member count out of bounds")
	// ErrDeadExclusion: an exclusion no inclusion covers (§3.3).
	ErrDeadExclusion = errors.New("cone: exclusion covered by no inclusion")
	// ErrContradiction: one path given as both an inclusion and an
	// exclusion (§3.3).
	ErrContradiction = errors.New("cone: path is both included and excluded")
)

// Cone is a normalized cone. It is immutable; amendments return a new Cone.
type Cone struct {
	include []string // sorted, validated, empty only with a horizon
	exclude []string // sorted, validated, each strictly inside an inclusion
	horizon *Horizon // nil when the cone has no horizon member
}

// entry is one member during normalization.
type entry struct {
	path    string
	exclude bool
}

// New builds a cone from operator input. It validates every member, sorts,
// and drops members whose nearest covering member has the same polarity, so
// input order never affects the result (§3.2, §3.3).
func New(include, exclude []string) (*Cone, error) {
	return build(include, exclude, nil)
}

// build is New with an optional horizon, the one thing that makes an empty
// inclusion list legal.
func build(include, exclude []string, h *Horizon) (*Cone, error) {
	if (len(include) == 0 && h == nil) || len(include)+len(exclude) > MaxMembers {
		return nil, fmt.Errorf("%w: %d inclusions, %d exclusions", ErrBounds, len(include), len(exclude))
	}
	entries := make([]entry, 0, len(include)+len(exclude))
	for _, list := range [2][]string{include, exclude} {
		for _, m := range list {
			if err := validateMember(m); err != nil {
				return nil, err
			}
			entries = append(entries, entry{path: m, exclude: len(entries) >= len(include)})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	c, err := normalize(entries)
	if err != nil {
		return nil, err
	}
	if len(c.include) == 0 && h == nil {
		panic("cone.New: normalization kept no inclusion")
	}
	c.horizon = h
	return c, nil
}

// validateMember applies the tracked-path rules (§3.2).
func validateMember(m string) error {
	if len(m) > MaxMemberLen || !deck.WorktreePathname(m) {
		return fmt.Errorf("%w: %q", ErrInvalidMember, m)
	}
	if deck.IsReservedName(m) {
		return fmt.Errorf("%w: %q", ErrReservedMember, m)
	}
	return nil
}

// normalize keeps a member iff its nearest covering kept member has the
// opposite polarity (or, for an inclusion, there is none), and refuses a
// dead exclusion and a path carried with both polarities. A covering member
// is a byte prefix, so it sorts before everything it covers and has already
// been decided when a descendant arrives. The nearest-ancestor probe scans
// the kept lists rather than keeping a path stack: sorted order interleaves
// uncovered siblings inside a member's range ("src-x" sorts between "src"
// and "src/v", since '-' < '/'), which a stack would wrongly pop past.
func normalize(sorted []entry) (*Cone, error) {
	c := &Cone{}
	for i, e := range sorted {
		if i > 0 && sorted[i-1].path == e.path {
			if sorted[i-1].exclude != e.exclude {
				return nil, fmt.Errorf("%w: %q", ErrContradiction, e.path)
			}
			continue
		}
		nearest, nearestExcludes := c.longestCovering(e.path, true)
		if nearest == "" && e.exclude {
			return nil, fmt.Errorf("%w: %q", ErrDeadExclusion, e.path)
		}
		if nearest != "" && nearestExcludes == e.exclude {
			continue // redundant
		}
		if e.exclude {
			c.exclude = append(c.exclude, e.path)
		} else {
			c.include = append(c.include, e.path)
		}
	}
	return c, nil
}

// longestCovering returns the longest member covering path, with its
// polarity, or "" when none does. With exclusions false only inclusions are
// considered. Ties are impossible: two distinct members covering one path
// are nested, so they differ in length.
func (c *Cone) longestCovering(path string, exclusions bool) (member string, excludes bool) {
	for _, m := range c.include {
		if len(m) > len(member) && memberCovers(m, path) {
			member, excludes = m, false
		}
	}
	if !exclusions {
		return member, excludes
	}
	for _, m := range c.exclude {
		if len(m) > len(member) && memberCovers(m, path) {
			member, excludes = m, true
		}
	}
	return member, excludes
}

// memberCovers reports whether member covers path under slash-component
// semantics (§2): equality, or a prefix ending at a '/' boundary.
func memberCovers(member, path string) bool {
	if member == "" {
		panic("cone.memberCovers: empty member")
	}
	if !strings.HasPrefix(path, member) {
		return false
	}
	return len(path) == len(member) || path[len(member)] == '/'
}

// Decision is a coverage verdict with the member that produced it.
type Decision struct {
	Covered bool
	// Member is the longest member covering the path, of either polarity,
	// or "" when none does.
	Member string
}

// Excluded reports that an exclusion decided the verdict.
func (d Decision) Excluded() bool {
	return d.Member != "" && !d.Covered
}

// Decide applies §3.3: the longest member covering path decides, by its
// polarity. Naming the member is what makes a verdict legible: under longest
// match it is often neither the first member listed nor the obvious one.
func (c *Cone) Decide(path string) Decision {
	if c.pathDense() {
		return Decision{Covered: true}
	}
	member, excludes := c.longestCovering(path, true)
	if member == "" {
		return Decision{}
	}
	return Decision{Covered: !excludes, Member: member}
}

// Covers reports whether the cone covers path.
func (c *Cone) Covers(path string) bool {
	return c.Decide(path).Covered
}

// IntersectsSubtree reports whether the subtree rooted at dir can hold a
// covered path: the cone covers dir, or an inclusion lies inside it (a
// re-inclusion under an exclusion counts). It is conservative on a subtree an
// exclusion hollows out: descending into one costs time, never correctness.
func (c *Cone) IntersectsSubtree(dir string) bool {
	if dir == "" {
		panic("cone.IntersectsSubtree: empty dir")
	}
	if c.pathDense() {
		return true
	}
	if c.Covers(dir) {
		return true
	}
	for _, m := range c.include {
		if memberCovers(dir, m) {
			return true
		}
	}
	return false
}

// MayCover reports whether some covered path p could satisfy first <= p <
// limit, an empty limit meaning no upper bound: the range predicate of §4.1
// with which an enumeration prunes sub-manifest subtrees (§1.3). Every path
// an inclusion m covers lies in [m, m+"0"), '0' being the byte after '/', so
// a range missing every such interval holds nothing covered. Exclusions only
// remove coverage and are ignored, which keeps the answer conservative: a
// true answer costs a descent, never a wrong file set.
func (c *Cone) MayCover(first, limit string) bool {
	if c.pathDense() {
		return true
	}
	for _, m := range c.include {
		if limit != "" && limit <= m {
			continue
		}
		if first >= m+"0" {
			continue
		}
		return true
	}
	return false
}
