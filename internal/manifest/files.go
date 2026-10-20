package manifest

import (
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

type FileEntry struct {
	Name string
	UUID string
	Perm string
}

func ListFiles(r *repo.Repo, rid libfossil.FslID) ([]FileEntry, error) {
	return ListFilesCached(r, rid, nil)
}

// ListFilesCached is ListFiles that serves the checkin manifest -- and, for a
// delta manifest, its baseline, and for a tree manifest, its sub-manifests --
// through cache. A nil cache is exactly ListFiles (content.Cache's nil
// receiver falls through to content.Expand). A live cache helps only a caller
// listing many overlapping revisions, such as annotate walking one file's
// history; see GetManifestCached.
func ListFilesCached(r *repo.Repo, rid libfossil.FslID, cache *content.Cache) ([]FileEntry, error) {
	d, err := GetManifestCached(r, rid, cache)
	if err != nil {
		return nil, fmt.Errorf("manifest.ListFiles: %w", err)
	}
	cards, err := checkinFileCards(r.DB(), cache, d)
	if err != nil {
		return nil, fmt.Errorf("manifest.ListFiles: %w", err)
	}
	return fCardsToEntries(cards), nil
}

// FileFilter narrows an enumeration to the files a sparse cone covers: the
// range predicate draft-fossil-sparse-cone-00 §4.1 applies at this seam, so
// every consumer of a narrowed file set gets it from one place. Covers
// decides one name; MayCover reports whether any covered name n could have
// first <= n < limit (an empty limit is unbounded), and may err toward true.
type FileFilter interface {
	Covers(name string) bool
	MayCover(first, limit string) bool
}

// ListFilesFiltered is ListFiles narrowed to the files filter covers; a nil
// filter lists every file. On a tree manifest the walk skips every
// sub-manifest whose key range filter cannot cover, so the cost follows the
// covered part and the spine rather than the whole tree (§1.3, §4.5), and a
// skipped node need not be held at all.
func ListFilesFiltered(r *repo.Repo, rid libfossil.FslID, filter FileFilter) ([]FileEntry, error) {
	if filter == nil {
		return ListFiles(r, rid)
	}
	d, err := GetManifestCached(r, rid, nil)
	if err != nil {
		return nil, fmt.Errorf("manifest.ListFilesFiltered: %w", err)
	}
	if d.Type == deck.Submanifest {
		return nil, fmt.Errorf("manifest.ListFilesFiltered: a sub-manifest is a slice of a check-in, not a check-in")
	}
	cards, err := checkinFileCardsFiltered(r.DB(), nil, d, filter)
	if err != nil {
		return nil, fmt.Errorf("manifest.ListFilesFiltered: %w", err)
	}
	kept := cards[:0:0]
	for _, c := range cards {
		if filter.Covers(c.Name) {
			kept = append(kept, c)
		}
	}
	return fCardsToEntries(kept), nil
}

// checkinFileCards is the enumeration seam of draft-fossil-submanifests-00
// §9.3: the one place a check-in's complete file set is assembled, in
// comparator order and with no delete markers, whatever form the manifest
// takes -- flat, delta (artifact-format-00 8.2), tree (§5.1) or a delta over a
// tree (§5.4). Every reader of a whole file set goes through here, or through
// ownFileCards for a manifest's own cards; nothing else reads F cards as a
// file set.
func checkinFileCards(q db.Querier, cache *content.Cache, d *deck.Deck) ([]deck.FileCard, error) {
	return checkinFileCardsFiltered(q, cache, d, nil)
}

func checkinFileCardsFiltered(q db.Querier, cache *content.Cache, d *deck.Deck, filter FileFilter) ([]deck.FileCard, error) {
	if d.Type == deck.Submanifest {
		return nil, fmt.Errorf("a sub-manifest is a slice of a check-in, not a check-in")
	}
	if d.B == "" {
		return baselineFileCardsFiltered(q, cache, d, filter)
	}
	baseDeck, err := loadBaseline(q, cache, d)
	if err != nil {
		return nil, err
	}
	baseCards, err := baselineFileCardsFiltered(q, cache, baseDeck, filter)
	if err != nil {
		return nil, err
	}
	return mergeDeltaCards(baseCards, d.F), nil
}

// ownFileCards returns the cards a manifest itself conveys: a delta's or a flat
// manifest's F cards, delete markers included, or a tree manifest's walked
// enumeration (§5.1), which has none. It is fossil's manifest_file_seek_base
// view, which mlink derivation reads beside the merged file set.
func ownFileCards(q db.Querier, cache *content.Cache, d *deck.Deck) ([]deck.FileCard, error) {
	if d.Type == deck.Submanifest {
		return nil, fmt.Errorf("a sub-manifest is a slice of a check-in, not a check-in")
	}
	return baselineFileCards(q, cache, d)
}

// baselineFileCards returns the cards a manifest with no B card lists as its
// file set, or, for a delta named as another delta's baseline, that delta's
// own cards (artifact-format-00 4.7.3: one level, its B card not followed). A
// tree manifest's are its walked enumeration (§5.1), which can fail; any
// other manifest's are its F cards.
func baselineFileCards(q db.Querier, cache *content.Cache, d *deck.Deck) ([]deck.FileCard, error) {
	return baselineFileCardsFiltered(q, cache, d, nil)
}

func baselineFileCardsFiltered(q db.Querier, cache *content.Cache, d *deck.Deck, filter FileFilter) ([]deck.FileCard, error) {
	if isTree(d) {
		return walkTreeFiltered(q, cache, d, filter)
	}
	return d.F, nil
}

// mergeDeltaCards is the merge walk of artifact-format-00 8.2: the baseline's
// cards overlaid with the delta's own, a delta's delete marker removing the
// name. A delete marker on the baseline side -- a named delta's own -- adds no
// file either (4.7.3), so no card in the result is hashless.
func mergeDeltaCards(baseCards, own []deck.FileCard) []deck.FileCard {
	byName := make(map[string]deck.FileCard, len(baseCards)+len(own))
	for _, f := range baseCards {
		if f.UUID != "" {
			byName[f.Name] = f
		}
	}
	for _, f := range own {
		if f.UUID == "" {
			delete(byName, f.Name)
		} else {
			byName[f.Name] = f
		}
	}
	merged := make([]deck.FileCard, 0, len(byName))
	for _, f := range byName {
		merged = append(merged, f)
	}
	sort.Slice(merged, func(i, j int) bool { return deck.Compare(merged[i].Name, merged[j].Name) < 0 })
	return merged
}

// loadBaseline resolves the B card of delta manifest d to its baseline deck.
// The target must be a check-in (4.7.3, 8.3); a control artifact would hand
// back an empty file set as if it were whole. Per artifact-format-00 4.7.3 and
// fossil's manifest_get, the target MAY itself be a delta manifest: only its
// own F cards are used, its own B card is not followed (one level). -00 and
// #275 refused that case; -00 reverses it. It MAY also be a tree manifest
// (draft-fossil-submanifests-00 §5.4), whose file set baselineFileCards walks.
// Every reader of a B card goes through here, so the rule has one owner.
func loadBaseline(q db.Querier, cache *content.Cache, d *deck.Deck) (*deck.Deck, error) {
	if d.B == "" {
		panic("manifest.loadBaseline: d has no B card")
	}
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
	if baseDeck.Type != deck.Checkin {
		return nil, fmt.Errorf("baseline %s is not a check-in manifest", d.B)
	}
	return baseDeck, nil
}

func fCardsToEntries(fCards []deck.FileCard) []FileEntry {
	entries := make([]FileEntry, len(fCards))
	for i, f := range fCards {
		entries[i] = FileEntry{Name: f.Name, UUID: f.UUID, Perm: f.Perm}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// MergeParentFiles returns supplied with every file tracked at parentRID
// that is not already in supplied appended. Names in supplied win over the
// parent's entry; the parent's untouched files have their content expanded
// from the repo so the resulting slice is a full-tree set ready for Checkin.
//
// parentRID == 0 returns supplied unchanged.
func MergeParentFiles(r *repo.Repo, parentRID libfossil.FslID, supplied []File) ([]File, error) {
	if parentRID == 0 {
		return supplied, nil
	}
	parentFiles, err := ListFiles(r, parentRID)
	if err != nil {
		return nil, fmt.Errorf("manifest.MergeParentFiles: %w", err)
	}
	suppliedNames := make(map[string]struct{}, len(supplied))
	for _, f := range supplied {
		suppliedNames[f.Name] = struct{}{}
	}
	merged := supplied
	for _, pf := range parentFiles {
		if _, ok := suppliedNames[pf.Name]; ok {
			continue
		}
		baseRid, ok := content.AvailableByUUID(r.DB(), pf.UUID)
		if !ok {
			return nil, fmt.Errorf("manifest.MergeParentFiles: blob %s for %s not found", pf.UUID, pf.Name)
		}
		// Bare Expand: each untouched parent file is expanded once to build a
		// single commit's full-tree file set. Distinct blobs, no overlap.
		data, err := content.Expand(r.DB(), baseRid)
		if err != nil {
			return nil, fmt.Errorf("manifest.MergeParentFiles: expand %s: %w", pf.Name, err)
		}
		merged = append(merged, File{
			Name:    pf.Name,
			Content: data,
			Perm:    pf.Perm,
		})
	}
	return merged, nil
}

// LookupFile resolves one filename in a check-in's file set, as
// artifact-format-00 8.4 orders the search: a delta manifest's own F cards
// first, and only a name absent from them goes to the baseline. found is false
// when no file has the name or the delta deletes it; a delete marker answers
// without reading the baseline, so a missing baseline does not fail it. A name
// that needs the baseline fails when it cannot be resolved (8.3). The search
// uses the baseline's own F cards and does not follow a B card it carries.
func LookupFile(r *repo.Repo, rid libfossil.FslID, name string) (FileEntry, bool, error) {
	return LookupFileCached(r, rid, name, nil)
}

// LookupFileCached is LookupFile that serves the manifest and baseline through
// cache; a nil cache is exactly LookupFile. Like ListFilesCached it pays off
// only for a caller resolving one name across many overlapping revisions.
func LookupFileCached(
	r *repo.Repo, rid libfossil.FslID, name string, cache *content.Cache,
) (FileEntry, bool, error) {
	if r == nil {
		panic("manifest.LookupFile: r must not be nil")
	}
	if name == "" {
		panic("manifest.LookupFile: name must not be empty")
	}
	d, err := GetManifestCached(r, rid, cache)
	if err != nil {
		return FileEntry{}, false, fmt.Errorf("manifest.LookupFile: %w", err)
	}
	card, found, err := lookupFileCard(r.DB(), cache, d, name)
	if err != nil {
		return FileEntry{}, false, fmt.Errorf("manifest.LookupFile: %w", err)
	}
	if !found {
		return FileEntry{}, false, nil
	}
	return fileCardEntry(card)
}

// lookupFileCard is the single-name half of the enumeration seam: a delta's
// own cards first (artifact-format-00 8.4), a delete marker answering without
// the baseline, then the baseline -- by binary search of a flat one's F cards
// or by the tree lookup of draft-fossil-submanifests-00 §5.3. The card it
// returns may be a delete marker; fileCardEntry reads that as absent.
func lookupFileCard(q db.Querier, cache *content.Cache, d *deck.Deck, name string) (deck.FileCard, bool, error) {
	if d.Type == deck.Submanifest {
		return deck.FileCard{}, false, fmt.Errorf("a sub-manifest is a slice of a check-in, not a check-in")
	}
	if isTree(d) {
		return lookupTree(q, cache, d, name)
	}
	if card, ok := findFileCard(d.F, name); ok {
		return card, true, nil
	}
	if d.B == "" {
		return deck.FileCard{}, false, nil
	}
	baseDeck, err := loadBaseline(q, cache, d)
	if err != nil {
		return deck.FileCard{}, false, err
	}
	if isTree(baseDeck) {
		return lookupTree(q, cache, baseDeck, name)
	}
	card, ok := findFileCard(baseDeck.F, name)
	return card, ok, nil
}

// findFileCard binary-searches cards, which a parsed deck keeps in comparator
// order (4.5.2), for name.
func findFileCard(cards []deck.FileCard, name string) (deck.FileCard, bool) {
	i := sort.Search(len(cards), func(i int) bool {
		return deck.Compare(cards[i].Name, name) >= 0
	})
	if i < len(cards) && cards[i].Name == name {
		return cards[i], true
	}
	return deck.FileCard{}, false
}

// fileCardEntry turns a found F card into LookupFile's answer: a delete
// marker (no hash) reads as not found.
func fileCardEntry(card deck.FileCard) (FileEntry, bool, error) {
	if card.UUID == "" {
		return FileEntry{}, false, nil
	}
	return FileEntry{Name: card.Name, UUID: card.UUID, Perm: card.Perm}, true, nil
}
