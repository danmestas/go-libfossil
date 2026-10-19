package manifest

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ReparentCheckins applies every singleton `parent` tag by rewriting the
// tagged check-in's ancestry to the parents the tag names. It is fossil's
// manifest_crosslink_end reparent pass (src/manifest.c
// manifest_reparent_checkin): the check-in's plink and mlink rows are deleted
// and re-derived against the tag's parents instead of its own P-card.
//
// It runs after the candidate sweep and before tag propagation, as fossil
// does, so the propagation walk and the batch's leaf rechecks see the
// rewritten plink. Like fossil's, the rewrite schedules no leaf recheck of its
// own (draft-fossil-repo-state-01 Section 7.3): an old parent left without a
// child is rechecked only when a branch write in the batch reaches it, and an
// explicit rebuild (leaf.Rebuild) always reaches it. A parent tag whose value
// is not a well-formed list of artifact hashes is ignored, matching fossil's
// reparent_abort.
//
// Each repair round scans tagxref for parent tags and rewrites only the tagged
// check-ins, so the pass is O(reparented), not O(repository); a round with no
// parent tag does one indexed query and returns. This matches fossil's
// manifest_crosslink_end, which the receive path's repair gate runs once per
// round rather than once per repository.
//
// The new primary parent of each rewritten check-in is recorded on parents,
// so the gate's propagation reaches the check-in under its new parent.
func ReparentCheckins(tx *db.Tx, parents *parentSet) error {
	if tx == nil {
		panic("manifest.ReparentCheckins: tx must not be nil")
	}
	if parents == nil {
		panic("manifest.ReparentCheckins: parents must not be nil")
	}
	type reparent struct {
		rid   libfossil.FslID
		value string
	}
	rows, err := tx.Query(`SELECT tagxref.rid, tagxref.value FROM tagxref
		 JOIN tag USING(tagid)
		 WHERE tag.tagname='parent' AND tagxref.tagtype=1`)
	if err != nil {
		return fmt.Errorf("manifest.ReparentCheckins query: %w", err)
	}
	var todo []reparent
	for rows.Next() {
		var r reparent
		var value sql.NullString
		if err := rows.Scan(&r.rid, &value); err != nil {
			rows.Close()
			return fmt.Errorf("manifest.ReparentCheckins scan: %w", err)
		}
		r.value = value.String
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("manifest.ReparentCheckins rows: %w", err)
	}
	if len(todo) == 0 {
		return nil // the common case: no reparent, no cache to build
	}
	cache := content.NewCache(crosslinkCacheBytes)
	for _, r := range todo {
		if err := reparentCheckin(tx, cache, r.rid, r.value, parents); err != nil {
			return err
		}
	}
	return nil
}

// reparentCheckin rewrites one check-in's plink and mlink to the parents named
// in value, a space-separated list of artifact hashes. The rows are derived by
// the same builders a normal crosslink uses, over a deck whose P-card is the
// override: fossil reuses manifest_add_checkin_linkages the same way.
func reparentCheckin(
	tx *db.Tx, cache *content.Cache, rid libfossil.FslID, value string, parents *parentSet,
) error {
	hashes := strings.Fields(value)
	if len(hashes) == 0 {
		return nil
	}
	for _, p := range hashes {
		if !isHash(p) {
			return nil // malformed value: fossil leaves the plink rows untouched
		}
	}
	data, err := expandManifestBytes(tx, cache, rid)
	if err != nil {
		return nil // unreadable check-in: nothing to rewrite
	}
	d, err := deck.Parse(data)
	if err != nil || d.Type != deck.Checkin {
		return nil
	}

	reparented := *d
	reparented.P = hashes

	if _, err := tx.Exec("DELETE FROM plink WHERE cid=?", rid); err != nil {
		return fmt.Errorf("manifest.reparentCheckin plink delete rid=%d: %w", rid, err)
	}
	if _, err := tx.Exec("DELETE FROM mlink WHERE mid=?", rid); err != nil {
		return fmt.Errorf("manifest.reparentCheckin mlink delete rid=%d: %w", rid, err)
	}
	if err := insertCheckinPlinks(tx, rid, &reparented, checkinBaseid(tx, d), parents); err != nil {
		return fmt.Errorf("manifest.reparentCheckin plink rid=%d: %w", rid, err)
	}
	if err := insertCheckinMlinks(tx, cache, rid, &reparented); err != nil {
		return fmt.Errorf("manifest.reparentCheckin mlink rid=%d: %w", rid, err)
	}
	return nil
}
