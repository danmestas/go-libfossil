package manifest

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/content"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
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
func resolveMlinkParent(tx *db.Tx, fnid int64, primaryParentMid libfossil.FslID, mergeParentMids []libfossil.FslID) (pmid, pid int64, err error) {
	if tx == nil {
		panic("manifest.resolveMlinkParent: tx must not be nil")
	}
	if fnid <= 0 {
		panic("manifest.resolveMlinkParent: fnid must be positive")
	}
	if len(mergeParentMids) > maxMlinkMergeParents {
		panic("manifest.resolveMlinkParent: mergeParentMids exceeds bound")
	}

	if primaryParentMid <= 0 {
		return 0, 0, nil // no parent at all: file is new to this check-in
	}
	pmid = int64(primaryParentMid)

	var parentFid int64
	err = tx.QueryRow("SELECT fid FROM mlink WHERE mid=? AND fnid=?", primaryParentMid, fnid).Scan(&parentFid)
	switch {
	case err == nil:
		return pmid, parentFid, nil // carried over from the primary parent
	case !errors.Is(err, sql.ErrNoRows):
		return 0, 0, fmt.Errorf("lookup primary parent fid: %w", err)
	}

	// Not present in the primary parent's file set. If a merge parent
	// carried this filename, the file was added by the merge (pid=-1);
	// otherwise it is genuinely new to this check-in (pid=0).
	//
	// Known divergence from canonical: real Fossil's rule is not "present
	// in a merge parent" but count(*) < nLink over the per-fnid mlink rows
	// it writes for every parent transition (src/manifest.c:1905-1915). For
	// a file that exists in a merge parent with DIFFERENT content — i.e. a
	// conflict resolved during the merge — canonical emits an auxiliary
	// row and leaves pid=0, but this function returns pid=-1. Low blast
	// radius: every mlink consumer in this package (finfo.go, dephantomize.go)
	// filters on pid != fid, which holds either way. Left as-is rather than
	// tracking per-parent content equality, which the single-row-per-file
	// model this package uses does not otherwise need.
	for _, mp := range mergeParentMids {
		if mp <= 0 {
			continue
		}
		var exists int64
		err = tx.QueryRow("SELECT 1 FROM mlink WHERE mid=? AND fnid=?", mp, fnid).Scan(&exists)
		switch {
		case err == nil:
			return pmid, -1, nil // added by merge
		case !errors.Is(err, sql.ErrNoRows):
			return 0, 0, fmt.Errorf("lookup merge parent fid: %w", err)
		}
	}
	return pmid, 0, nil // added by this check-in
}

// insertMlinkRow inserts one mlink row, resolving pmid/pid via
// resolveMlinkParent and pfnid via oldName (empty if the file was not
// renamed). fid must be 0 when the check-in deletes the file. mperm is the
// raw Fossil permission string ("", "x", "l"); this function converts it.
// isaux is always written 0: neither write path in this package produces
// more than one mlink row per (mid, fnid), so the auxiliary/merge-parent
// row distinction canonical Fossil uses does not arise here.
func insertMlinkRow(tx *db.Tx, mid libfossil.FslID, fid int64, fnid int64, oldName string, perm string, primaryParentMid libfossil.FslID, mergeParentMids []libfossil.FslID) error {
	if tx == nil {
		panic("manifest.insertMlinkRow: tx must not be nil")
	}
	if mid <= 0 {
		panic("manifest.insertMlinkRow: mid must be positive")
	}
	if fnid <= 0 {
		panic("manifest.insertMlinkRow: fnid must be positive")
	}

	pmid, pid, err := resolveMlinkParent(tx, fnid, primaryParentMid, mergeParentMids)
	if err != nil {
		return fmt.Errorf("resolve mlink parent: %w", err)
	}

	var pfnid int64
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
