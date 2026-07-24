package manifest

import (
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// maxMlinkMergeParents bounds the merge-parent lookup loop in
// resolveMlinkParent. TigerStyle: every loop needs an explicit bound. A
// check-in with more merge parents than this is almost certainly corrupt
// input, not a legitimate octopus merge.
const maxMlinkMergeParents = 1024

// permToMperm converts a Fossil F-card permission string to the mlink.mperm
// encoding used by canonical Fossil (src/manifest.c add_one_mlink): 0 =
// regular file, 1 = executable, 2 = symlink.
//
// Canonical manifest_file_mperm (src/manifest.c:1482-1492) does a substring
// test (strstr), not an exact match: perm fields can carry more than one
// character (e.g. Fossil's " w" rename placeholder — see #51), and
// internal/deck/parse.go:194 assigns the F-card perm field verbatim from
// remote input over xfer. An exact match would silently drop the
// executable bit for any multi-character perm string containing "x", which
// is the exact invariant PR #48 landed to protect. x is tested before l to
// match canonical's check order.
func permToMperm(perm string) int64 {
	switch {
	case strings.Contains(perm, "x"):
		return 1
	case strings.Contains(perm, "l"):
		return 2
	default:
		return 0
	}
}

// resolveMlinkParent implements Fossil's mlink parent-column convention —
// the single place in this package that knows it. See
// src/manifest.c:1668-1679 (the add_mlink header comment) in canonical
// Fossil:
//
//   - a file carried over from the primary parent: pid = the parent file's
//     blob rid, pmid = the primary parent's manifest rid
//   - a file added by this check-in (absent from every parent): pid = 0
//   - a file added by a merge (absent from the primary parent, present in
//     a merge parent): pid = -1
//
// pmid is always the primary parent's manifest rid, or 0 if this check-in
// has no parent at all. Both insertMlinks (direct check-in) and
// insertCheckinMlinks (xfer/Crosslink ingestion) route every mlink row
// through this function so the two write paths cannot drift apart again.
// mlinkParents holds a check-in's parent file sets, loaded once by
// loadMlinkParents so resolveMlinkParent resolves each F-card with a map hit
// instead of an mlink query. The prior per-F-card `WHERE mid=? AND fnid=?`
// lookup let SQLite pick the fnid index, which returns a file's entire
// crosslinked history -- growing as the sweep advances -- so the whole-repo
// crosslink cost climbed with the mlink table's size. Loading each parent's
// file set with one mid-indexed scan and reading it F times is O(P+F) per
// check-in instead of O(F) growing scans, and reads exactly the same rows, so
// the mlink output is unchanged.
type mlinkParents struct {
	primaryMid libfossil.FslID
	primaryFid map[int64]int64    // fnid -> fid recorded by the primary parent
	mergeFnids map[int64]struct{} // fnids any merge parent recorded
}

// loadMlinkParents reads the primary parent's (fnid, fid) pairs and the union
// of merge parents' fnids into memory. mergeParentMids beyond maxMlinkMergeParents
// is rejected as corrupt input, matching resolveMlinkParent's old bound.
func loadMlinkParents(tx *db.Tx, primaryParentMid libfossil.FslID, mergeParentMids []libfossil.FslID) (*mlinkParents, error) {
	if tx == nil {
		panic("manifest.loadMlinkParents: tx must not be nil")
	}
	if len(mergeParentMids) > maxMlinkMergeParents {
		panic("manifest.loadMlinkParents: mergeParentMids exceeds bound")
	}
	mp := &mlinkParents{
		primaryMid: primaryParentMid,
		primaryFid: make(map[int64]int64),
		mergeFnids: make(map[int64]struct{}),
	}
	if primaryParentMid > 0 {
		rows, err := tx.Query("SELECT fnid, fid FROM mlink WHERE mid=?", primaryParentMid)
		if err != nil {
			return nil, fmt.Errorf("load primary parent mlinks: %w", err)
		}
		for rows.Next() {
			var fnid, fid int64
			if err := rows.Scan(&fnid, &fid); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan primary parent mlink: %w", err)
			}
			if _, dup := mp.primaryFid[fnid]; !dup {
				mp.primaryFid[fnid] = fid // first row wins, matching the old QueryRow
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("load primary parent mlinks: %w", err)
		}
		rows.Close()
	}
	for _, m := range mergeParentMids {
		if m <= 0 {
			continue
		}
		rows, err := tx.Query("SELECT fnid FROM mlink WHERE mid=?", m)
		if err != nil {
			return nil, fmt.Errorf("load merge parent mlinks: %w", err)
		}
		for rows.Next() {
			var fnid int64
			if err := rows.Scan(&fnid); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan merge parent mlink: %w", err)
			}
			mp.mergeFnids[fnid] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("load merge parent mlinks: %w", err)
		}
		rows.Close()
	}
	return mp, nil
}

// resolveMlinkParent implements Fossil's mlink parent-column convention against
// a check-in's preloaded parent file sets (mlinkParents) -- the single place in
// this package that knows it. See src/manifest.c:1668-1679 (the add_mlink header
// comment) in canonical Fossil:
//
//   - a file carried over from the primary parent: pid = the parent file's
//     blob rid, pmid = the primary parent's manifest rid
//   - a file added by this check-in (absent from every parent): pid = 0
//   - a file added by a merge (absent from the primary parent, present in
//     a merge parent): pid = -1
//
// pmid is always the primary parent's manifest rid, or 0 if this check-in has
// no parent at all. Both insertMlinks (direct check-in) and insertCheckinMlinks
// (xfer/Crosslink ingestion) route every mlink row through this function so the
// two write paths cannot drift apart again.
//
// Known divergence from canonical: real Fossil's rule for pid=-1 is not
// "present in a merge parent" but count(*) < nLink over the per-fnid mlink rows
// it writes for every parent transition (src/manifest.c:1905-1915). For a file
// that exists in a merge parent with DIFFERENT content -- a conflict resolved
// during the merge -- canonical emits an auxiliary row and leaves pid=0, but
// this function returns pid=-1. Low blast radius: every mlink consumer in this
// package (finfo.go, dephantomize.go) filters on pid != fid, which holds either
// way. Left as-is rather than tracking per-parent content equality.
func resolveMlinkParent(fnid int64, mp *mlinkParents) (pmid, pid int64) {
	if fnid <= 0 {
		panic("manifest.resolveMlinkParent: fnid must be positive")
	}
	if mp == nil {
		panic("manifest.resolveMlinkParent: mp must not be nil")
	}
	if mp.primaryMid <= 0 {
		return 0, 0 // no parent at all: file is new to this check-in
	}
	pmid = int64(mp.primaryMid)
	if fid, ok := mp.primaryFid[fnid]; ok {
		return pmid, fid // carried over from the primary parent
	}
	if _, ok := mp.mergeFnids[fnid]; ok {
		return pmid, -1 // added by merge
	}
	return pmid, 0 // added by this check-in
}

// insertMlinkRow inserts one mlink row, resolving pmid/pid via
// resolveMlinkParent and pfnid via oldName (empty if the file was not
// renamed). fid must be 0 when the check-in deletes the file. mperm is the
// raw Fossil permission string ("", "x", "l"); this function converts it.
// isaux is always written 0: neither write path in this package produces
// more than one mlink row per (mid, fnid), so the auxiliary/merge-parent
// row distinction canonical Fossil uses does not arise here.
func insertMlinkRow(tx *db.Tx, mid libfossil.FslID, fid int64, fnid int64, oldName string, perm string, parents *mlinkParents) error {
	if tx == nil {
		panic("manifest.insertMlinkRow: tx must not be nil")
	}
	if mid <= 0 {
		panic("manifest.insertMlinkRow: mid must be positive")
	}
	if fnid <= 0 {
		panic("manifest.insertMlinkRow: fnid must be positive")
	}

	pmid, pid := resolveMlinkParent(fnid, parents)

	var pfnid int64
	var err error
	if oldName != "" {
		pfnid, err = ensureFilename(tx, oldName)
		if err != nil {
			return fmt.Errorf("prior filename %q: %w", oldName, err)
		}
	}

	if _, err := tx.Exec(
		"INSERT INTO mlink(mid, fid, pmid, pid, fnid, pfnid, mperm, isaux) VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
		mid, fid, pmid, pid, fnid, pfnid, permToMperm(perm), 0,
	); err != nil {
		return fmt.Errorf("mlink: %w", err)
	}

	// Store the file's previous version as a delta against this one. Both
	// write paths in this package funnel through here, so this is the only
	// place file content is deltified. Mirrors add_mlink's tail in canonical
	// Fossil (src/manifest.c:1562, `if( pid && fid ) content_deltify(pid,
	// &fid, 1, 0)`); content.Deltify holds the policy and declines on its
	// own when the pair is not worth encoding.
	if pid > 0 && fid > 0 {
		if _, err := content.Deltify(tx, libfossil.FslID(pid), libfossil.FslID(fid)); err != nil {
			return fmt.Errorf("deltify prior file version: %w", err)
		}
	}
	return nil
}
