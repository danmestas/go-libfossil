package manifest

// mlink derivation for check-in manifests, ported from canonical Fossil's
// src/manifest.c (add_one_mlink, add_mlink, manifest_add_checkin_linkages).
//
// The shape matters as much as the individual rules: canonical does not diff a
// check-in against its primary parent alone. It runs the SAME parent-to-child
// diff once per parent transition -- the primary parent, then every merge
// parent, then every cherrypick ('+') source -- and records the merge-parent
// rows with isaux=1. Those auxiliary rows are 4% of a real repository's mlink
// table and are what `fossil rebuild` used to add back on top of a libfossil
// clone (issue #193).
//
// Two consequences fall out of running the diff per parent rather than once:
//
//   - "added by merge" (pid=-1) is not a lookup against the merge parents' file
//     names. It is a count: a file that produced fewer rows than there are
//     parent transitions did not change relative to every parent, so it came in
//     with a merge. See fixMergeAddedPids.
//   - A parent whose manifest cannot be read (a phantom, or an artifact that
//     does not parse) yields NO rows at all, not "every file is new". The rows
//     are derived later, from the parent's side, once it arrives -- see
//     linkPlinkChildren.

import (
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// maxMlinkRecursionDepth bounds addMlink's self-call. Canonical recurses
// exactly one level -- the primary-parent pass fans out to merge parents and
// cherrypick sources, and those passes never recurse further -- so anything
// deeper is a bug in this file rather than unusual input.
const maxMlinkRecursionDepth = 2

// manifestFiles is a check-in manifest's file tree in the two shapes canonical
// Fossil's add_mlink reads it through: the manifest's OWN F-cards
// (manifest_file_seek_base) and the effective tree a delta manifest presents
// once its baseline is merged in (manifest_file_seek / manifest_file_next).
//
// The distinction is load-bearing. add_mlink's delta-vs-delta pass asks "does
// the child have its own card for this name?" (own) and "what does the child
// resolve this name to?" (effective) about the same name, and gets different
// answers when the child reverted a file to its baseline content.
type manifestFiles struct {
	isDelta bool
	// own is this manifest's own F-cards keyed by name, deletion cards (empty
	// UUID) included -- manifest_file_seek_base returns those too.
	own map[string]deck.FileCard
	// ownNames is own's keys in sorted order, so the delta-vs-delta pass walks
	// the parent's cards deterministically.
	ownNames []string
	// tree is the effective file set: the baseline's cards overlaid with own's,
	// with deletion cards removing the name. Empty-UUID entries never appear.
	//
	// A rename's PRIOR name is deliberately left in place: canonical's
	// manifest_file_seek finds it in the baseline (a rename card lives under the
	// new name and does not remove the old one from the baseline), and the
	// delete row for the vacated name comes from the child's own explicit
	// deletion card, not from inference here.
	tree map[string]deck.FileCard
}

// loadManifestFiles builds the file views for one parsed check-in manifest,
// expanding its baseline when it is a delta manifest. A delta manifest whose
// baseline is absent or unparseable is an error: canonical's fetch_baseline
// failure makes add_mlink return without writing a row, and the caller must be
// able to tell that apart from "the manifest has no files".
func loadManifestFiles(q db.Querier, cache *content.Cache, d *deck.Deck) (*manifestFiles, error) {
	if q == nil {
		panic("manifest.loadManifestFiles: q must not be nil")
	}
	if d == nil {
		panic("manifest.loadManifestFiles: d must not be nil")
	}

	mf := &manifestFiles{
		isDelta: d.B != "",
		own:     make(map[string]deck.FileCard, len(d.F)),
		tree:    make(map[string]deck.FileCard, len(d.F)),
	}

	if mf.isDelta {
		baseRid, ok := content.AvailableByUUID(q, d.B)
		if !ok {
			return nil, fmt.Errorf("baseline %s not found", d.B)
		}
		baseData, err := expandManifestBytes(q, cache, baseRid)
		if err != nil {
			return nil, fmt.Errorf("expand baseline: %w", err)
		}
		baseDeck, err := deck.Parse(baseData)
		if err != nil {
			return nil, fmt.Errorf("parse baseline: %w", err)
		}
		for _, f := range baseDeck.F {
			if f.UUID == "" {
				delete(mf.tree, f.Name)
				continue
			}
			mf.tree[f.Name] = f
		}
	}

	for _, f := range d.F {
		mf.own[f.Name] = f
		if f.UUID == "" {
			delete(mf.tree, f.Name)
			continue
		}
		mf.tree[f.Name] = f
	}

	mf.ownNames = make([]string, 0, len(mf.own))
	for name := range mf.own {
		mf.ownNames = append(mf.ownNames, name)
	}
	sort.Strings(mf.ownNames)
	return mf, nil
}

// seekBase is canonical manifest_file_seek_base: the manifest's own card for
// name, deletion cards included.
func (mf *manifestFiles) seekBase(name string) (deck.FileCard, bool) {
	f, ok := mf.own[name]
	return f, ok
}

// seek is canonical manifest_file_seek: what the manifest resolves name to,
// with a deletion card reading as "absent" rather than falling through to the
// baseline.
func (mf *manifestFiles) seek(name string) (deck.FileCard, bool) {
	f, ok := mf.tree[name]
	return f, ok
}

// treeNames returns the effective tree's names in sorted order -- canonical's
// manifest_file_rewind/manifest_file_next walk, which yields the merged
// baseline-plus-delta set with deletions skipped.
func (mf *manifestFiles) treeNames() []string {
	names := make([]string, 0, len(mf.tree))
	for name := range mf.tree {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// addOneMlink is canonical add_one_mlink (src/manifest.c). fromUUID is the
// file's content in the parent ("" for a file the parent did not have),
// toUUID its content in the child ("" when the child deletes it), and prior
// the file's name in the parent when this row records a rename.
//
// isPrimary false means pmid is a merge parent of mid, and canonical only
// records such a row when the primary parent already produced one for the same
// file: a merge parent's view of a file nobody touched is not news. The row is
// then flagged isaux so the primary-parent rows stay distinguishable.
func addOneMlink(
	tx *db.Tx,
	pmid libfossil.FslID,
	fromUUID string,
	mid libfossil.FslID,
	toUUID string,
	filename string,
	prior string,
	isPrimary bool,
	mperm int64,
) error {
	if tx == nil {
		panic("manifest.addOneMlink: tx must not be nil")
	}
	if mid <= 0 {
		panic("manifest.addOneMlink: mid must be positive")
	}
	if filename == "" {
		panic("manifest.addOneMlink: filename must not be empty")
	}

	fnid, err := ensureFilename(tx, filename)
	if err != nil {
		return fmt.Errorf("filename %q: %w", filename, err)
	}
	var pfnid int64
	if prior != "" {
		pfnid, err = ensureFilename(tx, prior)
		if err != nil {
			return fmt.Errorf("prior filename %q: %w", prior, err)
		}
	}
	pid, err := ridOrPhantom(tx, fromUUID)
	if err != nil {
		return fmt.Errorf("reserve source content %q for file %q: %w", fromUUID, filename, err)
	}
	fid, err := ridOrPhantom(tx, toUUID)
	if err != nil {
		return fmt.Errorf("reserve target content %q for file %q: %w", toUUID, filename, err)
	}

	doInsert := true
	if !isPrimary {
		var one int
		doInsert = tx.QueryRow(
			"SELECT 1 FROM mlink WHERE mid=? AND fnid=? AND NOT isaux", mid, fnid,
		).Scan(&one) == nil
	}
	if doInsert {
		isaux := 0
		if !isPrimary {
			isaux = 1
		}
		if _, err := tx.Exec(
			"INSERT INTO mlink(mid, fid, pmid, pid, fnid, pfnid, mperm, isaux) VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
			mid, fid, pmid, pid, fnid, pfnid, mperm, isaux,
		); err != nil {
			return fmt.Errorf("mlink: %w", err)
		}
	}

	// Store the file's previous version as a delta against this one, exactly
	// where canonical does it (src/manifest.c add_one_mlink tail) -- outside the
	// doInsert branch, so a merge-parent transition that writes no row still
	// gets its content pair encoded. A phantom reserves the mlink pointer but
	// has no bytes, so Deltify only receives present source and target blobs.
	if fromUUID != "" && toUUID != "" {
		var sourceSize, targetSize int64
		if err := tx.QueryRow(
			"SELECT (SELECT size FROM blob WHERE uuid=?), (SELECT size FROM blob WHERE uuid=?)",
			fromUUID, toUUID,
		).Scan(&sourceSize, &targetSize); err != nil {
			return fmt.Errorf("file content availability for %q: %w", filename, err)
		}
		if sourceSize >= 0 && targetSize >= 0 {
			if _, err := content.Deltify(tx, libfossil.FslID(pid), libfossil.FslID(fid)); err != nil {
				return fmt.Errorf("deltify prior file version: %w", err)
			}
		}
	}
	return nil
}

// ridForUUID resolves a check-in artifact hash for addMlink's merge and
// cherrypick parents. It returns 0 when the hash is empty or its artifact has
// not arrived, leaving those transitions to be derived when it does.
//
// File-content pointers use ridOrPhantom in addOneMlink instead, preserving a
// non-empty missing UUID as a phantom mlink RID.
func ridForUUID(q db.Querier, uuid string) int64 {
	if uuid == "" {
		return 0
	}
	var rid int64
	if err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		return 0
	}
	return rid
}

// addMlink is canonical add_mlink: record one parent-to-child transition in
// mlink, emitting a row for every file whose content, name, or permissions
// differ between pmid and mid.
//
// child/childFiles describe mid and may both be nil, in which case mid's
// manifest is loaded here -- that is the "parent arrived after its child" entry
// point (linkPlinkChildren), and it is also where canonical's guard against
// deriving merge-parent rows before the primary parent exists applies.
//
// A pmid of 0, a parent whose content cannot be expanded or parsed, or a delta
// manifest on either side whose baseline is missing, all return without writing
// anything. That is canonical's behaviour and it is deliberate: a check-in
// whose primary parent is a phantom has NO mlink rows until the phantom fills,
// rather than a full tree's worth of bogus "added" rows.
func addMlink(
	tx *db.Tx,
	cache *content.Cache,
	pmid libfossil.FslID,
	mid libfossil.FslID,
	child *deck.Deck,
	childFiles *manifestFiles,
	isPrim bool,
	depth int,
) error {
	if tx == nil {
		panic("manifest.addMlink: tx must not be nil")
	}
	if mid <= 0 {
		panic("manifest.addMlink: mid must be positive")
	}
	if depth >= maxMlinkRecursionDepth {
		panic("manifest.addMlink: recursion depth exceeded")
	}
	if pmid <= 0 {
		return nil // canonical: content_get(0) yields nothing, add_mlink returns
	}

	// Already derived for this exact transition: canonical's first act.
	var one int
	if err := tx.QueryRow("SELECT 1 FROM mlink WHERE mid=? AND pmid=?", mid, pmid).Scan(&one); err == nil {
		return nil
	}

	childLoaded := false
	if child == nil || childFiles == nil {
		var err error
		child, childFiles, err = loadCheckinManifest(tx, cache, mid)
		if err != nil {
			return nil // unreadable child: canonical returns without writing
		}
		childLoaded = true
	}

	_, parentFiles, err := loadCheckinManifest(tx, cache, pmid)
	if err != nil {
		return nil // phantom or unparseable parent: canonical returns
	}

	// A merge-parent transition derived from the child's side is deferred while
	// the child's primary parent is still a phantom: the primary pass must run
	// first so addOneMlink's "is there already a non-aux row" test can see it.
	if !isPrim && childLoaded && len(child.P) > 0 && !blobHasContent(tx, child.P[0]) {
		return nil
	}

	for _, f := range child.F {
		mperm := permToMperm(f.Perm)
		if f.OldName != "" {
			// A rename whose prior name this parent has is a rename relative to
			// it; a rename from a name this parent never had is just a new file.
			pf, ok := parentFiles.seek(f.OldName)
			prior := f.OldName
			if !ok {
				prior = ""
			}
			if err := addOneMlink(tx, pmid, pf.UUID, mid, f.UUID, f.Name, prior, isPrim, mperm); err != nil {
				return err
			}
			continue
		}
		pf, ok := parentFiles.seek(f.Name)
		switch {
		case !ok:
			if f.UUID == "" {
				continue // deletion card for a file this parent did not have
			}
			if err := addOneMlink(tx, pmid, "", mid, f.UUID, f.Name, "", isPrim, mperm); err != nil {
				return err
			}
		case pf.UUID != f.UUID || permToMperm(pf.Perm) != mperm:
			if err := addOneMlink(tx, pmid, pf.UUID, mid, f.UUID, f.Name, "", isPrim, mperm); err != nil {
				return err
			}
		}
	}

	switch {
	case parentFiles.isDelta && childFiles.isDelta:
		// Both sides are delta manifests, so neither one's cards mention a file
		// that reverted to the shared baseline. Walk the parent's own cards to
		// find files it changed or deleted that the child puts back.
		for _, name := range parentFiles.ownNames {
			pf := parentFiles.own[name]
			if pf.UUID != "" {
				if _, ownsIt := childFiles.seekBase(name); ownsIt {
					continue // the child has its own card: handled above
				}
				cf, ok := childFiles.seek(name)
				if !ok {
					continue
				}
				// The child reverts to baseline: a change relative to pmid.
				if err := addOneMlink(tx, pmid, pf.UUID, mid, cf.UUID, cf.Name, "", isPrim, permToMperm(cf.Perm)); err != nil {
					return err
				}
				continue
			}
			// The parent deleted this file; the child still has it, so the child
			// resurrected it -- an add relative to pmid.
			if cf, ok := childFiles.seek(name); ok {
				if err := addOneMlink(tx, pmid, "", mid, cf.UUID, cf.Name, "", isPrim, permToMperm(cf.Perm)); err != nil {
					return err
				}
			}
		}
	case !childFiles.isDelta:
		// The child is a baseline manifest, so it lists its whole tree and a
		// file missing from it was dropped by omission rather than by a card.
		// mperm is 0 on a delete row, matching canonical.
		for _, name := range parentFiles.treeNames() {
			if _, ok := childFiles.seek(name); ok {
				continue
			}
			pf := parentFiles.tree[name]
			if pf.UUID == "" {
				continue
			}
			if err := addOneMlink(tx, pmid, pf.UUID, mid, "", name, "", isPrim, 0); err != nil {
				return err
			}
		}
	}

	if !isPrim {
		return nil
	}
	// The same diff, once per remaining parent transition: merge parents first,
	// then cherrypick sources. Both are skipped when the artifact has not
	// arrived (canonical passes 0 to uuid_to_rid here, so no phantom is made).
	for i := 1; i < len(child.P); i++ {
		mergeMid := ridForUUID(tx, child.P[i])
		if mergeMid <= 0 {
			continue
		}
		if err := addMlink(tx, cache, libfossil.FslID(mergeMid), mid, child, childFiles, false, depth+1); err != nil {
			return err
		}
	}
	for _, cp := range child.Q {
		if cp.IsBackout {
			continue // only cherrypick ('+') sources, not backouts
		}
		cpMid := ridForUUID(tx, cp.Target)
		if cpMid <= 0 {
			continue
		}
		if err := addMlink(tx, cache, libfossil.FslID(cpMid), mid, child, childFiles, false, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// loadCheckinManifest expands, parses, and builds the file views for the
// check-in manifest stored at rid.
func loadCheckinManifest(q db.Querier, cache *content.Cache, rid libfossil.FslID) (*deck.Deck, *manifestFiles, error) {
	data, err := expandManifestBytes(q, cache, rid)
	if err != nil {
		return nil, nil, fmt.Errorf("expand manifest rid=%d: %w", rid, err)
	}
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("manifest rid=%d is empty", rid)
	}
	d, err := deck.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse manifest rid=%d: %w", rid, err)
	}
	mf, err := loadManifestFiles(q, cache, d)
	if err != nil {
		return nil, nil, fmt.Errorf("files of rid=%d: %w", rid, err)
	}
	return d, mf, nil
}

// blobHasContent reports whether uuid names a blob whose content has arrived --
// canonical's `size>0` phantom test.
func blobHasContent(q db.Querier, uuid string) bool {
	if uuid == "" {
		return false
	}
	var one int
	return q.QueryRow("SELECT 1 FROM blob WHERE uuid=? AND size>0", uuid).Scan(&one) == nil
}

// validateCheckinMlinkBounds rejects manifest parent sources that would make
// mlink derivation exceed its fixed work bound.
func validateCheckinMlinkBounds(d *deck.Deck) error {
	if d == nil {
		return fmt.Errorf("checkin mlink bounds: deck must not be nil")
	}
	if len(d.P) > maxMlinkMergeParents+1 {
		excess := d.P[maxMlinkMergeParents+1]
		return fmt.Errorf("merge parent %s: exceeds maximum of %d", excess, maxMlinkMergeParents)
	}
	if len(d.Q) > maxMlinkMergeParents {
		excess := d.Q[maxMlinkMergeParents]
		return fmt.Errorf("cherrypick source %s: exceeds maximum of %d", excess.Target, maxMlinkMergeParents)
	}
	return nil
}

// preflightCheckinMlinks verifies that direct Checkin can derive every
// parent-to-child mlink transition before it stores the child manifest.
func preflightCheckinMlinks(tx *db.Tx, cache *content.Cache, d *deck.Deck) error {
	if tx == nil {
		panic("manifest.preflightCheckinMlinks: tx must not be nil")
	}
	if cache == nil {
		panic("manifest.preflightCheckinMlinks: cache must not be nil")
	}
	if d == nil {
		panic("manifest.preflightCheckinMlinks: deck must not be nil")
	}

	if err := validateCheckinMlinkBounds(d); err != nil {
		return err
	}

	if _, err := loadManifestFiles(tx, cache, d); err != nil {
		return fmt.Errorf("child effective tree: %w", err)
	}

	checkSource := func(kind, uuid string) error {
		rid, ok := content.AvailableByUUID(tx, uuid)
		if !ok {
			return fmt.Errorf("%s %s: not available", kind, uuid)
		}
		source, files, err := loadCheckinManifest(tx, cache, rid)
		if err != nil {
			return fmt.Errorf("%s %s: load manifest: %w", kind, uuid, err)
		}
		if source.Type != deck.Checkin {
			return fmt.Errorf("%s %s: expected checkin manifest, got %v", kind, uuid, source.Type)
		}
		for _, name := range files.treeNames() {
			file, _ := files.seek(name)
			if _, ok := content.AvailableByUUID(tx, file.UUID); !ok {
				return fmt.Errorf("%s %s: effective file %q (%s): not available", kind, uuid, name, file.UUID)
			}
		}
		return nil
	}

	for i := range d.P {
		kind := "merge parent"
		if i == 0 {
			kind = "primary parent"
		}
		if err := checkSource(kind, d.P[i]); err != nil {
			return err
		}
	}

	for _, cp := range d.Q {
		if cp.IsBackout {
			continue
		}
		if err := checkSource("cherrypick source", cp.Target); err != nil {
			return err
		}
	}
	return nil
}

// DeriveCheckinMlinks derives the mlink rows owned by a check-in.
func DeriveCheckinMlinks(tx *db.Tx, cache *content.Cache, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("manifest.DeriveCheckinMlinks: tx must not be nil")
	}
	if cache == nil {
		panic("manifest.DeriveCheckinMlinks: cache must not be nil")
	}
	if rid <= 0 {
		panic("manifest.DeriveCheckinMlinks: rid must be positive")
	}
	if d == nil {
		panic("manifest.DeriveCheckinMlinks: d must not be nil")
	}
	return insertCheckinMlinks(tx, cache, rid, d)
}

// insertCheckinMlinks derives every mlink row a check-in owns: the primary
// parent transition (which fans out to the merge parents and cherrypick
// sources), the "added by merge" fixup, and the transitions of any child that
// was crosslinked before this check-in arrived. It is canonical Fossil's
// manifest_add_checkin_linkages minus the plink half, which
// insertCheckinPlinks already owns.
func insertCheckinMlinks(tx *db.Tx, cache *content.Cache, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("manifest.insertCheckinMlinks: tx must not be nil")
	}
	if rid <= 0 {
		panic("manifest.insertCheckinMlinks: rid must be positive")
	}
	if d == nil {
		panic("manifest.insertCheckinMlinks: d must not be nil")
	}

	if err := validateCheckinMlinkBounds(d); err != nil {
		return err
	}

	childFiles, err := loadManifestFiles(tx, cache, d)
	if err != nil {
		// A delta manifest whose baseline has not arrived: no file view, so no
		// rows. The check-in is deferred before reaching here in the normal
		// path; this keeps the cascade path from inventing rows.
		return nil
	}

	primaryMid, _ := resolveParentMids(tx, d)
	if err := addMlink(tx, cache, primaryMid, rid, d, childFiles, true, 0); err != nil {
		return err
	}

	if err := fixMergeAddedPids(tx, rid, d); err != nil {
		return err
	}
	if err := linkPlinkChildren(tx, cache, rid); err != nil {
		return err
	}

	if len(d.P) == 0 {
		// A root check-in has no transition to diff, so every file it carries is
		// new: canonical's `if( nParent==0 )` block, with pmid and pid both 0.
		for _, f := range d.F {
			if f.UUID == "" {
				continue
			}
			if err := addOneMlink(tx, 0, "", rid, f.UUID, f.Name, "", true, permToMperm(f.Perm)); err != nil {
				return err
			}
		}
	}
	return nil
}

// fixMergeAddedPids rewrites pid from 0 to -1 for the files a check-in gained
// through a merge rather than by adding them itself.
//
// Canonical's rule is a count, not a name lookup (src/manifest.c
// manifest_add_checkin_linkages): a check-in with nLink parent transitions
// emits one row per transition for a file it genuinely changed, so a file with
// FEWER rows than that did not change relative to every parent -- it arrived
// with one of them. nLink counts the P-card parents plus the cherrypick ('+')
// sources whether or not their artifacts are present locally, matching
// canonical exactly.
func fixMergeAddedPids(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("manifest.fixMergeAddedPids: tx must not be nil")
	}
	if d == nil {
		panic("manifest.fixMergeAddedPids: d must not be nil")
	}
	nLink := len(d.P)
	for _, cp := range d.Q {
		if !cp.IsBackout {
			nLink++
		}
	}
	if nLink <= 1 {
		return nil
	}
	if _, err := tx.Exec(
		`UPDATE mlink SET pid=-1
		  WHERE mid=?
		    AND pid=0
		    AND fnid IN (SELECT fnid FROM mlink WHERE mid=? GROUP BY fnid HAVING count(*)<?)`,
		rid, rid, nLink,
	); err != nil {
		return fmt.Errorf("mlink merge-added pid: %w", err)
	}
	return nil
}

// linkPlinkChildren derives the transitions that could not be derived when a
// child of rid was crosslinked because rid itself was still missing.
//
// This is what makes the derivation independent of the order artifacts are
// visited in. The sweep walks candidates in delta-chain order, and a phantom
// parent may fill rounds after its children linked; canonical handles both with
// the same loop over plink (src/manifest.c manifest_add_checkin_linkages), and
// addMlink's early-out makes re-visiting an already-derived transition free.
func linkPlinkChildren(tx *db.Tx, cache *content.Cache, rid libfossil.FslID) error {
	if tx == nil {
		panic("manifest.linkPlinkChildren: tx must not be nil")
	}
	// plink.isprim is declared BOOLEAN, and one of the two SQLite drivers this
	// package builds against surfaces that as a Go bool. CAST pins it to an
	// integer so the scan below means the same thing on both.
	rows, err := tx.Query("SELECT cid, CAST(isprim AS INTEGER) FROM plink WHERE pid=?", rid)
	if err != nil {
		return fmt.Errorf("plink children of rid=%d: %w", rid, err)
	}
	type childEdge struct {
		cid    libfossil.FslID
		isPrim bool
	}
	var children []childEdge
	for rows.Next() {
		var cid int64
		var isprim int
		if err := rows.Scan(&cid, &isprim); err != nil {
			rows.Close()
			return fmt.Errorf("scan plink child of rid=%d: %w", rid, err)
		}
		children = append(children, childEdge{cid: libfossil.FslID(cid), isPrim: isprim != 0})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("plink children of rid=%d: %w", rid, err)
	}

	for _, c := range children {
		// addMlink tests this itself, but testing it here as well keeps the
		// common case -- a child whose rows were already derived from its own
		// side -- from paying a manifest expansion for the fixup below.
		var one int
		if tx.QueryRow("SELECT 1 FROM mlink WHERE mid=? AND pmid=?", c.cid, rid).Scan(&one) == nil {
			continue
		}
		if err := addMlink(tx, cache, rid, c.cid, nil, nil, c.isPrim, 0); err != nil {
			return err
		}
		if !c.isPrim {
			continue
		}
		// The child's primary transition just became derivable, so its
		// merge-added fixup can run for the first time too.
		childDeck, _, err := loadCheckinManifest(tx, cache, c.cid)
		if err != nil {
			continue
		}
		if err := fixMergeAddedPids(tx, c.cid, childDeck); err != nil {
			return err
		}
	}
	return nil
}
