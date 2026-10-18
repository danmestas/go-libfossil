package manifest

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
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
	// Precheck, when set, runs first inside the check-in's transaction, and
	// an error from it aborts the check-in before anything is stored. It makes
	// a caller's read-then-write decision atomic with the write: branch.Create
	// refuses an open branch name here, so two concurrent creates of one name
	// cannot both commit. It must only read: a write would land in the
	// check-in's transaction.
	Precheck func(q db.Querier) error
}

type File struct {
	Name    string
	Content []byte
	Perm    string
	OldName string // prior pathname when this file card records a rename
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
		// If a panic is already unwinding the stack (e.g. a lower-level
		// invariant violation inside the transaction body), that panic is
		// the real diagnosis and must propagate unchanged. Without this
		// check, the postcondition assertion below fires unconditionally
		// during unwind too — manifestRid is still its zero value because
		// the panicking call never returned — replacing the original
		// panic with an unrelated "manifestRid must be positive" message
		// and masking the actual cause (issue #68).
		if p := recover(); p != nil {
			panic(p)
		}
		if err == nil && manifestRid <= 0 {
			panic("manifest.Checkin: manifestRid must be positive on success")
		}
	}()

	// The D card holds the time as fossil writes it: to the millisecond, a
	// caller's time rounded as fossil rounds a --date-override. Finer digits
	// are legal (§4.6.4) but zit cannot read them.
	if opts.Time.IsZero() {
		opts.Time = libfossil.CardTimeNow(time.Now())
	} else {
		opts.Time = libfossil.CardTime(opts.Time)
	}

	err = r.WithTx(func(tx *db.Tx) error {
		if opts.Precheck != nil {
			if txErr := opts.Precheck(tx); txErr != nil {
				return txErr
			}
		}
		fCards, fileRids, txErr := storeFileBlobs(tx, opts.Files)
		if txErr != nil {
			return txErr
		}

		d, txErr := buildCheckinDeck(tx, opts, fCards)
		if txErr != nil {
			return txErr
		}

		cache := content.NewCache(crosslinkCacheBytes)
		if txErr := preflightCheckinMlinks(tx, cache, d); txErr != nil {
			return fmt.Errorf("preflight checkin mlinks: %w", txErr)
		}

		manifestRid, manifestUUID, txErr = insertCheckinBlob(tx, d)
		if txErr != nil {
			return txErr
		}

		if txErr := VerifyRCard(tx, cache, manifestRid); txErr != nil {
			return txErr
		}

		if txErr := insertCheckinMlinks(tx, cache, manifestRid, d); txErr != nil {
			return txErr
		}

		// Store the parent check-in manifest as a delta against this one,
		// the same backwards direction used for file content. Canonical does
		// this at the end of commit (src/checkin.c:3234,
		// `content_deltify(vid, &nvid, 1, 0)`). File content is handled by
		// insertCheckinMlinks above; this covers the manifests themselves.
		if opts.Parent > 0 {
			if _, txErr := content.Deltify(tx, opts.Parent, manifestRid); txErr != nil {
				return fmt.Errorf("deltify parent manifest: %w", txErr)
			}
		}

		if txErr := markPlinkAndEvent(tx, opts, manifestRid); txErr != nil {
			return txErr
		}

		// Mark file blobs as unsent so sync pushes them.
		// (unclustered is handled by blob.Store automatically.)
		for _, frid := range fileRids {
			if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", frid); err != nil {
				return fmt.Errorf("unsent file: %w", err)
			}
		}

		return linkCheckinTags(tx, opts, manifestRid, d.T)
	})
	if err != nil {
		return 0, "", fmt.Errorf("manifest.Checkin: %w", err)
	}

	return manifestRid, manifestUUID, nil
}

// linkCheckinTags derives a new check-in's tag and leaf state on the check-in's
// own transaction, after its event and plink rows, so the check-in commits
// with its branch already in effect: a reader never sees it untagged, and a
// Precheck in a later check-in sees it.
func linkCheckinTags(
	tx *db.Tx, opts CheckinOpts, manifestRid libfossil.FslID, tcards []deck.TagCard,
) error {
	// The check-in's transaction is the batch: the rechecks its branch rows
	// schedule are applied before it commits, as fossil's commit applies
	// them at the end of its transaction (src/db.c, leaf_do_pending_checks).
	leaves := leaf.NewPending()
	// Inline T-cards (branch, sym-trunk, etc.) name the check-in itself.
	for _, tc := range tcards {
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
		if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
			TargetRID: manifestRid,
			SrcRID:    manifestRid,
			TagName:   tc.Name,
			TagType:   tagType,
			Value:     tc.Value,
			MTime:     libfossil.TimeToJulian(opts.Time),
			Leaves:    leaves,
		}); err != nil {
			return fmt.Errorf("inline tag %q: %w", tc.Name, err)
		}
	}

	// Inherit propagating tags (branch, sym-*, bgcolor) from the primary parent.
	// Without this, a new commit on an existing branch would not carry its
	// parent's branch tag — breaking branch-aware queries. A local commit is
	// always linked immediately after its parent exists, so calling this
	// here (rather than deferring to a repair pass) is safe; crosslink's
	// sync-import path defers the equivalent work to repairTagPropagation
	// instead, since candidates there are not visited in parent-before-child
	// order. Both converge on the same tagxref state.
	if opts.Parent > 0 {
		if err := tag.PropagateAllLeaves(tx, opts.Parent, leaves); err != nil {
			return fmt.Errorf("propagate from parent: %w", err)
		}
	}

	// Leaf state last, once every branch tag it reads is in place. Only what
	// the branch writes above scheduled is checked (draft-fossil-repo-state-01
	// Section 7.3): the check-in and its parents when it declares or inherits
	// a branch row. A check-in whose parent's branch row is a singleton, or
	// absent, inherits none, so neither is rechecked, as in fossil's commit.
	return leaves.Apply(tx)
}

// checkWriterName refuses a filename the artifact format's writer profile
// forbids (section 4.6.1): a backslash, or bytes that are not valid UTF-8,
// which includes surrogates and overlong forms. A parser accepts them, so a
// manifest naming one would be stored by a reader but is not one a writer
// may create. The empty name is OldName's "no rename" and passes.
func checkWriterName(name string) error {
	if strings.ContainsRune(name, '\\') {
		return fmt.Errorf("filename %q contains a backslash", name)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("filename %q is not valid UTF-8", name)
	}
	return nil
}

func storeFileBlobs(tx *db.Tx, files []File) ([]deck.FileCard, []libfossil.FslID, error) {
	fCards := make([]deck.FileCard, len(files))
	var rids []libfossil.FslID
	for i, f := range files {
		if err := checkWriterName(f.Name); err != nil {
			return nil, nil, err
		}
		if err := checkWriterName(f.OldName); err != nil {
			return nil, nil, err
		}
		rid, uuid, err := blob.Store(tx, f.Content)
		if err != nil {
			return nil, nil, fmt.Errorf("storing file %q: %w", f.Name, err)
		}
		fCards[i] = deck.FileCard{Name: f.Name, UUID: uuid, Perm: f.Perm, OldName: f.OldName}
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
		U:    deck.User(opts.User),
	}

	// Parents: primary parent first, then any merge parents. Fossil renders
	// this as a space-separated P-card; plink marks i==0 as the primary.
	if parents := collectParents(opts); len(parents) > 0 {
		d.P = make([]string, 0, len(parents))
		for i, parent := range parents {
			var parentUUID string
			if err := tx.QueryRow("SELECT uuid FROM blob WHERE rid=?", parent).Scan(&parentUUID); err != nil {
				kind := "merge parent"
				if i == 0 {
					kind = "primary parent"
				}
				return nil, fmt.Errorf("%s uuid: %w", kind, err)
			}
			d.P = append(d.P, parentUUID)
		}
	}

	// Tags: use custom tags if provided, otherwise default trunk tags for initial checkin
	if len(opts.Tags) > 0 {
		d.T = opts.Tags
	} else if opts.Parent == 0 {
		// Both tags propagate, matching `fossil new` and repo-state §6.1: the
		// branch name and its sym- alias descend to every child on the branch.
		// sym-trunk as a singleton (+) would stop at the root (issue #289).
		// The initial check-in starts the default branch, which is the
		// main-branch setting (Section 6.1), as fossil's commit does.
		main, err := repo.MainBranch(tx)
		if err != nil {
			return nil, fmt.Errorf("main branch: %w", err)
		}
		d.T = []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: main},
			{Type: deck.TagPropagating, Name: "sym-" + main, UUID: "*"},
		}
	}

	// Delta manifest support
	if opts.Delta && opts.Parent > 0 {
		if err := applyDelta(tx, d, fCards, opts.Parent); err != nil {
			return nil, err
		}
	}

	sort.Slice(d.F, func(i, j int) bool { return deck.Compare(d.F[i].Name, d.F[j].Name) < 0 })

	// R-card (always over full file set)
	rDeck := &deck.Deck{F: fCards}
	// Bare Expand: computing one checkin's R-card expands each file in the
	// tree exactly once. Distinct blobs, single pass, no overlap to amortize.
	getContent := func(uuid string) ([]byte, error) {
		rid, ok := content.AvailableByUUID(tx, uuid)
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
	// Writer postcondition (AF-6.3-4): ComputeR yields lowercase hex, so this
	// can only trip on a programmer error upstream.
	if len(d.R) != 32 || strings.ToLower(d.R) != d.R {
		panic("manifest.insertCheckinBlob: R card must be 32 lowercase hex digits")
	}
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

// VerifyRCard is the commit-time self-check of artifact-format-01 6.3.4
// (AF-6.3-5), fossil's checkin.c "manifest checksum self-test": it reads the
// stored manifest back, recomputes the R digest over the tree the manifest
// resolves to (a delta manifest's baseline merged with its own cards), and
// requires it to equal the manifest's R card byte for byte. The digest is
// always lowercase, so an uppercase or otherwise wrong R card fails. Reading
// back, not reusing the digest the writer just computed, is what makes this a
// check: it catches a manifest whose cards do not describe the tree R covers.
// It returns an error rather than panicking because a mismatch can come from
// stored-state corruption, which is not a programmer error. It is exported
// (inside internal/) so the conformance suite can drive it with a stored
// manifest that carries a bad R card.
func VerifyRCard(tx *db.Tx, cache *content.Cache, rid libfossil.FslID) error {
	if rid <= 0 {
		panic("manifest.VerifyRCard: rid must be positive")
	}
	d, mf, err := loadCheckinManifest(tx, cache, rid)
	if err != nil {
		return fmt.Errorf("R-card check: %w", err)
	}
	tree := &deck.Deck{}
	for _, name := range mf.treeNames() {
		tree.F = append(tree.F, mf.tree[name])
	}
	got, err := tree.ComputeR(func(uuid string) ([]byte, error) {
		frid, ok := content.AvailableByUUID(tx, uuid)
		if !ok {
			return nil, fmt.Errorf("blob not found: %s", uuid)
		}
		return content.Expand(tx, frid)
	})
	if err != nil {
		return fmt.Errorf("R-card check: %w", err)
	}
	if d.R != got {
		return fmt.Errorf("R-card check: manifest R card %q, tree digest %q", d.R, got)
	}
	return nil
}

func markPlinkAndEvent(tx *db.Tx, opts CheckinOpts, manifestRid libfossil.FslID) error {
	// plink: one row per parent; primary (isprim=1) is opts.Parent, merge
	// parents are secondary (isprim=0). Matches the sync-path layout in
	// crosslink.go so merged history looks identical however it arrived.
	mtime := libfossil.TimeToTextJulian(opts.Time)
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

	// event. omtime is the check-in's own time, as fossil keeps it.
	if _, err := tx.Exec(
		`INSERT INTO event(type, mtime, objid, user, comment, omtime)
		 VALUES('ci', ?, ?, ?, ?, ?)`,
		mtime, manifestRid, opts.User, opts.Comment, mtime,
	); err != nil {
		return fmt.Errorf("event: %w", err)
	}

	// leaf is deliberately not written here. Whether this checkin or any of its
	// parents is a leaf depends on the branch tags of both ends of every plink
	// edge, and this checkin's own branch tag is not applied until after the
	// transaction commits -- so Checkin runs the leaf check once the tag work
	// is done, the same deferral fossil gets from running its pending leaf
	// checks at commit time (see leaf.Pending).

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

func GetManifest(r *repo.Repo, rid libfossil.FslID) (*deck.Deck, error) {
	return GetManifestCached(r, rid, nil)
}

// GetManifestCached is GetManifest that serves the manifest's own delta-chain
// expansion through cache. A nil cache behaves exactly like GetManifest:
// content.Cache's nil receiver falls through to content.Expand. Passing a live
// cache pays off only for a caller that reads many manifests whose delta chains
// overlap -- annotate walking one file's whole history re-expands the same
// chain interiors at every revision -- so a single-manifest read gains nothing
// and should pass nil.
func GetManifestCached(r *repo.Repo, rid libfossil.FslID, cache *content.Cache) (result *deck.Deck, err error) {
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
	data, err := cache.Expand(r.DB(), rid)
	if err != nil {
		return nil, fmt.Errorf("manifest.GetManifest: %w", err)
	}
	return deck.Parse(data)
}

func applyDelta(tx *db.Tx, d *deck.Deck, fullFCards []deck.FileCard, parentRid libfossil.FslID) error {
	// Bare Expand: building one delta manifest reads its parent and baseline
	// once each. This is the commit path, not a history sweep -- no repeated
	// overlapping-chain access for a cache to pay off against.
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

	baseRid, ok := content.AvailableByUUID(tx, baselineUUID)
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

	// artifact-format-01 4.7.3: a writer MUST NOT emit a B card naming a
	// delta manifest (fossil asserts the same in checkin.c). When the parent's
	// baseline is itself a delta, leave d a full baseline manifest.
	if baseDeck.B != "" {
		return nil
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
