package conformance

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
)

// The horizon suite tracks draft-fossil-horizon-00, none of which is
// implemented yet. Its one case is the half of §3.3 that binds an
// implementation predating the document: a cone serialized with a version
// go-libfossil does not know, as a cone carrying a horizon member will be,
// is refused rather than read as path members.

func horizonSuiteCases() []Case {
	return []Case{horizonUnknownVersion}
}

var horizonUnknownVersion = Case{ID: "horizon.3.3.unknown-version", Req: "HZ-3.3-2"}

func TestHorizon(t *testing.T) {
	check(t, horizonUnknownVersion, func() error {
		// A version-2 cone with one inclusion, relabelled as each later
		// version: a reader that skipped the version would see "src".
		for v := byte(3); v < 8; v++ {
			forged := []byte{0, v, 1, 0, 3, 0, 's', 'r', 'c'}
			if c, err := cone.Parse(forged); err == nil {
				return expectf("serialized version %d read as %v", v, c)
			}
		}
		return nil
	})
}
