package libfossil

import (
	"fmt"

	fsltype "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// EnableSubmanifests turns on tree manifest emission
// (draft-fossil-submanifests-00 §8.1): from the next commit, check-ins carry
// their file sets as sub-manifest trees. A pinned partition parameter is kept,
// and an explicit k (1 to 12) that differs from it is refused, since
// changing it is a re-tune. With no pin, k is pinned as given, or, when k is
// 0, selected over the file set of current, the check-in the next commit
// descends from (0 for none); a set no k partitions is refused. A refused
// request changes nothing. It returns the pinned k and the warning the
// operator must be shown: trees cannot be retired, and peers older than
// Fossil 2.29 are refused them.
func (r *Repo) EnableSubmanifests(current int64, k int) (pinned int, warning string, err error) {
	pinned, err = manifest.EnableSubmanifests(r.inner, fsltype.FslID(current), k)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: enable sub-manifests: %w", err)
	}
	return pinned, manifest.SubmanifestEnableWarning, nil
}

// RetuneSubmanifests pins a new partition parameter k, 1 to 12, for the
// commits that follow (draft-fossil-submanifests-00 §6.4). Existing
// check-ins are unchanged; the next tree shares only the sub-manifests whose
// bytes are the same under both values.
func (r *Repo) RetuneSubmanifests(k int) error {
	if err := manifest.RetuneSubmanifests(r.inner, k); err != nil {
		return fmt.Errorf("libfossil: re-tune sub-manifests: %w", err)
	}
	return nil
}
