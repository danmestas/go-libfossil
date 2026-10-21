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
	return slices.Equal(c.include, other.include) && slices.Equal(c.exclude, other.exclude)
}

// String lists the members in one sorted order, an exclusion marked with a
// leading '!' as zit's cone file marks one, for diagnostics that must name
// the cone.
func (c *Cone) String() string {
	var b strings.Builder
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

// Serialize returns the canonical form. With no exclusions it is version 1:
// the member count, then per member a u16 little-endian length and the
// bytes, so an exclusion-free cone keeps its digest across the exclusion
// extension. With exclusions: a 0x00 lead-in, the version byte, the total
// count, then per member a polarity flag (0 include, 1 exclude), the length
// and the bytes, in one sorted order (§3.1, §3.3).
func (c *Cone) Serialize() []byte {
	total := len(c.include) + len(c.exclude)
	if len(c.include) == 0 || total > MaxMembers {
		panic("cone.Serialize: cone was not built by New")
	}
	var out []byte
	if len(c.exclude) > 0 {
		out = append(out, 0, serialVersionExclusions)
	}
	out = append(out, byte(total))
	c.eachMember(func(m string, exclude bool) {
		if len(c.exclude) > 0 {
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
	// A version-1 form opens with its nonzero count; a later one with 0x00,
	// the version byte and the count.
	flagged := data[0] == 0
	offset := 1
	if flagged {
		if len(data) < 3 || data[1] != serialVersionExclusions {
			return nil, fmt.Errorf("%w: unknown serialized cone version", ErrInvalidMember)
		}
		offset = 3
	}
	count := int(data[offset-1])
	if count == 0 {
		return nil, fmt.Errorf("%w: no members", ErrBounds)
	}
	include, exclude, err := parseMembers(data, offset, count, flagged)
	if err != nil {
		return nil, err
	}
	c, err := New(include, exclude)
	if err != nil {
		return nil, err
	}
	// Round-trip identity: stored state that normalization would change was
	// not written by Serialize, and authoritative state fails closed.
	if !bytes.Equal(c.Serialize(), data) {
		return nil, fmt.Errorf("%w: serialized cone is not canonical", ErrInvalidMember)
	}
	return c, nil
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
	return New(append(c.Include(), include...), append(c.Exclude(), exclude...))
}

// Without returns the cone with every member equal to one of paths removed,
// of either polarity, renormalized. As in zit, a path that is no member
// removes nothing. Removing the last inclusion is ErrBounds; removing the
// inclusion an exclusion depends on is ErrDeadExclusion.
func (c *Cone) Without(paths []string) (*Cone, error) {
	drop := func(m string) bool { return slices.Contains(paths, m) }
	return New(slices.DeleteFunc(c.Include(), drop), slices.DeleteFunc(c.Exclude(), drop))
}
