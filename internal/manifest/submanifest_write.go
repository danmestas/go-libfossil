package manifest

// The writer-side settings of draft-fossil-submanifests-00 and the decisions
// they drive: whether a commit emits a tree and at which k (§8.1, §6.3), how
// a local request enables emission and pins k (§8.1, §6.3), the manual and
// the automatic re-tune (§6.4), and storing a tree beside its manifest
// (§6.2). Defaults leave every commit byte-identical to a writer that has
// never heard of trees.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// The three project-scope settings, named as configuration exchange carries
// them (§6.3, §8.1, §6.4).
const (
	SettingEnableSubmanifests = "enable-submanifests"
	SettingSubmanifestK       = "submanifest-k"
	SettingAutoRetune         = "submanifest-auto-retune"
)

// SubmanifestEnableWarning is what an operator is told on enabling emission
// (§8.1, §7.2, §8.2): the consequences that cannot be undone. The wording is
// zit's, so both writers warn alike.
const SubmanifestEnableWarning = "enabling sub-manifests makes this repository write tree manifests; " +
	"from the first one, peers that do not announce sub-manifest support will be refused every S-bearing artifact rather " +
	"than receive a converted or downgraded one, and disabling the setting later cannot retire " +
	"the artifacts already written. While this specification is provisional a repository " +
	"containing an S-bearing artifact must not be synchronized with any repository that is not " +
	"itself a disposable test fixture."

// ParseSettingBool reads a boolean setting as zit and fossil write one: on,
// off, 1 or 0, and fossil's yes, no, true and false.
func ParseSettingBool(v string) (value, ok bool) {
	switch strings.ToLower(v) {
	case "on", "1", "yes", "true":
		return true, true
	case "off", "0", "no", "false":
		return false, true
	}
	return false, false
}

// ParseSettingK reads submanifest-k: a decimal from MinPartitionK to
// MaxPartitionK inclusive (§6.3).
func ParseSettingK(v string) (int, bool) {
	k, err := strconv.Atoi(v)
	if err != nil || !validPartitionK(k) {
		return 0, false
	}
	return k, true
}

func validPartitionK(k int) bool {
	return k >= MinPartitionK && k <= MaxPartitionK
}

// submanifestSettings are the three settings as stored.
type submanifestSettings struct {
	enabled    bool
	k          int    // the pin, or 0 when there is no valid one
	rawK       string // the stored text, for the out-of-range report
	autoRetune bool
}

func readSubmanifestSettings(q db.Querier) (submanifestSettings, error) {
	var s submanifestSettings
	read := func(name string) (string, error) {
		var value sql.NullString
		err := q.QueryRow("SELECT value FROM config WHERE name=?", name).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return value.String, nil
	}
	enable, err := read(SettingEnableSubmanifests)
	if err != nil {
		return s, err
	}
	if s.rawK, err = read(SettingSubmanifestK); err != nil {
		return s, err
	}
	retune, err := read(SettingAutoRetune)
	if err != nil {
		return s, err
	}
	s.enabled, _ = ParseSettingBool(enable)
	s.k, _ = ParseSettingK(s.rawK)
	s.autoRetune, _ = ParseSettingBool(retune)
	return s, nil
}

// emissionK is the k a commit partitions at, or 0 for a flat manifest:
// §8.1 emits no S card unless enable-submanifests is on, nor after the
// session's server sent avoid-submanifests (§7.2); §6.3 falls back to flat,
// and reports it, when the stored k is not a value from 1 to 12.
func emissionK(s submanifestSettings, avoid bool) int {
	if !s.enabled || avoid {
		return 0
	}
	if s.k == 0 {
		// zit's report, naming this library's re-tune command.
		slog.Warn(fmt.Sprintf("sub-manifests: pinned submanifest-k is out of range (%s; must be %d..%d)"+
			" — emitting a flat manifest. Re-pin with 'libfossil repo submanifests retune --k N'.",
			s.rawK, MinPartitionK, MaxPartitionK))
	}
	return s.k
}

// writeSubmanifestSetting stores one of the three settings with fossil's
// seconds-since-epoch mtime, so configuration exchange carries it.
func writeSubmanifestSetting(q db.Querier, name, value string) error {
	if _, err := q.Exec("REPLACE INTO config(name, value, mtime) VALUES(?, ?, strftime('%s','now'))",
		name, value); err != nil {
		return fmt.Errorf("config %s: %w", name, err)
	}
	return nil
}

// errNoSelection is §6.3's automatic selection without a result.
var errNoSelection = errors.New("no partition parameter can be selected")

// EnableSubmanifests is §8.1's local request to turn emission on. A valid
// pin takes precedence: an explicit k that differs from it is refused, since
// changing it is a re-tune. A stored k outside 1 to 12 is no valid pin and
// not absent either: an explicit k replaces it, and a request without one is
// refused. With no stored k, an explicit k (1 to 12) is
// pinned, and k = 0 asks for automatic selection over current, the
// check-in the next commit will descend from (0 for none). Selection is
// zit's: the §6.3 snapshot over current, refined by a replay of recent
// history when there is one. It only reads, outside any write transaction;
// the settings are then re-read and written in a short one, and a pin that
// appeared meanwhile takes precedence as §8.1 says. A request that cannot
// be met changes nothing. It returns the pinned k; the caller owes the
// operator SubmanifestEnableWarning.
func EnableSubmanifests(r *repo.Repo, current libfossil.FslID, k int) (pinned int, err error) {
	if r == nil {
		panic("manifest.EnableSubmanifests: r must not be nil")
	}
	return enableWith(r, k, func() (int, error) { return selectK(r.DB(), current) })
}

// enableWith is EnableSubmanifests with its selection supplied.
func enableWith(r *repo.Repo, k int, selection func() (int, error)) (pinned int, err error) {
	before, err := readSubmanifestSettings(r.DB())
	if err != nil {
		return 0, fmt.Errorf("manifest.EnableSubmanifests: %w", err)
	}
	proposed, err := enableDecision(before, k, selection)
	if err != nil {
		return 0, fmt.Errorf("manifest.EnableSubmanifests: %w", err)
	}
	err = r.WithTx(func(tx *db.Tx) error {
		now, err := readSubmanifestSettings(tx)
		if err != nil {
			return err
		}
		pinned = proposed
		if now.rawK != before.rawK {
			pinned, err = enableDecision(now, k, func() (int, error) {
				return 0, errors.New("the pin changed while selecting; retry")
			})
			if err != nil {
				return err
			}
		}
		if pinned != now.k {
			if err := writeSubmanifestSetting(tx, SettingSubmanifestK, strconv.Itoa(pinned)); err != nil {
				return err
			}
		}
		if now.enabled {
			return nil
		}
		return writeSubmanifestSetting(tx, SettingEnableSubmanifests, "on")
	})
	if err != nil {
		return 0, fmt.Errorf("manifest.EnableSubmanifests: %w", err)
	}
	return pinned, nil
}

// enableDecision is §8.1's precedence for a request with explicit k (0 for
// none) against settings s; selection runs only when nothing else decides.
func enableDecision(s submanifestSettings, k int, selection func() (int, error)) (int, error) {
	switch {
	case s.k != 0 && k != 0 && k != s.k:
		return 0, fmt.Errorf("submanifest-k is pinned to %d; changing it is a re-tune", s.k)
	case s.k != 0:
		return s.k, nil
	case k == 0 && s.rawK != "":
		return 0, fmt.Errorf("submanifest-k is stored as %q, outside %d..%d; enable with an explicit k",
			s.rawK, MinPartitionK, MaxPartitionK)
	case k != 0:
		if !validPartitionK(k) {
			return 0, fmt.Errorf("submanifest-k %d is outside %d..%d", k, MinPartitionK, MaxPartitionK)
		}
		return k, nil
	}
	return selection()
}

// selectK is zit's selection: the §6.3 snapshot over current, then the
// replay of recent history, which outranks it when it can speak
// (submanifest_replay.go).
func selectK(q db.Querier, current libfossil.FslID) (int, error) {
	snapshot, err := selectForCheckin(q, current)
	if err != nil {
		return 0, err
	}
	k, ok, err := replayK(q, snapshot)
	if err != nil || !ok {
		return snapshot, err
	}
	return k, nil
}

// NewestCheckin is the newest check-in by time, the highest rid breaking a
// tie, or 0 when the repository has none.
func NewestCheckin(q db.Querier) (libfossil.FslID, error) {
	var rid int64
	err := q.QueryRow("SELECT objid FROM event WHERE type='ci' ORDER BY mtime DESC, objid DESC LIMIT 1").Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("newest check-in: %w", err)
	}
	return libfossil.FslID(rid), nil
}

// selectForCheckin runs §6.3's automatic selection over current's file set.
func selectForCheckin(q db.Querier, current libfossil.FslID) (int, error) {
	if current <= 0 {
		return 0, fmt.Errorf("%w: there is no current check-in", errNoSelection)
	}
	d, err := loadCheckinDeck(q, current)
	if err != nil {
		return 0, err
	}
	files, err := checkinFileCards(q, nil, d)
	if err != nil {
		return 0, fmt.Errorf("files of the current check-in: %w", err)
	}
	k, ok := SelectPartitionK(files)
	if !ok {
		return 0, fmt.Errorf("%w: no k from %d to %d partitions the current check-in's %d files",
			errNoSelection, MinPartitionK, MaxPartitionK, len(files))
	}
	return k, nil
}

// loadCheckinDeck parses the check-in manifest at rid.
func loadCheckinDeck(q db.Querier, rid libfossil.FslID) (*deck.Deck, error) {
	data, err := content.Expand(q, rid)
	if err != nil {
		return nil, fmt.Errorf("expand check-in %d: %w", rid, err)
	}
	d, err := deck.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse check-in %d: %w", rid, err)
	}
	if d.Type != deck.Checkin {
		return nil, fmt.Errorf("artifact %d is not a check-in", rid)
	}
	return d, nil
}

// SubmanifestDisableNote is zit's note on disabling emission in a
// repository that already holds trees.
const SubmanifestDisableNote = "note: this repository already holds S-bearing artifacts. Disabling stops new\n" +
	"tree manifests; it cannot retire the existing ones, and peers that do not\n" +
	"announce sub-manifest support are still refused every one of them."

// DisableSubmanifests turns emission off (§8.1): later commits are flat, and
// the pin stays, so re-enabling is not a re-tune. Existing trees cannot be
// retired; when the repository holds any it returns SubmanifestDisableNote.
func DisableSubmanifests(r *repo.Repo) (note string, err error) {
	if r == nil {
		panic("manifest.DisableSubmanifests: r must not be nil")
	}
	err = r.WithTx(func(tx *db.Tx) error {
		if err := writeSubmanifestSetting(tx, SettingEnableSubmanifests, "off"); err != nil {
			return err
		}
		seen, err := SeenSubmanifest(tx)
		if seen {
			note = SubmanifestDisableNote
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("manifest.DisableSubmanifests: %w", err)
	}
	return note, nil
}

// RetuneSubmanifests is §6.4's manual, operator-initiated re-tune: it pins
// k, from 1 to 12, for the commits that follow. History is not rewritten.
func RetuneSubmanifests(r *repo.Repo, k int) error {
	if r == nil {
		panic("manifest.RetuneSubmanifests: r must not be nil")
	}
	if !validPartitionK(k) {
		return fmt.Errorf("manifest.RetuneSubmanifests: k %d is outside %d..%d", k, MinPartitionK, MaxPartitionK)
	}
	return r.WithTx(func(tx *db.Tx) error {
		return writeSubmanifestSetting(tx, SettingSubmanifestK, strconv.Itoa(k))
	})
}

// storedTree is what storing a check-in's tree did: its S run, empty when
// the construction is flat, and the sub-manifests that filled phantoms.
type storedTree struct {
	s      []deck.SubmanifestCard
	filled []libfossil.FslID
}

// storeCheckinTree stores the canonical tree of files at k, children before
// parents (§6.2), each named under the repository's hash policy alone. A
// check-in is public, so every sub-manifest it names is made public, as
// fossil's commit makes its blobs public; only sub-manifests it wrote are
// queued for sync. It records the seen-submanifest indicator (§8.1).
func storeCheckinTree(tx *db.Tx, files []deck.FileCard, k int) (storedTree, error) {
	var st storedTree
	tree, err := BuildTree(files, k, func(data []byte) (string, error) {
		return storeSubmanifest(tx, data, &st)
	})
	if err != nil || len(tree.S) == 0 {
		return storedTree{}, err
	}
	if err := MarkSeenSubmanifest(tx); err != nil {
		return storedTree{}, err
	}
	st.s = tree.S
	return st, nil
}

// storeSubmanifest stores one sub-manifest for storeCheckinTree.
func storeSubmanifest(tx *db.Tx, data []byte, st *storedTree) (string, error) {
	rid, uuid, outcome, err := blob.StoreUnderPolicy(tx, data)
	if err != nil {
		return "", fmt.Errorf("store sub-manifest: %w", err)
	}
	if outcome == blob.FilledPhantom {
		st.filled = append(st.filled, rid)
	}
	wasPrivate := content.IsPrivate(tx, int64(rid))
	if err := content.MakePublic(tx, int64(rid)); err != nil {
		return "", fmt.Errorf("publish sub-manifest: %w", err)
	}
	// Queue what this commit wrote, and what it just made public: a private
	// sub-manifest was never sent.
	if outcome != blob.Found || wasPrivate {
		if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", rid); err != nil {
			return "", fmt.Errorf("unsent sub-manifest: %w", err)
		}
	}
	return uuid, nil
}

// checkinTree is a commit's tree decision: the settings it read, the k it
// partitions at (0 for flat), its files in canonical order and what storing
// the tree did.
type checkinTree struct {
	settings submanifestSettings
	k        int
	files    []deck.FileCard
	storedTree
}

// storeTreeFor makes the tree decision for a check-in of fCards and, when
// it yields a tree, stores the tree.
func storeTreeFor(tx *db.Tx, opts CheckinOpts, fCards []deck.FileCard) (checkinTree, error) {
	var ct checkinTree
	var err error
	if ct.settings, err = readSubmanifestSettings(tx); err != nil {
		return ct, fmt.Errorf("sub-manifest settings: %w", err)
	}
	if ct.k = emissionK(ct.settings, opts.AvoidSubmanifests); ct.k == 0 {
		return ct, nil
	}
	ct.files = make([]deck.FileCard, len(fCards))
	copy(ct.files, fCards)
	sort.Slice(ct.files, func(i, j int) bool { return deck.Compare(ct.files[i].Name, ct.files[j].Name) < 0 })
	for i := 1; i < len(ct.files); i++ {
		if ct.files[i-1].Name == ct.files[i].Name {
			return ct, fmt.Errorf("file %q is listed twice", ct.files[i].Name)
		}
	}
	ct.storedTree, err = storeCheckinTree(tx, ct.files, ct.k)
	return ct, err
}

// afterCommit is what a tree commit owes once it is durable, as zit runs it:
// the check-ins waiting on a sub-manifest it filled are linked now rather
// than at the next sweep (§7.1, §7.3), and the §6.4 automatic re-tune runs.
// Neither can fail the commit; a failure is reported.
func (ct checkinTree) afterCommit(r *repo.Repo) {
	if len(ct.filled) > 0 {
		ctx := context.Background()
		linked := AfterDephantomizeBatch(ctx, r, ct.filled)
		if _, err := CrosslinkAfterCascades(ctx, r, linked); err != nil {
			slog.Warn("manifest.Checkin: linking check-ins that waited on a sub-manifest", "err", err)
		}
	}
	if ct.k == 0 {
		return
	}
	if err := r.WithTx(ct.retune); err != nil {
		slog.Warn("manifest.Checkin: sub-manifest re-tune failed; the commit stands", "err", err)
	}
}

// retune is §6.4's automatic re-tune over the commit's file set, the
// current check-in once the commit is durable: while
// submanifest-auto-retune is on it re-pins k when the gate clears;
// otherwise it only advises. It re-reads the settings first, so a manual
// re-tune, or the automatic one switched off, since the commit stands.
func (ct checkinTree) retune(tx *db.Tx) error {
	target, ok := retuneTarget(partitionCosts(ct.files), ct.k)
	if !ok {
		return nil
	}
	now, err := readSubmanifestSettings(tx)
	if err != nil {
		return err
	}
	if now.k != ct.k {
		return nil
	}
	if !now.autoRetune {
		slog.Info("manifest.Checkin: submanifest-k has drifted from the analyzed optimum",
			"submanifest-k", ct.k, "optimum", target)
		return nil
	}
	return writeSubmanifestSetting(tx, SettingSubmanifestK, strconv.Itoa(target))
}
