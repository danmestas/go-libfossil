package libfossil

import (
	"errors"
	"fmt"

	fsltype "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// EnableSubmanifests turns on tree manifest emission
// (draft-fossil-submanifests-00 §8.1): from the next commit, check-ins carry
// their file sets as sub-manifest trees. k is the partition parameter, 1 to
// 12, or 0 to select one. A valid pinned k is kept, and an explicit k that
// differs from it is refused: changing it is RetuneSubmanifests. With no
// valid pin, an explicit k is pinned, or one is selected as zit selects it:
// the §6.3 cost over one check-in's file set, refined by replaying recent
// history. The check-in analyzed is the current trunk (CurrentTrunk, the
// main-branch tip, which the next commit most plausibly descends from);
// when no check-in is on the main branch, it is the newest check-in by
// time, the highest rid breaking a tie. A request that cannot be met -- k
// outside 1..12, no check-in at all to select from, or a file set no k
// partitions -- changes neither setting. It returns the pinned k and the
// warning the operator must be shown: peers older than Fossil 2.29 are
// refused trees, and trees cannot be retired.
func (r *Repo) EnableSubmanifests(k int) (pinned int, warning string, err error) {
	current, err := r.CurrentTrunk()
	if errors.Is(err, ErrVersionNotFound) {
		var newest fsltype.FslID
		newest, err = manifest.NewestCheckin(r.inner.DB())
		current = int64(newest)
	}
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: enable sub-manifests: %w", err)
	}
	pinned, err = manifest.EnableSubmanifests(r.inner, fsltype.FslID(current), k)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: enable sub-manifests: %w", err)
	}
	return pinned, manifest.SubmanifestEnableWarning, nil
}

// RetuneSubmanifests pins a new partition parameter k, 1 to 12, for the
// commits that follow (draft-fossil-submanifests-00 §6.4). It is forward
// only: existing check-ins are unchanged, and the next tree shares only the
// sub-manifests whose bytes are the same under both values.
func (r *Repo) RetuneSubmanifests(k int) error {
	if err := manifest.RetuneSubmanifests(r.inner, k); err != nil {
		return fmt.Errorf("libfossil: re-tune sub-manifests: %w", err)
	}
	return nil
}

// DisableSubmanifests turns tree manifest emission off: later commits are
// flat, and the pinned k is kept for a later EnableSubmanifests. Existing
// trees remain -- artifact names are immutable, so they cannot be retired --
// and keep being served, though peers older than Fossil 2.29 are still
// refused them. Unlike the CLI's disable, it returns no note saying so; an
// embedder that wants to tell its operator must do it itself.
func (r *Repo) DisableSubmanifests() error {
	if _, err := manifest.DisableSubmanifests(r.inner); err != nil {
		return fmt.Errorf("libfossil: disable sub-manifests: %w", err)
	}
	return nil
}
