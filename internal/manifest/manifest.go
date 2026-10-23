package manifest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
)

type CheckinOpts struct {
	Files   []File
	Comment string
	// User is the name the U card carries: fossil's commit --user-override,
	// not --user. It is never checked against the user table, so any name
	// commits. The check-in's rcvfrom row credits User's uid when the table
	// holds that login and the repository's local (setup) user otherwise,
	// as fossil credits g.userUid whatever the U card says.
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
	// AvoidSubmanifests suppresses tree emission for this commit, as
	// draft-fossil-submanifests-00 §7.2 requires of a commit made in a
	// sync session whose server sent pragma avoid-submanifests (the
	// session's SyncResult.AvoidSubmanifests).
	AvoidSubmanifests bool
	// NoSign stores the manifest unsigned even when the repository's
	// clearsign setting is on: fossil's commit --nosign.
	NoSign bool
}

type File struct {
	Name    string
	Content []byte
	Perm    string
	OldName string // prior pathname when this file card records a rename
	// UUID, when set, carries an artifact the repository already holds into
	// the check-in by its name, with Content nil: the F card names it
	// verbatim and nothing is stored. A sparse checkout's commit carries its
	// out-of-cone files this way (draft-fossil-sparse-cone-00 §4.2).
	UUID string
}

func Checkin(r *repo.Repo, opts CheckinOpts) (libfossil.FslID, string, error) {
	return CheckinContext(context.Background(), r, opts)
}

// CheckinContext checks cancellation again before publishing its transaction.
func CheckinContext(
	ctx context.Context, r *repo.Repo, opts CheckinOpts,
) (manifestRid libfossil.FslID, manifestUUID string, err error) {
	if ctx == nil {
		panic("manifest.CheckinContext: nil context")
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
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

	var tree checkinTree
	// A check-in is one receipt, credited to its user, as a fossil commit
	// records one rcvfrom row for everything it stores.
	uid, err := r.LocalUID(opts.User)
	if err != nil {
		return 0, "", fmt.Errorf("manifest.Checkin: %w", err)
	}
	err = r.WithReceipt(&db.Receipt{UID: uid}).WithTx(func(tx *db.Tx) error {
		var txErr error
		manifestRid, manifestUUID, tree, txErr = checkinTx(ctx, tx, opts)
		if txErr != nil {
			return txErr
		}
		return ctx.Err()
	})
	if err != nil {
		return 0, "", fmt.Errorf("manifest.Checkin: %w", err)
	}
	tree.afterCommit(r)
	return manifestRid, manifestUUID, nil
}

// checkinTx stores a check-in and derives its rows, all in tx.
func checkinTx(
	ctx context.Context, tx *db.Tx, opts CheckinOpts,
) (libfossil.FslID, string, checkinTree, error) {
	if opts.Precheck != nil {
		if err := opts.Precheck(tx); err != nil {
			return 0, "", checkinTree{}, err
		}
	}
	parents := append([]libfossil.FslID{opts.Parent}, opts.MergeParents...)
	if err := RequireParentContent(ctx, tx, parents); err != nil {
		return 0, "", checkinTree{}, err
	}
	// A sparse store (draft-fossil-sparse-cone-00 §5) lacks the content of
	// files its cone leaves out. The check-in names them by hash, as the
	// parent did, and carries no R card.
	storeCone, err := content.StoreCone(tx)
	if err != nil {
		return 0, "", checkinTree{}, err
	}
	fCards, fileRids, err := storeFileBlobs(tx, opts.Files, storeCone)
	if err != nil {
		return 0, "", checkinTree{}, err
	}
	d, tree, err := checkinDeck(tx, opts, fCards, storeCone)
	if err != nil {
		return 0, "", tree, err
	}
	cache := content.NewCache(crosslinkCacheBytes)
	if err := preflightCheckinMlinks(tx, cache, d, storeCone); err != nil {
		return 0, "", tree, fmt.Errorf("preflight checkin mlinks: %w", err)
	}
	rid, uuid, err := insertCheckinBlob(ctx, tx, d, storeCone, !opts.NoSign)
	if err != nil {
		return 0, "", tree, err
	}
	if err := VerifyRCard(tx, cache, rid); err != nil {
		return 0, "", tree, err
	}
	if err := insertCheckinMlinks(tx, cache, rid, d); err != nil {
		return 0, "", tree, err
	}

	// Store the parent check-in manifest as a delta against this one,
	// the same backwards direction used for file content. Canonical does
	// this at the end of commit (src/checkin.c:3234,
	// `content_deltify(vid, &nvid, 1, 0)`). File content is handled by
	// insertCheckinMlinks above; this covers the manifests themselves.
	if opts.Parent > 0 {
		if _, err := content.Deltify(tx, opts.Parent, rid); err != nil {
			return 0, "", tree, fmt.Errorf("deltify parent manifest: %w", err)
		}
	}
	if err := markPlinkAndEvent(tx, opts, rid); err != nil {
		return 0, "", tree, err
	}

	// Mark file blobs as unsent so sync pushes them.
	// (unclustered is handled by blob.Store automatically.)
	for _, frid := range fileRids {
		if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", frid); err != nil {
			return 0, "", tree, fmt.Errorf("unsent file: %w", err)
		}
	}
	return rid, uuid, tree, linkCheckinTags(tx, opts, rid, d.T)
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
	// the branch writes above scheduled is checked (draft-fossil-repo-state-00
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

func storeFileBlobs(tx *db.Tx, files []File, storeCone *cone.Cone) ([]deck.FileCard, []libfossil.FslID, error) {
	fCards := make([]deck.FileCard, len(files))
	var rids []libfossil.FslID
	for i, f := range files {
		if err := checkWriterName(f.Name); err != nil {
			return nil, nil, err
		}
		if err := checkWriterName(f.OldName); err != nil {
			return nil, nil, err
		}
		var rid libfossil.FslID
		uuid := f.UUID
		held := true
		var err error
		if uuid != "" {
			rid, held, err = checkCarried(tx, f, storeCone)
		} else {
			rid, uuid, err = blob.Store(tx, f.Content)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("storing file %q: %w", f.Name, err)
		}
		// Every check-in is public, including carried content and known
		// phantoms. Only held content can be offered in the unsent queue.
		if rid > 0 {
			if err := content.MakePublic(tx, int64(rid)); err != nil {
				return nil, nil, fmt.Errorf("publish file %q: %w", f.Name, err)
			}
		}
		fCards[i] = deck.FileCard{Name: f.Name, UUID: uuid, Perm: f.Perm, OldName: f.OldName}
		if held {
			rids = append(rids, rid)
		}
	}
	return fCards, rids, nil
}

// checkCarried requires a carried file's artifact to be held: the R card
// reads its content, and a check-in naming content its writer lacks would
// be committed blind. The exception is a file the store cone leaves out,
// whose content a sparse store deliberately lacks: §5.3 references it by
// hash without possession, and the check-in carries no R card.
func checkCarried(tx *db.Tx, f File, storeCone *cone.Cone) (libfossil.FslID, bool, error) {
	if f.Content != nil {
		panic("manifest.checkCarried: a carried file has content: " + f.Name)
	}
	rid, held, err := content.CheckAvailableByUUID(tx, f.UUID)
	if err != nil {
		return 0, false, err
	}
	if held {
		return rid, true, nil
	}
	if !elidable(storeCone, f.Name) {
		return 0, false, fmt.Errorf("carried file %q: artifact %s is not available", f.Name, f.UUID)
	}
	// Availability deliberately returns no RID for missing content. Recover
	// an existing phantom's identity so publication clears its private flag.
	err = tx.QueryRow("SELECT coalesce((SELECT rid FROM blob WHERE uuid=?), 0)", f.UUID).Scan(&rid)
	return rid, false, err
}

// elidable reports whether a sparse store may lack the content of the file
// at name: its store cone does not cover the name. A dense store, with no
// cone, may lack nothing.
func elidable(storeCone *cone.Cone, name string) bool {
	return storeCone != nil && !storeCone.Covers(name)
}

// checkinDeck builds a check-in's manifest. With sub-manifest emission
// enabled the file set goes to a tree, stored first
// (draft-fossil-submanifests-00 §6.2), and a tree carries no B card (§4.4),
// so a tree check-in is never a delta.
func checkinDeck(
	tx *db.Tx, opts CheckinOpts, fCards []deck.FileCard, storeCone *cone.Cone,
) (*deck.Deck, checkinTree, error) {
	tree, err := storeTreeFor(tx, opts, fCards)
	if err != nil {
		return nil, tree, err
	}
	if len(tree.s) > 0 {
		opts.Delta = false
	}
	d, err := buildCheckinDeck(tx, opts, fCards, storeCone)
	if err != nil {
		return nil, tree, err
	}
	if len(tree.s) > 0 {
		d.F, d.S = nil, tree.s
	}
	return d, tree, nil
}

func buildCheckinDeck(
	tx *db.Tx, opts CheckinOpts, fCards []deck.FileCard, storeCone *cone.Cone,
) (*deck.Deck, error) {
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

	// The R card is an aggregate over every file's content, which a sparse
	// store does not hold; a commit over one omits it, which is conforming
	// (draft-fossil-sparse-cone-00 §4.3).
	if storeCone != nil {
		return d, nil
	}
	rHash, err := checkinRCard(tx, fCards)
	if err != nil {
		return nil, err
	}
	d.R = rHash
	return d, nil
}

// checkinRCard is the R card over a check-in's full file set, whatever form
// its manifest takes.
func checkinRCard(tx *db.Tx, fCards []deck.FileCard) (string, error) {
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
		return "", fmt.Errorf("R-card: %w", err)
	}
	return rHash, nil
}

// insertCheckinBlob stores d's manifest, clear-signed when sign is set and
// the repository's clearsign setting is on, as fossil's commit signs it
// (src/checkin.c): the signed text is the artifact, so its name is the hash
// of the signed bytes. A signer that fails abandons the commit, as fossil's
// does when it cannot prompt.
func insertCheckinBlob(
	ctx context.Context, tx *db.Tx, d *deck.Deck, storeCone *cone.Cone, sign bool,
) (libfossil.FslID, string, error) {
	// Writer postcondition (AF-6.3-4): ComputeR yields lowercase hex, so this
	// can only trip on a programmer error upstream. Over a sparse store there
	// is no R card at all.
	if storeCone != nil {
		if d.R != "" {
			panic("manifest.insertCheckinBlob: an R card over a sparse store")
		}
	} else if len(d.R) != 32 || strings.ToLower(d.R) != d.R {
		panic("manifest.insertCheckinBlob: R card must be 32 lowercase hex digits")
	}
	manifestBytes, err := d.Marshal()
	if err != nil {
		return 0, "", fmt.Errorf("marshal: %w", err)
	}
	if sign {
		signer, err := clearsign.FromSettings(tx)
		if err != nil {
			return 0, "", err
		}
		if signer != nil {
			if manifestBytes, err = signer.Sign(ctx, manifestBytes); err != nil {
				return 0, "", fmt.Errorf("abandoning commit: manifest signing failed: %w", err)
			}
		}
	}
	rid, uuid, err := blob.Store(tx, manifestBytes)
	if err != nil {
		return 0, "", fmt.Errorf("store manifest: %w", err)
	}
	return rid, uuid, nil
}

// VerifyRCard is the commit-time self-check of artifact-format-00 6.3.4
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
//
// An absent R card is not damage, and there is nothing to check (§4.3 of
// draft-fossil-sparse-cone-00). Nor is there over a sparse store, which
// lacks the content outside its cone: the aggregate is uncomputable, so the
// check is skipped rather than reported as a disagreement.
func VerifyRCard(tx *db.Tx, cache *content.Cache, rid libfossil.FslID) error {
	if rid <= 0 {
		panic("manifest.VerifyRCard: rid must be positive")
	}
	storeCone, err := content.StoreCone(tx)
	if err != nil {
		return fmt.Errorf("R-card check: %w", err)
	}
	if storeCone != nil {
		return nil
	}
	d, mf, err := loadCheckinManifest(tx, cache, rid)
	if err != nil {
		return fmt.Errorf("R-card check: %w", err)
	}
	if d.R == "" {
		return nil
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

	// artifact-format-00 4.7.3: a writer MUST NOT emit a B card naming a
	// delta manifest (fossil asserts the same in checkin.c). When the parent's
	// baseline is itself a delta, leave d a full baseline manifest. The same
	// goes for a tree baseline: draft-fossil-submanifests-00 §5.4 says a
	// writer with emission off SHOULD NOT create a delta against one, and with
	// emission on this writer keeps zit's rule of none either.
	if baseDeck.B != "" || isTree(baseDeck) {
		return nil
	}
	baseCards, err := baselineFileCards(tx, nil, baseDeck)
	if err != nil {
		return fmt.Errorf("baseline files: %w", err)
	}
	baseFiles := make(map[string]string, len(baseCards))
	for _, f := range baseCards {
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
