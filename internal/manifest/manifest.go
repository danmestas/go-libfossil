package manifest

import (
	"fmt"
	"time"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/internal/tag"
)

type CheckinOpts struct {
	Files        []File
	Comment      string
	User         string
	Parent       libfossil.FslID
	MergeParents []libfossil.FslID // additional parents beyond Parent (for merge commits)
	Delta        bool
	Time         time.Time
	Tags         []deck.TagCard
}

type File struct {
	Name    string
	Content []byte
	Perm    string
}

func Checkin(r *repo.Repo, opts CheckinOpts) (manifestRid libfossil.FslID, manifestUUID string, err error) {
	if r == nil {
		panic("manifest.Checkin: r must not be nil")
	}
	if len(opts.Files) == 0 {
		panic("manifest.Checkin: opts.Files must not be empty")
	}
	if opts.User == "" {
		panic("manifest.Checkin: opts.User must not be empty")
	}
	defer func() {
		if err == nil && manifestRid <= 0 {
			panic("manifest.Checkin: manifestRid must be positive on success")
		}
	}()

	if opts.Time.IsZero() {
		opts.Time = time.Now().UTC()
	}

	var inlineTCards []deck.TagCard

	err = r.WithTx(func(tx *db.Tx) error {
		fCards, fileRids, txErr := storeFileBlobs(tx, opts.Files)
		if txErr != nil {
			return txErr
		}

		d, txErr := buildCheckinDeck(tx, opts, fCards)
		if txErr != nil {
			return txErr
		}
		inlineTCards = d.T // capture for post-tx tag processing

		manifestRid, manifestUUID, txErr = insertCheckinBlob(tx, d)
		if txErr != nil {
			return txErr
		}

		if txErr := insertMlinks(tx, opts, manifestRid); txErr != nil {
			return txErr
		}

		if txErr := markLeafAndEvent(tx, opts, manifestRid); txErr != nil {
			return txErr
		}

		// Mark file blobs as unsent so sync pushes them.
		// (unclustered is handled by blob.Store automatically.)
		for _, frid := range fileRids {
			if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", frid); err != nil {
				return fmt.Errorf("unsent file: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return 0, "", fmt.Errorf("manifest.Checkin: %w", err)
	}

	// Process inline T-cards (branch, sym-trunk, etc.) after the transaction
	// completes so the event/plink tables are populated for tag propagation.
	for _, tc := range inlineTCards {
		if tc.UUID != "*" {
			continue // non-self T-cards are for control artifacts
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
			TargetRID: manifestRid,
			SrcRID:    manifestRid,
			TagName:   tc.Name,
			TagType:   tagType,
			Value:     tc.Value,
			MTime:     libfossil.TimeToJulian(opts.Time),
		}); err != nil {
			return 0, "", fmt.Errorf("manifest.Checkin inline tag %q: %w", tc.Name, err)
		}
	}

	// Inherit propagating tags (branch, sym-*, bgcolor) from the primary parent.
	// Without this, a new commit on an existing branch would not carry its
	// parent's branch tag — breaking branch-aware queries. Matches the
	// corresponding PropagateAll call in crosslink's applyInlineTags so the
	// fresh-commit and sync-import paths converge on the same tagxref state.
	if opts.Parent > 0 {
		if err := tag.PropagateAll(r.DB(), opts.Parent); err != nil {
			return 0, "", fmt.Errorf("manifest.Checkin propagate from parent: %w", err)
		}
	}

	return manifestRid, manifestUUID, nil
}

func storeFileBlobs(tx *db.Tx, files []File) ([]deck.FileCard, []libfossil.FslID, error) {
	fCards := make([]deck.FileCard, len(files))
	var rids []libfossil.FslID
	for i, f := range files {
		rid, uuid, err := blob.Store(tx, f.Content)
		if err != nil {
			return nil, nil, fmt.Errorf("storing file %q: %w", f.Name, err)
		}
		fCards[i] = deck.FileCard{Name: f.Name, UUID: uuid, Perm: f.Perm}
		rids = append(rids, rid)
	}
	return fCards, rids, nil
}

func buildCheckinDeck(tx *db.Tx, opts CheckinOpts, fCards []deck.FileCard) (*deck.Deck, error) {
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    opts.Comment,
		D:    opts.Time,
		F:    fCards,
		U:    opts.User,
	}

	// Parents: primary parent first, then any merge parents. Fossil renders
	// this as a space-separated P-card; plink marks i==0 as the primary.
	if opts.Parent > 0 {
		var parentUUID string
		if err := tx.QueryRow("SELECT uuid FROM blob WHERE rid=?", opts.Parent).Scan(&parentUUID); err != nil {
			return nil, fmt.Errorf("parent uuid: %w", err)
		}
		d.P = []string{parentUUID}
		for _, mp := range opts.MergeParents {
			if mp <= 0 || mp == opts.Parent {
				continue
			}
			var mpUUID string
			if err := tx.QueryRow("SELECT uuid FROM blob WHERE rid=?", mp).Scan(&mpUUID); err != nil {
				return nil, fmt.Errorf("merge parent uuid: %w", err)
			}
			d.P = append(d.P, mpUUID)
		}
	}

	// Tags: use custom tags if provided, otherwise default trunk tags for initial checkin
	if len(opts.Tags) > 0 {
		d.T = opts.Tags
	} else if opts.Parent == 0 {
		d.T = []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		}
	}

	// Delta manifest support
	if opts.Delta && opts.Parent > 0 {
		if err := applyDelta(tx, d, fCards, opts.Parent); err != nil {
			return nil, err
		}
	}

	// R-card (always over full file set)
	rDeck := &deck.Deck{F: fCards}
	getContent := func(uuid string) ([]byte, error) {
		rid, ok := blob.Exists(tx, uuid)
		if !ok {
			return nil, fmt.Errorf("blob not found: %s", uuid)
		}
		return content.Expand(tx, rid)
	}
	rHash, err := rDeck.ComputeR(getContent)
	if err != nil {
		return nil, fmt.Errorf("R-card: %w", err)
	}
	d.R = rHash

	return d, nil
}

func insertCheckinBlob(tx *db.Tx, d *deck.Deck) (libfossil.FslID, string, error) {
	manifestBytes, err := d.Marshal()
	if err != nil {
		return 0, "", fmt.Errorf("marshal: %w", err)
	}
	rid, uuid, err := blob.Store(tx, manifestBytes)
	if err != nil {
		return 0, "", fmt.Errorf("store manifest: %w", err)
	}
	return rid, uuid, nil
}

func insertMlinks(tx *db.Tx, opts CheckinOpts, manifestRid libfossil.FslID) error {
	for _, f := range opts.Files {
		fnid, err := ensureFilename(tx, f.Name)
		if err != nil {
			return fmt.Errorf("filename %q: %w", f.Name, err)
		}
		fileUUID := hash.SHA1(f.Content)
		fileRid, _ := blob.Exists(tx, fileUUID)
		var pmid, pid int64
		if opts.Parent > 0 {
			pmid = int64(opts.Parent)
			tx.QueryRow("SELECT fid FROM mlink WHERE mid=? AND fnid=?", opts.Parent, fnid).Scan(&pid)
		}
		if _, err := tx.Exec(
			"INSERT INTO mlink(mid, fid, pmid, pid, fnid) VALUES(?, ?, ?, ?, ?)",
			manifestRid, fileRid, pmid, pid, fnid,
		); err != nil {
			return fmt.Errorf("mlink: %w", err)
		}
	}
	return nil
}

func markLeafAndEvent(tx *db.Tx, opts CheckinOpts, manifestRid libfossil.FslID) error {
	// plink: one row per parent; primary (isprim=1) is opts.Parent, merge
	// parents are secondary (isprim=0). Matches the sync-path layout in
	// crosslink.go so merged history looks identical however it arrived.
	mtime := libfossil.TimeToJulian(opts.Time)
	allParents := collectParents(opts)
	for i, pid := range allParents {
		isPrim := 0
		if i == 0 {
			isPrim = 1
		}
		if _, err := tx.Exec(
			"INSERT INTO plink(pid, cid, isprim, mtime) VALUES(?, ?, ?, ?)",
			pid, manifestRid, isPrim, mtime,
		); err != nil {
			return fmt.Errorf("plink: %w", err)
		}
	}

	// event
	if _, err := tx.Exec(
		"INSERT INTO event(type, mtime, objid, user, comment) VALUES('ci', ?, ?, ?, ?)",
		libfossil.TimeToJulian(opts.Time), manifestRid, opts.User, opts.Comment,
	); err != nil {
		return fmt.Errorf("event: %w", err)
	}

	// leaf: this checkin is new; remove every parent from leaf so merge
	// commits correctly supersede both sides.
	if _, err := tx.Exec("INSERT OR IGNORE INTO leaf(rid) VALUES(?)", manifestRid); err != nil {
		return fmt.Errorf("leaf insert: %w", err)
	}
	for _, pid := range allParents {
		if _, err := tx.Exec("DELETE FROM leaf WHERE rid=?", pid); err != nil {
			return fmt.Errorf("leaf delete parent: %w", err)
		}
	}

	// Mark manifest as unsent so sync pushes it (unclustered is handled by blob.Store).
	if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", manifestRid); err != nil {
		return fmt.Errorf("unsent: %w", err)
	}

	return nil
}

// collectParents returns the primary parent followed by any merge parents,
// skipping zero entries and de-duplicating. Used for both the plink insert
// loop and the leaf cleanup so both see the same set.
func collectParents(opts CheckinOpts) []libfossil.FslID {
	if opts.Parent <= 0 {
		return nil
	}
	seen := map[libfossil.FslID]bool{opts.Parent: true}
	out := []libfossil.FslID{opts.Parent}
	for _, mp := range opts.MergeParents {
		if mp <= 0 || seen[mp] {
			continue
		}
		seen[mp] = true
		out = append(out, mp)
	}
	return out
}

func ensureFilename(tx *db.Tx, name string) (int64, error) {
	if tx == nil {
		panic("manifest.ensureFilename: tx must not be nil")
	}
	if name == "" {
		panic("manifest.ensureFilename: name must not be empty")
	}
	var fnid int64
	err := tx.QueryRow("SELECT fnid FROM filename WHERE name=?", name).Scan(&fnid)
	if err == nil {
		return fnid, nil
	}
	result, err := tx.Exec("INSERT INTO filename(name) VALUES(?)", name)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func GetManifest(r *repo.Repo, rid libfossil.FslID) (result *deck.Deck, err error) {
	if r == nil {
		panic("manifest.GetManifest: r must not be nil")
	}
	if rid <= 0 {
		panic("manifest.GetManifest: rid must be positive")
	}
	defer func() {
		if err == nil && result == nil {
			panic("manifest.GetManifest: result must not be nil on success")
		}
	}()
	data, err := content.Expand(r.DB(), rid)
	if err != nil {
		return nil, fmt.Errorf("manifest.GetManifest: %w", err)
	}
	return deck.Parse(data)
}

func applyDelta(tx *db.Tx, d *deck.Deck, fullFCards []deck.FileCard, parentRid libfossil.FslID) error {
	parentData, err := content.Expand(tx, parentRid)
	if err != nil {
		return fmt.Errorf("expand parent: %w", err)
	}
	parentDeck, err := deck.Parse(parentData)
	if err != nil {
		return fmt.Errorf("parse parent: %w", err)
	}

	baselineUUID := parentDeck.B
	if baselineUUID == "" {
		var puuid string
		tx.QueryRow("SELECT uuid FROM blob WHERE rid=?", parentRid).Scan(&puuid)
		baselineUUID = puuid
	}

	baseRid, ok := blob.Exists(tx, baselineUUID)
	if !ok {
		return fmt.Errorf("baseline %s not found", baselineUUID)
	}
	baseData, err := content.Expand(tx, baseRid)
	if err != nil {
		return fmt.Errorf("expand baseline: %w", err)
	}
	baseDeck, err := deck.Parse(baseData)
	if err != nil {
		return fmt.Errorf("parse baseline: %w", err)
	}

	baseFiles := make(map[string]string)
	for _, f := range baseDeck.F {
		baseFiles[f.Name] = f.UUID
	}

	var deltaFCards []deck.FileCard
	currentFiles := make(map[string]bool)
	for _, f := range fullFCards {
		currentFiles[f.Name] = true
		if baseUUID, exists := baseFiles[f.Name]; !exists || baseUUID != f.UUID {
			deltaFCards = append(deltaFCards, f)
		}
	}
	for name := range baseFiles {
		if !currentFiles[name] {
			deltaFCards = append(deltaFCards, deck.FileCard{Name: name})
		}
	}

	if len(deltaFCards) < len(fullFCards) {
		d.B = baselineUUID
		d.F = deltaFCards
	}
	return nil
}
