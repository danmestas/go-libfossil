package verify

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
)

// rebuildTags reconstructs tagxref rows from inline T-cards in checkin
// manifests and from control artifacts. It runs inside the single rebuild
// transaction, so it uses tag.ApplyTagWithTx to avoid nested transactions.
//
// Precondition: rebuildManifests has already populated event/plink/mlink/filename.
// The plink graph must exist before tag propagation can walk descendants.
func rebuildTags(_ *repo.Repo, tx *db.Tx, report *Report, cache *content.Cache) error {
	if tx == nil {
		panic("rebuildTags: nil *db.Tx")
	}
	if report == nil {
		panic("rebuildTags: nil *Report")
	}

	entries, err := collectBlobEntries(tx)
	if err != nil {
		return err
	}

	// Pass 1: process checkin manifests (inline T-cards where UUID == "*").
	// Both passes re-expand every blob; sharing checkBlobs'/rebuildManifests'
	// cache means each blob's chain is walked once for the whole rebuild, not
	// once per pass.
	for _, e := range entries {
		data, err := cache.Expand(tx, e.rid)
		if err != nil {
			continue // already counted in BlobsSkipped by rebuildManifests
		}
		d, err := deck.Parse(data)
		if err != nil {
			continue
		}
		if d.Type != deck.Checkin {
			continue
		}
		if err := rebuildInlineTags(tx, e.rid, d); err != nil {
			return fmt.Errorf("rebuildTags inline rid=%d: %w", e.rid, err)
		}
	}

	// Pass 2: process control artifacts (T-cards with explicit target UUID).
	for _, e := range entries {
		data, err := cache.Expand(tx, e.rid)
		if err != nil {
			continue // already counted in BlobsSkipped by rebuildManifests
		}
		d, err := deck.Parse(data)
		if err != nil {
			continue
		}
		if d.Type != deck.Control {
			continue
		}
		if err := rebuildControlTags(tx, e.rid, d); err != nil {
			return fmt.Errorf("rebuildTags control rid=%d: %w", e.rid, err)
		}
	}

	return nil
}

// rebuildInlineTags processes inline T-cards (UUID == "*") from a checkin manifest.
func rebuildInlineTags(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("rebuildInlineTags: nil *db.Tx")
	}
	if d == nil {
		panic("rebuildInlineTags: nil *deck.Deck")
	}

	mtime := libfossil.TimeToJulian(d.D)

	for _, tc := range d.T {
		if tc.UUID != "*" {
			continue
		}
		tagType, ok := mapDeckTagType(tc.Type)
		if !ok {
			continue
		}
		if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
			TargetRID: rid,
			SrcRID:    rid, // inline: checkin is its own source
			TagName:   tc.Name,
			TagType:   tagType,
			Value:     tc.Value,
			MTime:     mtime,
		}); err != nil {
			return fmt.Errorf("inline tag %q: %w", tc.Name, err)
		}
	}
	return nil
}

// rebuildControlTags processes T-cards from a control artifact.
func rebuildControlTags(tx *db.Tx, srcRID libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("rebuildControlTags: nil *db.Tx")
	}
	if d == nil {
		panic("rebuildControlTags: nil *deck.Deck")
	}

	mtime := libfossil.TimeToJulian(d.D)

	for _, tc := range d.T {
		if tc.UUID == "*" {
			continue // self-referencing — handled by rebuildInlineTags
		}
		targetRID, ok := blob.Exists(tx, tc.UUID)
		if !ok {
			continue // target not found — phantom or not yet received
		}
		tagType, ok := mapDeckTagType(tc.Type)
		if !ok {
			continue
		}
		if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
			TargetRID: targetRID,
			SrcRID:    srcRID,
			TagName:   tc.Name,
			TagType:   tagType,
			Value:     tc.Value,
			MTime:     mtime,
		}); err != nil {
			return fmt.Errorf("control tag %q on rid=%d: %w", tc.Name, targetRID, err)
		}
	}
	return nil
}

// mapDeckTagType converts a deck.TagType to a tag package constant.
// Returns (tagType, true) on success, (0, false) for unrecognized types.
func mapDeckTagType(dt deck.TagType) (int, bool) {
	switch dt {
	case deck.TagPropagating:
		return tag.TagPropagating, true
	case deck.TagSingleton:
		return tag.TagSingleton, true
	case deck.TagCancel:
		return tag.TagCancel, true
	default:
		return 0, false
	}
}
