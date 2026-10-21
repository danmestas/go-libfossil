package sync

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// The §8 bounds of single-name lookup, charged across one request.
const (
	coneWalkDepth  = 16
	coneWalkVisits = 1 << 20
)

type sparseKind uint8

const (
	sparseContent    sparseKind = iota // bytes no artifact parses from
	sparseNode                         // sub-manifest node: sent only by a walk
	sparseStructural                   // held densely (§5.1)
	sparseCheckin
)

// coneWalk is §6.2 keep-set discovery for one clone round. A row is read
// only when the round reaches it, and a check-in's tree is walked only when
// the round emits that check-in, so the work tracks the round's batch. Each
// check-in sends what its primary parent's tree does not already hold; the
// parent is a row of the same clone, so by induction every in-cone node and
// blob of every check-in is sent once its own check-in is.
type coneWalk struct {
	h          *handler
	cone       *cone.Cone
	kinds      map[int]sparseKind
	decks      map[int]*deck.Deck
	reached    map[int]bool
	path       map[string]bool
	visits     int
	classified int
}

func newConeWalk(h *handler) *coneWalk {
	if h == nil || h.cone == nil {
		panic("sync.newConeWalk: nil handler or cone")
	}
	return &coneWalk{h: h, cone: h.cone, kinds: map[int]sparseKind{},
		decks: map[int]*deck.Deck{}, reached: map[int]bool{}, path: map[string]bool{}}
}

// classify parses rid once per request. Unreadable bytes stay structural:
// they go out at their row and fail there exactly as a dense clone would.
func (w *coneWalk) classify(rid int) (sparseKind, *deck.Deck) {
	if kind, ok := w.kinds[rid]; ok {
		return kind, w.decks[rid]
	}
	w.classified++
	kind, d := sparseStructural, (*deck.Deck)(nil)
	data, err := w.h.cache.Expand(w.h.repo.DB(), libfossil.FslID(rid))
	if err == nil {
		kind = sparseContent
		if deck.HasValidZ(data) {
			if parsed, perr := deck.Parse(data); perr == nil {
				kind, d = deckKind(parsed), parsed
			}
		}
	}
	w.kinds[rid], w.decks[rid] = kind, d
	return kind, d
}

func deckKind(d *deck.Deck) sparseKind {
	switch d.Type {
	case deck.Submanifest:
		return sparseNode
	case deck.Checkin:
		return sparseCheckin
	}
	return sparseStructural
}

// sourceKept is the §6.2 keep-set test for a served delta's base.
func (w *coneWalk) sourceKept(rid int) bool {
	if w.reached[rid] {
		return true
	}
	kind, _ := w.classify(rid)
	return kind == sparseStructural || kind == sparseCheckin
}

// servable resolves uuid to a row this clone session sends: held, inside
// the snapshot bound, not shunned, and public unless the clone is private.
func (w *coneWalk) servable(uuid string) (int, bool, error) {
	var rid int
	err := w.h.repo.DB().QueryRow("SELECT rid FROM blob WHERE uuid=? AND size>=0", uuid).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("sparse walk: resolve %s: %w", uuid, err)
	}
	if rid > w.h.cloneSnapMax {
		return 0, false, nil
	}
	info, ok, err := w.h.loadCloneBlob(rid)
	if err != nil || !ok {
		return 0, false, err
	}
	return rid, !info.priv || w.h.syncPrivate, nil
}

// items returns, ascending, the rows the artifact d at a structural row
// brings with it: an attachment's source, or a check-in's new in-cone tree.
func (w *coneWalk) items(d *deck.Deck) ([]int, error) {
	out := map[int]bool{}
	var err error
	switch {
	case d.A != nil && d.A.Source != "":
		err = w.add(d.A.Source, out)
	case d.Type == deck.Checkin && len(d.S) > 0:
		prior := coneTreeLevel{}
		if parent, perr := w.parent(d); perr != nil {
			return nil, perr
		} else if parent != nil {
			prior.cards = parent.S
		}
		err = w.tree(coneTreeLevel{cards: d.S}, prior, nil, 1, out)
	case d.Type == deck.Checkin:
		err = w.flat(d, out)
	}
	if err != nil {
		return nil, err
	}
	rids := make([]int, 0, len(out))
	for rid := range out {
		w.reached[rid] = true
		rids = append(rids, rid)
	}
	sort.Ints(rids)
	return rids, nil
}

func (w *coneWalk) add(uuid string, out map[int]bool) error {
	rid, ok, err := w.servable(uuid)
	if ok {
		out[rid] = true
	}
	return err
}

// parent is the primary parent check-in when this clone sends it, else nil:
// then nothing is known to be sent already and the whole cone goes out.
func (w *coneWalk) parent(d *deck.Deck) (*deck.Deck, error) {
	if len(d.P) == 0 {
		return nil, nil
	}
	return w.checkin(d.P[0])
}

func (w *coneWalk) checkin(uuid string) (*deck.Deck, error) {
	rid, ok, err := w.servable(uuid)
	if err != nil || !ok {
		return nil, err
	}
	if kind, d := w.classify(rid); kind == sparseCheckin {
		return d, nil
	}
	return nil, nil
}

// flat sends the in-cone files of d's full file list that its parent's full
// list does not name identically.
func (w *coneWalk) flat(d *deck.Deck, out map[int]bool) error {
	files, err := w.fileList(d)
	if err != nil {
		return err
	}
	parent, err := w.parent(d)
	if err != nil {
		return err
	}
	prior := map[deck.FileCard]bool{}
	if parent != nil {
		pf, err := w.fileList(parent)
		if err != nil {
			return err
		}
		for _, f := range pf {
			prior[deck.FileCard{Name: f.Name, UUID: f.UUID}] = true
		}
	}
	return w.files(files, prior, out)
}

// fileList applies a delta manifest's F cards to its baseline's. An unheld
// baseline leaves the delta's own cards, the most this store can prove.
func (w *coneWalk) fileList(d *deck.Deck) ([]deck.FileCard, error) {
	if d.B == "" {
		return d.F, nil
	}
	base, err := w.checkin(d.B)
	if err != nil || base == nil {
		return d.F, err
	}
	byName := make(map[string]deck.FileCard, len(base.F)+len(d.F))
	for _, f := range base.F {
		byName[f.Name] = f
	}
	for _, f := range d.F {
		byName[f.Name] = f
	}
	merged := make([]deck.FileCard, 0, len(byName))
	for _, f := range byName {
		merged = append(merged, f)
	}
	return merged, nil
}

func (w *coneWalk) files(files []deck.FileCard, prior map[deck.FileCard]bool, out map[int]bool) error {
	for _, f := range files {
		if err := w.h.ctx.Err(); err != nil {
			return err
		}
		key := deck.FileCard{Name: f.Name, UUID: f.UUID}
		if f.UUID == "" || prior[key] || !w.cone.Covers(f.Name) {
			continue
		}
		if err := w.add(f.UUID, out); err != nil {
			return err
		}
	}
	return nil
}
