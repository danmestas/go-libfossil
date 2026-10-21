package manifest

// Reading tree manifests, draft-fossil-submanifests-00 §5: the full file-set
// walk (§5.1), sub-manifest resolution (§5.2) and single-name lookup (§5.3).
// Nothing outside this file and files.go knows a check-in can be a tree; every
// file-set consumer asks the enumeration seam in files.go (§9.3), which calls
// in here for a manifest that carries S cards.

import (
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// maxTreeDepth is §5.1 step 3's bound: the sub-manifest artifacts on one
// reference path, counting the leaf. A walk holds at most maxTreeDepth+1
// frames, the manifest's included.
const maxTreeDepth = 16

// maxTreeNodes bounds the sub-manifests one enumeration or one lookup
// resolves (§10). Sub-manifests are shared by hash, so a legitimate tree of
// this many nodes already describes millions of files; past it the walk is
// treated as hostile rather than slow.
const maxTreeNodes = 1 << 20

// SubmanifestError reports that a tree cannot be enumerated because of the
// sub-manifest reference UUID: either that artifact is not available locally
// (Missing), the one fault a caller may defer on under §5.2's soft
// discipline, or the reference breaks a rule of §5.1, which is permanent for
// the bytes involved.
type SubmanifestError struct {
	UUID    string
	Missing bool
	Reason  string
}

func (e *SubmanifestError) Error() string {
	if e.Missing {
		return fmt.Sprintf("sub-manifest %s is not available", e.UUID)
	}
	return fmt.Sprintf("sub-manifest %s: %s", e.UUID, e.Reason)
}

// MissingSubmanifest reports the hash of the absent sub-manifest err names,
// if err is or wraps a SubmanifestError for an unavailable artifact.
func MissingSubmanifest(err error) (string, bool) {
	var e *SubmanifestError
	if errors.As(err, &e) && e.Missing {
		return e.UUID, true
	}
	return "", false
}

// treeFault builds the SubmanifestError for a rule the reference uuid breaks.
func treeFault(uuid, format string, args ...any) error {
	return &SubmanifestError{UUID: uuid, Reason: fmt.Sprintf(format, args...)}
}

// isTree reports whether d is a tree manifest: a manifest whose file set is
// conveyed by S cards (§5). A sub-manifest is not one.
func isTree(d *deck.Deck) bool {
	return d.Type == deck.Checkin && len(d.S) > 0
}

// subtreeLoader resolves S references to parsed sub-manifests, counting each
// resolution against maxTreeNodes.
type subtreeLoader struct {
	q        db.Querier
	cache    *content.Cache
	resolved int
}

// load resolves uuid to an available artifact of type SUBMANIFEST (§5.2):
// absent is a Missing SubmanifestError, anything else that is not a
// sub-manifest is a fault of the referencing tree.
func (l *subtreeLoader) load(uuid string) (*deck.Deck, libfossil.FslID, error) {
	if uuid == "" {
		panic("manifest.subtreeLoader.load: uuid must not be empty")
	}
	if l.resolved >= maxTreeNodes {
		return nil, 0, treeFault(uuid, "the tree resolves more than %d sub-manifests", maxTreeNodes)
	}
	l.resolved++
	rid, ok := content.AvailableByUUID(l.q, uuid)
	if !ok {
		return nil, 0, &SubmanifestError{UUID: uuid, Missing: true}
	}
	data, err := expandManifestBytes(l.q, l.cache, rid)
	if err != nil {
		return nil, 0, treeFault(uuid, "%v", err)
	}
	d, err := deck.Parse(data)
	if err != nil {
		return nil, 0, treeFault(uuid, "%v", err)
	}
	if d.Type != deck.Submanifest {
		return nil, 0, treeFault(uuid, "not a sub-manifest")
	}
	return d, rid, nil
}

// treeFrame is one artifact on the walk's stack and its cursor over its S run.
type treeFrame struct {
	rid    libfossil.FslID // 0 for the manifest itself
	cards  []deck.SubmanifestCard
	cursor int
	limit  string // the exclusive upper bound of the frame's keys; "" for none
}

// treeWalk is the state of one §5.1 enumeration.
type treeWalk struct {
	loader  *subtreeLoader
	filter  FileFilter // nil for the full enumeration
	stack   []treeFrame
	last    string
	hasLast bool
	files   []deck.FileCard
	// Only readiness inspection may skip proven outside gaps; enumeration never does.
	skipMissing func(string) bool
}

// walkTree enumerates tree manifest d as §5.1 specifies: depth first, in
// order, with an explicit stack rather than recursion. It returns the F cards
// of the leaves in enumeration order, or an error -- never a partial set
// (§5.2). Every tree fault is a SubmanifestError.
func walkTree(q db.Querier, cache *content.Cache, d *deck.Deck) ([]deck.FileCard, error) {
	return walkTreeFiltered(q, cache, d, nil)
}

// walkTreeFiltered is walkTree narrowed by filter, when it is not nil: it
// emits only the files filter covers and never resolves a sub-manifest whose
// key range filter cannot cover (draft-fossil-sparse-cone-00 §1.3, §4.1).
// Every node is a contiguous range of the sorted file list, from its S card's
// name up to the next sibling's (or its parent's bound), so a skipped node
// holds nothing the filter covers.
func walkTreeFiltered(q db.Querier, cache *content.Cache, d *deck.Deck, filter FileFilter) ([]deck.FileCard, error) {
	if !isTree(d) {
		panic("manifest.walkTree: d must be a tree manifest")
	}
	w := &treeWalk{
		loader: &subtreeLoader{q: q, cache: cache},
		filter: filter,
		stack:  []treeFrame{{cards: d.S}},
	}
	// Each step either advances a cursor or pops a frame, and every pushed
	// frame costs a resolution, so the loop is bounded by the S cards the
	// resolved artifacts carry; maxTreeNodes bounds those.
	for len(w.stack) > 0 {
		if err := w.step(); err != nil {
			return nil, err
		}
	}
	if len(w.files) == 0 && filter == nil {
		panic("manifest.walkTree: a tree enumerated no files")
	}
	return w.files, nil
}

// step performs §5.1 steps 2 to 5 once.
func (w *treeWalk) step() error {
	top := &w.stack[len(w.stack)-1]
	if top.cursor == len(top.cards) {
		w.stack = w.stack[:len(w.stack)-1] // step 2
		return nil
	}
	card := top.cards[top.cursor]
	top.cursor++
	if w.hasLast && deck.Compare(card.Name, w.last) <= 0 {
		return treeFault(card.UUID, "S-card %q does not follow %q", card.Name, w.last)
	}
	limit := top.limit
	if top.cursor < len(top.cards) {
		limit = top.cards[top.cursor].Name
	}
	if w.filter != nil && !w.filter.MayCover(card.Name, limit) {
		return nil
	}
	// The resolved artifact's depth is the sub-manifests on its path,
	// itself included: one per frame above the manifest's, plus one.
	if len(w.stack) > maxTreeDepth {
		return treeFault(card.UUID, "deeper than %d sub-manifests", maxTreeDepth)
	}
	sub, rid, err := w.loader.load(card.UUID)
	if err != nil {
		if uuid, missing := MissingSubmanifest(err); missing && w.skipMissing != nil && w.skipMissing(uuid) {
			return nil
		}
		return err
	}
	for _, frame := range w.stack {
		if frame.rid == rid {
			return treeFault(card.UUID, "the reference closes a cycle")
		}
	}
	if len(sub.F) > 0 {
		return w.emitLeaf(card, sub)
	}
	if sub.S[0].Name != card.Name { // step 5
		return treeFault(card.UUID, "S-card %q names a subtree starting at %q", card.Name, sub.S[0].Name)
	}
	w.stack = append(w.stack, treeFrame{rid: rid, cards: sub.S, limit: limit})
	return nil
}

// emitLeaf is §5.1 step 4: a leaf's F cards, the first of them named exactly
// as the S card that reached it, every one strictly after the last emitted.
func (w *treeWalk) emitLeaf(card deck.SubmanifestCard, leaf *deck.Deck) error {
	if leaf.F[0].Name != card.Name {
		return treeFault(card.UUID, "S-card %q names a leaf starting at %q", card.Name, leaf.F[0].Name)
	}
	for _, f := range leaf.F {
		if w.hasLast && deck.Compare(f.Name, w.last) <= 0 {
			return treeFault(card.UUID, "file %q does not follow %q", f.Name, w.last)
		}
		if w.filter == nil || w.filter.Covers(f.Name) {
			w.files = append(w.files, f)
		}
		w.last, w.hasLast = f.Name, true
	}
	return nil
}

// lookupTree resolves one name in tree manifest d as §5.3 specifies: the last
// S card at or before the name, one sub-manifest per level, at most
// maxTreeDepth of them. A sub-manifest on the path that cannot be resolved is
// a SubmanifestError; a name the search proves absent is (_, false, nil).
func lookupTree(q db.Querier, cache *content.Cache, d *deck.Deck, name string) (deck.FileCard, bool, error) {
	if !isTree(d) {
		panic("manifest.lookupTree: d must be a tree manifest")
	}
	loader := &subtreeLoader{q: q, cache: cache}
	cards := d.S
	for depth := 1; depth <= maxTreeDepth; depth++ {
		card, ok := lastSCardAtOrBefore(cards, name)
		if !ok {
			return deck.FileCard{}, false, nil
		}
		sub, _, err := loader.load(card.UUID)
		if err != nil {
			return deck.FileCard{}, false, err
		}
		if len(sub.F) > 0 {
			f, found := findFileCard(sub.F, name)
			return f, found, nil
		}
		cards = sub.S
	}
	return deck.FileCard{}, false, treeFault(cards[0].UUID, "lookup of %q descends deeper than %d", name, maxTreeDepth)
}

// lastSCardAtOrBefore binary-searches an S run, which the parser keeps in
// comparator order, for the last card whose filename is at or before name.
func lastSCardAtOrBefore(cards []deck.SubmanifestCard, name string) (deck.SubmanifestCard, bool) {
	lo, hi := 0, len(cards)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if deck.Compare(cards[mid].Name, name) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return deck.SubmanifestCard{}, false
	}
	return cards[lo-1], true
}
