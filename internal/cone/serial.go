package cone

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"

	"github.com/danmestas/go-libfossil/internal/hash"
)

// Include returns the inclusions, sorted. The slice is a copy.
func (c *Cone) Include() []string {
	return slices.Clone(c.include)
}

// Exclude returns the exclusions, sorted. The slice is a copy.
func (c *Cone) Exclude() []string {
	return slices.Clone(c.exclude)
}

// Equal reports whether two cones have the same members, and so the same
// coverage.
func (c *Cone) Equal(other *Cone) bool {
	return slices.Equal(c.include, other.include) && slices.Equal(c.exclude, other.exclude) &&
		(c.horizon == nil) == (other.horizon == nil) && (c.horizon == nil || *c.horizon == *other.horizon)
}

// String lists the members in one sorted order, an exclusion marked with a
// leading '!' as zit's cone file marks one, for diagnostics that must name
// the cone.
func (c *Cone) String() string {
	var b strings.Builder
	if c.horizon != nil {
		b.WriteString(c.horizon.String())
	}
	c.eachMember(func(m string, exclude bool) {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		if exclude {
			b.WriteByte('!')
		}
		b.WriteString(m)
	})
	return b.String()
}

// eachMember calls fn for every member in one sorted order across both
// polarities: a two-way merge of the sorted lists.
func (c *Cone) eachMember(fn func(member string, exclude bool)) {
	i, j := 0, 0
	for i < len(c.include) || j < len(c.exclude) {
		takeInclude := j == len(c.exclude) || (i < len(c.include) && c.include[i] < c.exclude[j])
		if takeInclude {
			fn(c.include[i], false)
			i++
		} else {
			fn(c.exclude[j], true)
			j++
		}
	}
}

// Serialize returns the canonical form. With no exclusions and no horizon it
// is version 1: the member count, then per member a u16 little-endian length
// and the bytes, so such a cone keeps its digest across the later versions.
// With exclusions: a 0x00 lead-in, the version byte, the total count, then
// per member a polarity flag (0 include, 1 exclude), the length and the
// bytes, in one sorted order (§3.1, §3.3). With a horizon, version 3: the
// same, with the horizon's rule, depth, hash length and hash between the
// count and the members (draft-fossil-horizon-00 §3.3).
func (c *Cone) Serialize() []byte {
	total := len(c.include) + len(c.exclude)
	if (len(c.include) == 0 && c.horizon == nil) || total > MaxMembers {
		panic("cone.Serialize: cone was not built by New")
	}
	flagged := len(c.exclude) > 0 || c.horizon != nil
	var out []byte
	switch {
	case c.horizon != nil:
		h := c.horizon
		out = append(out, 0, serialVersionHorizon, byte(total), byte(h.rule), byte(h.depth), byte(len(h.hash)))
		out = append(out, h.hash...)
	case flagged:
		out = append(out, 0, serialVersionExclusions, byte(total))
	default:
		out = append(out, byte(total))
	}
	c.eachMember(func(m string, exclude bool) {
		if flagged {
			flag := byte(0)
			if exclude {
				flag = 1
			}
			out = append(out, flag)
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(len(m)))
		out = append(out, m...)
	})
	return out
}

// Digest is the SHA3-256 of the canonical form, in lowercase hex: the cache
// key of §7, equal for equal coverage.
func (c *Cone) Digest() string {
	return hash.SHA3(c.Serialize())
}

// Parse reads a serialized cone. It rejects anything Serialize cannot emit,
// an unknown version included (§3.1): a cone is authoritative checkout
// state and is never read best-effort.
func Parse(data []byte) (*Cone, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty serialized cone", ErrInvalidMember)
	}
	h, offset, count, err := parseHeader(data)
	if err != nil {
		return nil, err
	}
	if count == 0 && h == nil {
		return nil, fmt.Errorf("%w: no members", ErrBounds)
	}
	// Only a version-1 form is unflagged, and it opens with its count.
	include, exclude, err := parseMembers(data, offset, count, data[0] == 0)
	if err != nil {
		return nil, err
	}
	c, err := build(include, exclude, h)
	if err != nil {
		return nil, err
	}
	// Round-trip identity: stored state that normalization would change was
	// not written by Serialize, and authoritative state fails closed. An
	// upper-case horizon hash is lower-cased by NewHorizon and so refused
	// here.
	if !bytes.Equal(c.Serialize(), data) {
		return nil, fmt.Errorf("%w: serialized cone is not canonical", ErrInvalidMember)
	}
	return c, nil
}

// parseHeader reads the version lead-in, the count and, for version 3, the
// horizon, returning the offset of the first member.
func parseHeader(data []byte) (h *Horizon, offset, count int, err error) {
	// A version-1 form opens with its nonzero count; a later one with 0x00,
	// the version byte and the count.
	if data[0] != 0 {
		return nil, 1, int(data[0]), nil
	}
	if len(data) < 3 {
		return nil, 0, 0, fmt.Errorf("%w: truncated serialized cone", ErrInvalidMember)
	}
	switch data[1] {
	case serialVersionExclusions:
		return nil, 3, int(data[2]), nil
	case serialVersionHorizon:
		const fixed = 6 // lead-in, version, count, rule, depth, hash length
		if len(data) < fixed {
			return nil, 0, 0, fmt.Errorf("%w: truncated horizon", ErrInvalidMember)
		}
		n := int(data[5])
		if n != 40 && n != 64 || len(data)-fixed < n {
			return nil, 0, 0, fmt.Errorf("%w: bad horizon hash length", ErrInvalidMember)
		}
		hz, err := NewHorizon(string(data[fixed:fixed+n]), HorizonRule(data[3]), HorizonDepth(data[4]))
		if err != nil {
			return nil, 0, 0, fmt.Errorf("%w: %v", ErrInvalidMember, err)
		}
		return &hz, fixed + n, int(data[2]), nil
	}
	return nil, 0, 0, fmt.Errorf("%w: unknown serialized cone version", ErrInvalidMember)
}

// parseMembers reads count members starting at offset, each with a polarity
// flag when flagged, and requires the input to end exactly after the last.
func parseMembers(data []byte, offset, count int, flagged bool) (include, exclude []string, err error) {
	if count > MaxMembers {
		panic("cone.parseMembers: count exceeds the count byte")
	}
	for range count {
		exclusion := false
		if flagged {
			if offset >= len(data) || data[offset] > 1 {
				return nil, nil, fmt.Errorf("%w: bad polarity flag", ErrInvalidMember)
			}
			exclusion = data[offset] == 1
			offset++
		}
		if len(data)-offset < 2 {
			return nil, nil, fmt.Errorf("%w: truncated member length", ErrInvalidMember)
		}
		n := int(binary.LittleEndian.Uint16(data[offset:]))
		offset += 2
		if n == 0 || len(data)-offset < n {
			return nil, nil, fmt.Errorf("%w: bad member length", ErrInvalidMember)
		}
		member := string(data[offset : offset+n])
		offset += n
		if exclusion {
			exclude = append(exclude, member)
		} else {
			include = append(include, member)
		}
	}
	if offset != len(data) {
		return nil, nil, fmt.Errorf("%w: bytes after the last member", ErrInvalidMember)
	}
	return include, exclude, nil
}

// With returns the cone with include added as inclusions and exclude as
// exclusions, renormalized: the incremental amendment of §3.5, equivalent to
// restating every member.
func (c *Cone) With(include, exclude []string) (*Cone, error) {
	return build(append(c.Include(), include...), append(c.Exclude(), exclude...), c.horizon)
}

// Without returns the cone with every member equal to one of paths removed,
// of either polarity, renormalized. As in zit, a path that is no member
// removes nothing. Removing the last inclusion is ErrBounds; removing the
// inclusion an exclusion depends on is ErrDeadExclusion.
func (c *Cone) Without(paths []string) (*Cone, error) {
	drop := func(m string) bool { return slices.Contains(paths, m) }
	return build(slices.DeleteFunc(c.Include(), drop), slices.DeleteFunc(c.Exclude(), drop), c.horizon)
}
