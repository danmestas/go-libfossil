package manifest

import (
	"fmt"
	"log/slog"
	"sort"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/internal/tag"
)

// attachTargetTypeName maps attachment target type codes to human-readable names.
// Used by crosslinkAttachment and updateAttachmentComments.
var attachTargetTypeName = map[byte]string{
	'w': "wiki page",
	't': "ticket",
	'e': "tech note",
}

type pendingItem struct {
	Type byte   // 'w' = wiki backlink, 't' = ticket rebuild
	ID   string
}

// Crosslink scans all blobs not yet crosslinked in event/tagxref/forumpost/attachment tables,
// parses them as manifests, and populates cross-reference tables (event/plink/leaf/mlink/tagxref).
// This is the Go equivalent of Fossil's manifest_crosslink.
func Crosslink(r *repo.Repo) (int, error) {
	if r == nil {
		panic("manifest.Crosslink: r must not be nil")
	}

	// Pass 1: Discover and crosslink all uncrosslinked artifacts.
	// ORDER BY b.rid: deferred manifests re-discovered across sweeps must
	// be processed in stable order. Without it, two syncs delivering the
	// same blobs in different arrival orders could produce divergent
	// per-defer slog streams and pending-item processing orders, masking
	// determinism bugs in downstream code.
	rows, err := r.DB().Query(`
		SELECT b.rid, b.uuid FROM blob b
		WHERE b.size >= 0
		  AND NOT EXISTS (SELECT 1 FROM event e WHERE e.objid = b.rid)
		  AND NOT EXISTS (SELECT 1 FROM tagxref tx WHERE tx.srcid = b.rid)
		  AND NOT EXISTS (SELECT 1 FROM forumpost fp WHERE fp.fpid = b.rid)
		  AND NOT EXISTS (SELECT 1 FROM attachment a WHERE a.attachid = b.rid)
		ORDER BY b.rid
	`)
	if err != nil {
		return 0, fmt.Errorf("manifest.Crosslink query: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		rid  libfossil.FslID
		uuid string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.rid, &c.uuid); err != nil {
			return 0, fmt.Errorf("manifest.Crosslink scan: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("manifest.Crosslink rows: %w", err)
	}

	linked := 0
	var deferredRids []libfossil.FslID
	missingBlobs := make(map[string]struct{})
	var pending []pendingItem
	for _, c := range candidates {
		data, err := content.Expand(r.DB(), c.rid)
		if err != nil {
			continue // not expandable, skip
		}

		d, err := deck.Parse(data)
		if err != nil {
			continue // not a valid manifest, skip
		}

		// Defer Checkin manifests whose referenced file blobs (F-cards) or
		// delta baseline (B-card) haven't all arrived locally yet. The
		// manifest blob remains durable in 'blob'; we just skip writing
		// event/leaf/plink/mlink in this sweep so a downstream
		// Checkout.Update walking the manifest's F-cards via
		// manifest.ListFiles doesn't hit
		// `expandUUID: blob not found for uuid <hex>` mid-traversal.
		//
		// Surfaced by agent-infra trial #10 under 16-way concurrent
		// fork+merge: a leaf Pulled a multi-blob session in which the
		// merge manifest landed before its file blobs, the original
		// crosslink ran with insertCheckinMlinks silently skipping
		// missing-blob F-cards, and the next Update on that leaf failed.
		// The next sync round that delivers the missing blob also
		// triggers another Crosslink sweep (HandleSync runs Crosslink
		// whenever filesRecvd > 0); the candidate query selects this
		// rid again because no event row was written, and the Checkin
		// crosslinks completely.
		if d.Type == deck.Checkin {
			if missing := missingCheckinRefs(r, d); len(missing) > 0 {
				deferredRids = append(deferredRids, c.rid)
				for _, u := range missing {
					missingBlobs[u] = struct{}{}
				}
				slog.Debug("manifest.Crosslink: deferring checkin",
					"rid", c.rid,
					"uuid", c.uuid,
					"missing_count", len(missing),
					"first_missing", missing[0])
				continue
			}
		}

		var linkErr error
		var p []pendingItem

		switch d.Type {
		case deck.Checkin:
			linkErr = crosslinkCheckin(r, c.rid, d)
		case deck.Wiki:
			p, linkErr = crosslinkWiki(r, c.rid, d)
		case deck.Ticket:
			p, linkErr = crosslinkTicket(r, c.rid, d)
		case deck.Event:
			p, linkErr = crosslinkEvent(r, c.rid, d)
		case deck.Attachment:
			linkErr = crosslinkAttachment(r, c.rid, d)
		case deck.Cluster:
			linkErr = CrosslinkCluster(r.DB(), c.rid, d)
		case deck.ForumPost:
			linkErr = crosslinkForum(r, c.rid, d)
		case deck.Control:
			linkErr = crosslinkControl(r, c.rid, d)
		default:
			continue
		}

		if linkErr != nil {
			return linked, fmt.Errorf("manifest.Crosslink rid=%d type=%d: %w", c.rid, d.Type, linkErr)
		}
		linked++
		pending = append(pending, p...)
	}

	if len(deferredRids) > 0 {
		// Sort missing-blob UUIDs so the rollup is byte-identical across
		// runs that defer the same set, regardless of map iteration order.
		distinctMissing := make([]string, 0, len(missingBlobs))
		for u := range missingBlobs {
			distinctMissing = append(distinctMissing, u)
		}
		sort.Strings(distinctMissing)
		slog.Info("manifest.Crosslink: deferred checkins awaiting missing blobs",
			"deferred", len(deferredRids),
			"linked", linked,
			"deferred_rids", deferredRids,
			"missing_blob_count", len(distinctMissing),
			"missing_blobs", distinctMissing)
	}

	// Pass 2: Process pending items (wiki backlinks, ticket rebuilds).
	for _, item := range pending {
		_ = item // Stubs return nil, nothing to process yet.
	}

	return linked, nil
}

// missingCheckinRefs returns the list of UUIDs referenced by a Checkin
// manifest whose blobs are not yet present locally. References checked:
//   - B-card: the baseline manifest UUID for delta manifests. Without
//     the baseline, ListFiles cannot resolve the effective F-card set.
//   - F-cards: every (non-deleted) file UUID. These are the targets
//     Checkout.Update.expandUUID will need.
//
// Empty result means crosslink is safe to run; non-empty means defer
// to a later sweep that will discover the manifest again (no event row
// was written, so the candidate query re-selects this rid).
//
// Divergence from fossil-scm/c: fossil's reference uses an `rcvfrom`
// table + deferred-flush at content arrival; the Go port reuses the
// existing whole-repo sweep semantics by checking presence at sweep
// time. The candidate query naturally re-discovers deferred manifests
// because we do not write any event/leaf/plink/mlink/tagxref rows for
// them.
func missingCheckinRefs(r *repo.Repo, d *deck.Deck) []string {
	if r == nil {
		panic("manifest.missingCheckinRefs: r must not be nil")
	}
	if d == nil {
		panic("manifest.missingCheckinRefs: d must not be nil")
	}
	var missing []string
	seen := make(map[string]struct{})
	check := func(uuid string) {
		if uuid == "" {
			return
		}
		if _, dup := seen[uuid]; dup {
			return
		}
		seen[uuid] = struct{}{}
		if !blobPresent(r, uuid) {
			missing = append(missing, uuid)
		}
	}
	check(d.B)
	for _, f := range d.F {
		check(f.UUID) // skipped if "" (deleted file in delta manifest)
	}
	return missing
}

// blobPresent reports whether the named UUID corresponds to a non-phantom
// blob locally. blob.Exists returns true for phantoms (size = -1), which
// content.Expand rejects, so we filter those out — a phantom blob row is
// not "present" for crosslink purposes.
func blobPresent(r *repo.Repo, uuid string) bool {
	if r == nil {
		panic("manifest.blobPresent: r must not be nil")
	}
	if uuid == "" {
		panic("manifest.blobPresent: uuid must not be empty")
	}
	var size int64
	err := r.DB().QueryRow("SELECT size FROM blob WHERE uuid=?", uuid).Scan(&size)
	if err != nil {
		return false
	}
	return size >= 0
}

func crosslinkCheckin(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) error {
	if r == nil {
		panic("crosslinkCheckin: r must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkCheckin: rid must be positive")
	}

	if err := crosslinkCheckinTables(r, rid, d); err != nil {
		return err
	}
	return applyInlineTags(r, rid, d)
}

// crosslinkCheckinTables populates event/plink/leaf/mlink/cherrypick in a single transaction.
func crosslinkCheckinTables(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) error {
	return r.WithTx(func(tx *db.Tx) error {
		// event
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO event(type, mtime, objid, user, comment) VALUES('ci', ?, ?, ?, ?)",
			libfossil.TimeToJulian(d.D), rid, d.U, d.C,
		); err != nil {
			return fmt.Errorf("event: %w", err)
		}

		// Resolve baseid for plink if B-card present
		var baseid any = nil
		if d.B != "" {
			var baseRid int64
			if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", d.B).Scan(&baseRid); err == nil {
				baseid = baseRid
			}
		}

		if err := insertCheckinPlinks(tx, rid, d, baseid); err != nil {
			return err
		}
		if err := updateLeafTable(tx, rid, d); err != nil {
			return err
		}
		if err := insertCheckinMlinks(tx, rid, d); err != nil {
			return err
		}
		return insertCherrypicks(tx, rid, d)
	})
}

// insertCheckinPlinks inserts plink rows for each parent (P-card).
func insertCheckinPlinks(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, baseid any) error {
	for i, parentUUID := range d.P {
		var parentRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", parentUUID).Scan(&parentRid); err != nil {
			continue // parent blob missing, skip
		}
		isPrim := 0
		if i == 0 {
			isPrim = 1
		}
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO plink(pid, cid, isprim, mtime, baseid) VALUES(?, ?, ?, ?, ?)",
			parentRid, rid, isPrim, libfossil.TimeToJulian(d.D), baseid,
		); err != nil {
			return fmt.Errorf("plink: %w", err)
		}
	}
	return nil
}

// updateLeafTable marks this checkin as a leaf and removes parents from the leaf table.
func updateLeafTable(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if _, err := tx.Exec("INSERT OR IGNORE INTO leaf(rid) VALUES(?)", rid); err != nil {
		return fmt.Errorf("leaf insert: %w", err)
	}
	// Remove parent from leaf table (it now has a child)
	for _, parentUUID := range d.P {
		var parentRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", parentUUID).Scan(&parentRid); err != nil {
			continue
		}
		if _, err := tx.Exec("DELETE FROM leaf WHERE rid=?", parentRid); err != nil {
			return fmt.Errorf("leaf delete parent %d: %w", parentRid, err)
		}
	}
	return nil
}

// insertCheckinMlinks inserts mlink rows for each file mapping (F-card).
func insertCheckinMlinks(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	for _, f := range d.F {
		if f.UUID == "" {
			continue // deleted file in delta manifest
		}
		fnid, err := ensureFilename(tx, f.Name)
		if err != nil {
			return fmt.Errorf("filename %q: %w", f.Name, err)
		}
		var fileRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", f.UUID).Scan(&fileRid); err != nil {
			continue // file blob missing
		}
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO mlink(mid, fid, fnid) VALUES(?, ?, ?)",
			rid, fileRid, fnid,
		); err != nil {
			return fmt.Errorf("mlink: %w", err)
		}
	}
	return nil
}

// insertCherrypicks inserts cherrypick rows for Q-cards (cherrypick/backout).
func insertCherrypicks(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	for _, cp := range d.Q {
		target := cp.Target
		isExclude := 0
		if cp.IsBackout {
			isExclude = 1
		}
		var parentRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", target).Scan(&parentRid); err != nil {
			continue // target blob missing, skip
		}
		if _, err := tx.Exec(
			"REPLACE INTO cherrypick(parentid, childid, isExclude) VALUES(?, ?, ?)",
			parentRid, rid, isExclude,
		); err != nil {
			return fmt.Errorf("cherrypick: %w", err)
		}
	}
	return nil
}

// applyInlineTags processes T-cards with UUID="*" (self-referencing tags) and propagates from parent.
func applyInlineTags(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) error {
	mtime := libfossil.TimeToJulian(d.D)
	for _, tc := range d.T {
		if tc.UUID != "*" {
			continue
		}
		var tagType int
		switch tc.Type {
		case deck.TagPropagating:
			tagType = tag.TagPropagating
		case deck.TagSingleton:
			tagType = tag.TagSingleton
		case deck.TagCancel:
			tagType = tag.TagCancel
		default:
			continue
		}

		if err := tag.ApplyTag(r, tag.ApplyOpts{
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

	// PropagateAll from primary parent (if checkin has parents)
	if len(d.P) > 0 {
		var primaryParentRid int64
		if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", d.P[0]).Scan(&primaryParentRid); err == nil {
			if err := tag.PropagateAll(r.DB(), libfossil.FslID(primaryParentRid)); err != nil {
				return fmt.Errorf("propagate from parent: %w", err)
			}
		}
	}

	return nil
}

func crosslinkControl(r *repo.Repo, srcRID libfossil.FslID, d *deck.Deck) error {
	if r == nil {
		panic("crosslinkControl: r must not be nil")
	}
	if srcRID <= 0 {
		panic("crosslinkControl: rid must be positive")
	}

	mtime := libfossil.TimeToJulian(d.D)
	for _, tc := range d.T {
		if tc.UUID == "*" {
			continue // self-referencing — handled in crosslinkCheckin
		}
		var targetRID int64
		if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", tc.UUID).Scan(&targetRID); err != nil {
			continue // target not found
		}
		var tagType int
		switch tc.Type {
		case deck.TagPropagating:
			tagType = tag.TagPropagating
		case deck.TagSingleton:
			tagType = tag.TagSingleton
		case deck.TagCancel:
			tagType = tag.TagCancel
		default:
			continue
		}
		if err := tag.ApplyTag(r, tag.ApplyOpts{
			TargetRID: libfossil.FslID(targetRID),
			SrcRID:    srcRID,
			TagName:   tc.Name,
			TagType:   tagType,
			Value:     tc.Value,
			MTime:     mtime,
		}); err != nil {
			return fmt.Errorf("apply tag %q to rid=%d: %w", tc.Name, targetRID, err)
		}
	}

	// Generate event row with type='g' and descriptive comment.
	comment := buildControlComment(d)
	if _, err := r.DB().Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('g', ?, ?, ?, ?)",
		mtime, srcRID, d.U, comment,
	); err != nil {
		return fmt.Errorf("control event: %w", err)
	}

	return nil
}

// buildControlComment generates a human-readable comment from a control artifact's T-cards.
func buildControlComment(d *deck.Deck) string {
	var comment string
	for _, tc := range d.T {
		if tc.UUID == "*" {
			continue
		}
		prefix := string(tc.Type)
		name := tc.Name
		val := tc.Value
		switch {
		case prefix == "*" && name == "branch":
			comment += fmt.Sprintf(" Move to branch %s.", val)
		case prefix == "*" && name == "bgcolor":
			comment += fmt.Sprintf(" Change branch background color to %q.", val)
		case prefix == "+" && name == "bgcolor":
			comment += fmt.Sprintf(" Change background color to %q.", val)
		case prefix == "-" && name == "bgcolor":
			comment += " Cancel background color."
		case prefix == "+" && name == "comment":
			comment += " Edit check-in comment."
		case prefix == "+" && name == "user":
			comment += fmt.Sprintf(" Change user to %q.", val)
		default:
			switch prefix {
			case "-":
				comment += fmt.Sprintf(" Cancel %q.", name)
			case "+":
				comment += fmt.Sprintf(" Add %q.", name)
			case "*":
				comment += fmt.Sprintf(" Add propagating %q.", name)
			}
		}
	}
	if comment == "" {
		comment = " "
	}
	return comment
}

// addFWTPlink handles plink insertion and tag propagation for wiki/forum/technote/ticket.
// Shared helper for artifact types that use P-cards (parents) but not the full checkin flow.
func addFWTPlink(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) error {
	if r == nil {
		panic("manifest.addFWTPlink: r must not be nil")
	}
	if rid <= 0 {
		panic("manifest.addFWTPlink: rid must be positive")
	}

	mtime := libfossil.TimeToJulian(d.D)
	var primaryParentRid libfossil.FslID

	for i, parentUUID := range d.P {
		var parentRid int64
		if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", parentUUID).Scan(&parentRid); err != nil {
			continue // parent blob missing, skip
		}
		isPrim := 0
		if i == 0 {
			isPrim = 1
			primaryParentRid = libfossil.FslID(parentRid)
		}
		if _, err := r.DB().Exec(
			"INSERT OR IGNORE INTO plink(pid, cid, isprim, mtime) VALUES(?, ?, ?, ?)",
			parentRid, rid, isPrim, mtime,
		); err != nil {
			return fmt.Errorf("addFWTPlink: %w", err)
		}
	}

	// Propagate tags from primary parent
	if primaryParentRid > 0 {
		if err := tag.PropagateAll(r.DB(), primaryParentRid); err != nil {
			return fmt.Errorf("addFWTPlink propagate: %w", err)
		}
	}

	return nil
}

func crosslinkWiki(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) ([]pendingItem, error) {
	if r == nil {
		panic("crosslinkWiki: r must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkWiki: rid must be positive")
	}

	if err := addFWTPlink(r, rid, d); err != nil {
		return nil, fmt.Errorf("wiki plink: %w", err)
	}

	title := d.L
	if title == "" {
		return nil, fmt.Errorf("wiki manifest missing title (L-card)")
	}

	// Apply wiki-<title> tag with value = content length
	wikiLen := fmt.Sprintf("%d", len(d.W))
	if err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   fmt.Sprintf("wiki-%s", title),
		TagType:   tag.TagSingleton,
		Value:     wikiLen,
		MTime:     libfossil.TimeToJulian(d.D),
	}); err != nil {
		return nil, fmt.Errorf("wiki tag: %w", err)
	}

	// Insert event row with prefix: '+' = new, ':' = edit, '-' = delete
	var prefix byte
	if len(d.W) == 0 {
		prefix = '-' // deletion
	} else if len(d.P) == 0 {
		prefix = '+' // new page
	} else {
		prefix = ':' // edit
	}
	comment := fmt.Sprintf("%c%s", prefix, title)

	if _, err := r.DB().Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('w', ?, ?, ?, ?)",
		libfossil.TimeToJulian(d.D), rid, d.U, comment,
	); err != nil {
		return nil, fmt.Errorf("wiki event: %w", err)
	}

	return []pendingItem{{Type: 'w', ID: title}}, nil
}

func crosslinkTicket(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) ([]pendingItem, error) {
	if r == nil {
		panic("crosslinkTicket: r must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkTicket: rid must be positive")
	}

	ticketUUID := d.K
	if ticketUUID == "" {
		return nil, fmt.Errorf("ticket manifest missing UUID (K-card)")
	}
	if err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   fmt.Sprintf("tkt-%s", ticketUUID),
		TagType:   tag.TagSingleton,
		MTime:     libfossil.TimeToJulian(d.D),
	}); err != nil {
		return nil, fmt.Errorf("ticket tag: %w", err)
	}
	if err := updateAttachmentComments(r, ticketUUID, 't'); err != nil {
		return nil, fmt.Errorf("ticket attachment comments: %w", err)
	}
	return []pendingItem{{Type: 't', ID: ticketUUID}}, nil
}

func crosslinkEvent(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) ([]pendingItem, error) {
	if r == nil {
		panic("crosslinkEvent: r must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkEvent: rid must be positive")
	}

	if d.E == nil {
		return nil, fmt.Errorf("event manifest missing E-card")
	}
	if err := addFWTPlink(r, rid, d); err != nil {
		return nil, fmt.Errorf("event plink: %w", err)
	}
	eventID := d.E.UUID
	tagName := fmt.Sprintf("event-%s", eventID)
	mtime := libfossil.TimeToJulian(d.D)
	if err := tag.ApplyTag(r, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   tagName,
		TagType:   tag.TagSingleton,
		Value:     fmt.Sprintf("%d", len(d.W)),
		MTime:     mtime,
	}); err != nil {
		return nil, fmt.Errorf("event tag: %w", err)
	}

	var tagid int64
	if err := r.DB().QueryRow("SELECT tagid FROM tag WHERE tagname=?", tagName).Scan(&tagid); err != nil {
		return nil, fmt.Errorf("event tagid: %w", err)
	}

	var subsequent int64
	r.DB().QueryRow("SELECT rid FROM tagxref WHERE tagid=? AND mtime>=? AND rid!=? ORDER BY mtime LIMIT 1",
		tagid, mtime, rid).Scan(&subsequent)

	// Fossil deletes stale event rows when a newer version of this tech note exists
	// but no subsequent version has been crosslinked yet. This ensures only the latest
	// version's event row survives, preventing duplicate timeline entries.
	if len(d.P) > 0 && subsequent == 0 {
		r.DB().Exec("DELETE FROM event WHERE type='e' AND tagid=? AND objid IN (SELECT rid FROM tagxref WHERE tagid=?)", tagid, tagid)
	}
	if subsequent == 0 {
		var bgcolor any
		var bgStr string
		if r.DB().QueryRow("SELECT value FROM tagxref JOIN tag USING(tagid) WHERE tagname='bgcolor' AND rid=?", rid).Scan(&bgStr) == nil {
			bgcolor = bgStr
		}
		if _, err := r.DB().Exec(
			"REPLACE INTO event(type, mtime, objid, tagid, user, comment, bgcolor) VALUES('e', ?, ?, ?, ?, ?, ?)",
			libfossil.TimeToJulian(d.E.Date), rid, tagid, d.U, d.C, bgcolor,
		); err != nil {
			return nil, fmt.Errorf("event insert: %w", err)
		}
	}
	if err := updateAttachmentComments(r, eventID, 'e'); err != nil {
		return nil, fmt.Errorf("event attachment comments: %w", err)
	}
	return nil, nil
}

func crosslinkAttachment(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) error {
	if r == nil {
		panic("crosslinkAttachment: r must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkAttachment: rid must be positive")
	}

	if d.A == nil {
		return fmt.Errorf("attachment manifest missing A-card")
	}
	mtime := libfossil.TimeToJulian(d.D)
	src, target, filename := d.A.Source, d.A.Target, d.A.Filename

	if _, err := r.DB().Exec(
		"INSERT INTO attachment(attachid, mtime, src, target, filename, comment, user) VALUES(?, ?, ?, ?, ?, ?, ?)",
		rid, mtime, src, target, filename, d.C, d.U,
	); err != nil {
		return fmt.Errorf("attachment insert: %w", err)
	}
	if _, err := r.DB().Exec(
		`UPDATE attachment SET isLatest = (mtime = (SELECT max(mtime) FROM attachment WHERE target=? AND filename=?)) WHERE target=? AND filename=?`,
		target, filename, target, filename,
	); err != nil {
		return fmt.Errorf("attachment isLatest: %w", err)
	}

	// Fossil defaults to wiki when target is not a hash (page name = wiki target).
	// Only hash-shaped targets can refer to tickets or tech notes.
	attachToType := byte('w')
	if isHash(target) {
		var dummy int
		if r.DB().QueryRow("SELECT 1 FROM tag WHERE tagname=?", "tkt-"+target).Scan(&dummy) == nil {
			attachToType = 't'
		} else if r.DB().QueryRow("SELECT 1 FROM tag WHERE tagname=?", "event-"+target).Scan(&dummy) == nil {
			attachToType = 'e'
		}
	}

	typeName := attachTargetTypeName[attachToType]
	var evComment string
	if src != "" {
		evComment = fmt.Sprintf("Add attachment %s to %s %s", filename, typeName, target)
	} else {
		evComment = fmt.Sprintf("Delete attachment %q from %s %s", filename, typeName, target)
	}
	if _, err := r.DB().Exec("REPLACE INTO event(type, mtime, objid, user, comment) VALUES(?, ?, ?, ?, ?)",
		string(attachToType), mtime, rid, d.U, evComment); err != nil {
		return fmt.Errorf("attachment event: %w", err)
	}
	return nil
}

func isHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func updateAttachmentComments(r *repo.Repo, targetID string, targetType byte) error {
	if r == nil {
		panic("updateAttachmentComments: r must not be nil")
	}
	if targetID == "" {
		panic("updateAttachmentComments: targetID must not be empty")
	}

	rows, err := r.DB().Query("SELECT attachid, src, target, filename FROM attachment WHERE target=?", targetID)
	if err != nil {
		return fmt.Errorf("updateAttachmentComments query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attachid int64
		var src, target, filename string
		if rows.Scan(&attachid, &src, &target, &filename) != nil {
			continue
		}
		typeName := attachTargetTypeName[targetType]
		var comment string
		if src != "" {
			comment = fmt.Sprintf("Add attachment %s to %s %s", filename, typeName, target)
		} else {
			comment = fmt.Sprintf("Delete attachment %q from %s %s", filename, typeName, target)
		}
		if _, err := r.DB().Exec("UPDATE event SET comment=?, type=? WHERE objid=?", comment, string(targetType), attachid); err != nil {
			return fmt.Errorf("updateAttachmentComments event update: %w", err)
		}
	}
	return rows.Err()
}

func crosslinkForum(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) error {
	if r == nil {
		panic("crosslinkForum: r must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkForum: rid must be positive")
	}

	if err := addFWTPlink(r, rid, d); err != nil {
		return fmt.Errorf("forum plink: %w", err)
	}

	// Resolve thread references
	froot, fprev, firt := resolveForumRefs(r, rid, d)

	// Insert forumpost
	if _, err := r.DB().Exec(
		"REPLACE INTO forumpost(fpid, froot, fprev, firt, fmtime) VALUES(?, ?, nullif(?, 0), nullif(?, 0), ?)",
		rid, froot, fprev, firt, libfossil.TimeToJulian(d.D),
	); err != nil {
		return fmt.Errorf("forumpost insert: %w", err)
	}

	mtime := libfossil.TimeToJulian(d.D)

	if firt == 0 {
		return crosslinkForumStarter(r, rid, d, froot, fprev, mtime)
	}
	return crosslinkForumReply(r, rid, d, froot, fprev, mtime)
}

// resolveForumRefs resolves the thread root, previous, and in-reply-to rids from deck cards.
func resolveForumRefs(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) (froot, fprev, firt libfossil.FslID) {
	if d.G != "" {
		var rootRid int64
		if r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", d.G).Scan(&rootRid) == nil {
			froot = libfossil.FslID(rootRid)
		}
	}
	if froot == 0 {
		froot = rid // self is thread root
	}
	if len(d.P) > 0 {
		var prevRid int64
		if r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", d.P[0]).Scan(&prevRid) == nil {
			fprev = libfossil.FslID(prevRid)
		}
	}
	if d.I != "" {
		var irtRid int64
		if r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", d.I).Scan(&irtRid) == nil {
			firt = libfossil.FslID(irtRid)
		}
	}
	return froot, fprev, firt
}

// crosslinkForumStarter inserts the event row for a thread-starting forum post.
func crosslinkForumStarter(r *repo.Repo, rid libfossil.FslID, d *deck.Deck, froot, fprev libfossil.FslID, mtime float64) error {
	title := d.H
	if title == "" {
		title = "(Deleted)"
	}
	fType := "Post"
	if fprev != 0 {
		fType = "Edit"
	}
	if _, err := r.DB().Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('f', ?, ?, ?, ?)",
		mtime, rid, d.U, fmt.Sprintf("%s: %s", fType, title),
	); err != nil {
		return fmt.Errorf("forum event: %w", err)
	}
	// Update thread title if most recent
	var hasNewer int
	r.DB().QueryRow("SELECT count(*) FROM forumpost WHERE froot=? AND firt=0 AND fpid!=? AND fmtime>?",
		froot, rid, mtime).Scan(&hasNewer)
	if hasNewer == 0 {
		r.DB().Exec(
			"UPDATE event SET comment=substr(comment,1,instr(comment,':')) || ' ' || ? WHERE objid IN (SELECT fpid FROM forumpost WHERE froot=?)",
			title, froot)
	}
	return nil
}

// crosslinkForumReply inserts the event row for a forum reply.
func crosslinkForumReply(r *repo.Repo, rid libfossil.FslID, d *deck.Deck, froot, fprev libfossil.FslID, mtime float64) error {
	var rootTitle string
	if r.DB().QueryRow("SELECT substr(comment, instr(comment,':')+2) FROM event WHERE objid=?", froot).Scan(&rootTitle) != nil {
		rootTitle = "Unknown"
	}
	fType := "Reply"
	if len(d.W) == 0 {
		fType = "Delete reply"
	} else if fprev != 0 {
		fType = "Edit reply"
	}
	if _, err := r.DB().Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('f', ?, ?, ?, ?)",
		mtime, rid, d.U, fmt.Sprintf("%s: %s", fType, rootTitle),
	); err != nil {
		return fmt.Errorf("forum reply event: %w", err)
	}
	return nil
}

// CrosslinkCluster processes a cluster artifact: applies the cluster singleton
// tag (tagid=7), removes clustered blobs from unclustered, and creates phantoms
// for any referenced UUIDs not yet in the blob table.
func CrosslinkCluster(q db.Querier, rid libfossil.FslID, d *deck.Deck) error {
	if q == nil {
		panic("manifest.CrosslinkCluster: q must not be nil")
	}
	if rid <= 0 {
		panic("manifest.CrosslinkCluster: rid must be > 0")
	}
	if d == nil {
		panic("manifest.CrosslinkCluster: d must not be nil")
	}

	// Apply cluster singleton tag (tagid=7, tagtype=1).
	if _, err := q.Exec(
		"INSERT OR REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) VALUES(7, 1, ?, ?, NULL, 0, ?)",
		rid, rid, rid,
	); err != nil {
		return fmt.Errorf("manifest.CrosslinkCluster tag: %w", err)
	}

	// Process each M-card UUID.
	for _, uuid := range d.M {
		memberRID, exists := blob.Exists(q, uuid)
		if exists {
			// Remove from unclustered — this blob is now accounted for.
			if _, err := q.Exec("DELETE FROM unclustered WHERE rid=?", memberRID); err != nil {
				return fmt.Errorf("manifest.CrosslinkCluster unclustered delete rid=%d: %w", memberRID, err)
			}
		} else {
			// Create phantom for unknown UUID.
			if _, err := blob.StorePhantom(q, uuid); err != nil {
				return fmt.Errorf("manifest.CrosslinkCluster phantom %s: %w", uuid, err)
			}
		}
	}

	return nil
}
