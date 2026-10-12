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
// does, so the caller's repairLeafTable sees the rewritten plink and makes a
// now-childless old parent a leaf. A parent tag whose value is not a
// well-formed list of artifact hashes is ignored, matching fossil's
// reparent_abort.
//
// Each repair round scans tagxref for parent tags and rewrites only the tagged
// check-ins, so the pass is O(reparented), not O(repository); a round with no
// parent tag does one indexed query and returns. This matches fossil's
// manifest_crosslink_end, which the receive path's repair gate runs once per
// round rather than once per repository.
func ReparentCheckins(tx *db.Tx) error {
	if tx == nil {
		panic("manifest.ReparentCheckins: tx must not be nil")
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
		if err := reparentCheckin(tx, cache, r.rid, r.value); err != nil {
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
	tx *db.Tx, cache *content.Cache, rid libfossil.FslID, value string,
) error {
	parents := strings.Fields(value)
	if len(parents) == 0 {
		return nil
	}
	for _, p := range parents {
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
	reparented.P = parents

	if _, err := tx.Exec("DELETE FROM plink WHERE cid=?", rid); err != nil {
		return fmt.Errorf("manifest.reparentCheckin plink delete rid=%d: %w", rid, err)
	}
	if _, err := tx.Exec("DELETE FROM mlink WHERE mid=?", rid); err != nil {
		return fmt.Errorf("manifest.reparentCheckin mlink delete rid=%d: %w", rid, err)
	}
	if err := insertCheckinPlinks(tx, rid, &reparented, checkinBaseid(tx, d)); err != nil {
		return fmt.Errorf("manifest.reparentCheckin plink rid=%d: %w", rid, err)
	}
	if err := insertCheckinMlinks(tx, cache, rid, &reparented); err != nil {
		return fmt.Errorf("manifest.reparentCheckin mlink rid=%d: %w", rid, err)
	}
	return nil
}
