package conformance

import (
	"bytes"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
)

// The horizon suite tracks draft-fossil-horizon-00. So far it covers the
// horizon member of the cone (§3.3): its canonical serialization and the
// version byte that makes a reader predating it refuse the cone rather than
// read it as path members.

func horizonSuiteCases() []Case {
	return []Case{horizonCanonical, horizonUnknownVersion}
}

var (
	horizonCanonical      = Case{ID: "horizon.3.3.canonical-member", Req: "HZ-3.3-1"}
	horizonUnknownVersion = Case{ID: "horizon.3.3.unknown-version", Req: "HZ-3.3-2"}
)

const horizonHash = "0123456789abcdef0123456789abcdef01234567"

func TestHorizon(t *testing.T) {
	check(t, horizonCanonical, func() error {
		h, err := cone.NewHorizon(horizonHash, cone.RuleLeaves, cone.DepthContent)
		if err != nil {
			return err
		}
		c, err := cone.NewWithHorizon([]string{"src"}, nil, h)
		if err != nil {
			return err
		}
		// Version 3, count 1, rule leaves, depth content, 40 hash digits,
		// then the one inclusion with its flag byte.
		want := append([]byte{0, 3, 1, 1, 0, 40}, horizonHash...)
		want = append(want, 0, 3, 0, 's', 'r', 'c')
		if got := c.Serialize(); !bytes.Equal(got, want) {
			return expectf("serialized % x, want % x", got, want)
		}
		back, err := cone.Parse(c.Serialize())
		if err != nil || !back.Equal(c) || back.Digest() != c.Digest() {
			return expectf("round trip gave %v, %v", back, err)
		}
		// The member is of a distinct kind: the same path members without a
		// horizon, or with another rule, digest differently.
		plain, err := cone.New([]string{"src"}, nil)
		if err != nil {
			return err
		}
		descend, err := cone.NewHorizon(horizonHash, cone.RuleDescendants, cone.DepthContent)
		if err != nil {
			return err
		}
		other, err := cone.NewWithHorizon([]string{"src"}, nil, descend)
		if err != nil {
			return err
		}
		if c.Digest() == plain.Digest() || c.Digest() == other.Digest() {
			return expectf("horizon did not change the digest")
		}
		// Not canonical: an upper-case hash is refused, not normalized.
		upper := bytes.Clone(want)
		upper[6] = 'A'
		if _, err := cone.Parse(upper); err == nil {
			return expectf("upper-case horizon hash accepted")
		}
		return nil
	})

	check(t, horizonUnknownVersion, func() error {
		// A version-2 cone with one inclusion, relabelled as each version
		// above the horizon's: a reader that skipped the version would see
		// "src".
		for v := byte(4); v < 8; v++ {
			forged := []byte{0, v, 1, 0, 3, 0, 's', 'r', 'c'}
			if c, err := cone.Parse(forged); err == nil {
				return expectf("serialized version %d read as %v", v, c)
			}
		}
		// Version 3 is the horizon's: a version-2 body under it is no
		// horizon and is refused, as is a malformed horizon.
		malformed := [][]byte{
			{0, 3, 1, 0, 3, 0, 's', 'r', 'c'},
			append([]byte{0, 3, 0, 2, 0, 40}, horizonHash...),
			append([]byte{0, 3, 0, 0, 2, 40}, horizonHash...),
			append([]byte{0, 3, 0, 0, 0, 39}, horizonHash[:39]...),
		}
		for _, forged := range malformed {
			if c, err := cone.Parse(forged); err == nil {
				return expectf("malformed version 3 % x read as %v", forged, c)
			}
		}
		return nil
	})
}
