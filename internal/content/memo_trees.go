package content

import (
	"cmp"
	"encoding/binary"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// memoTreeBytes bounds what a Memo keeps of the trees its horizons' walks
// read (draft-fossil-horizon-00 §6.2), so a second horizon over the store
// reads only the trees the first did not. The trees are kept as a
// treeIndex, which names each blob by a small id and keeps a full
// check-in's file set as its difference from the one kept before it:
// fossil-scm.org's check-ins name about 21M F-card entries between them,
// mostly the same few thousand blobs, and the index keeps every one of
// its trees in about 11MiB. Past the bound the index starts again; a miss
// costs a read, never a wrong answer.
const memoTreeBytes = 64 << 20

// treePieceBytes and treeNameBytes are what a kept piece and an interned
// name are charged beyond their references and bytes: list element, map
// entry, slice entry and struct headers.
const (
	treePieceBytes = 160
	treeNameBytes  = 64
)

// treeDecoded is how many decoded file sets a treeDecoder keeps, and
// treeChain how many set differences a piece may lie from a piece kept
// whole, so a walk that meets a chain's pieces in either order decodes
// each once.
const (
	treeDecoded = 16
	treeChain   = treeDecoded - 1
)

// plainTree is what a horizon's tree walk reads of one held check-in
// manifest, baseline or sub-manifest node: its own F-card targets and the
// pieces it reaches through its B and S cards. A delta manifest's entry
// holds only its own F cards; the baseline it shares with its siblings is
// an entry of its own, kept once.
type plainTree struct {
	rid     libfossil.FslID
	size    int64
	checkin bool
	files   []string
	b       string
	s       []string
}

// plainTreeOf is d's piece. Its names share d's bytes until an index
// interns them.
func plainTreeOf(rid libfossil.FslID, size int64, d *deck.Deck) *plainTree {
	p := &plainTree{rid: rid, size: size, checkin: d.Type == deck.Checkin, b: d.B,
		files: make([]string, 0, len(d.F))}
	for _, f := range d.F {
		if f.UUID != "" {
			p.files = append(p.files, f.UUID)
		}
	}
	for _, sc := range d.S {
		p.s = append(p.s, sc.UUID)
	}
	return p
}

// readTree returns the check-in or sub-manifest rid holds, or nil when its
// content cannot be read or is no tree: such a piece names nothing, and
// the classification after it arrives counts the blobs it names. Parse
// checks the Z card.
func readTree(q db.Querier, cache *Cache, rid libfossil.FslID) *deck.Deck {
	data, err := cache.Expand(q, rid)
	if err != nil {
		return nil
	}
	d, err := deck.Parse(data)
	if err != nil || d.Type != deck.Checkin && d.Type != deck.Submanifest {
		return nil
	}
	return d
}

// treeNames interns names as ids; id 0 is no name.
type treeNames struct {
	names []string
	ids   map[string]uint32
}

func (n *treeNames) intern(name string) (id uint32, added bool) {
	if name == "" {
		return 0, false
	}
	if id, ok := n.ids[name]; ok {
		return id, false
	}
	if len(n.names) >= math.MaxUint32 {
		panic("content.treeNames.intern: ids exhausted")
	}
	id = uint32(len(n.names))
	// A copy: the name may share the bytes of the whole tree it came from.
	n.names = append(n.names, strings.Clone(name))
	n.ids[n.names[id]] = id
	return id, true
}

func (n *treeNames) name(id uint32) string { return n.names[id] }

// treePiece is a plainTree in a treeIndex: names as ids, and its files as
// sorted ids, delta-varint encoded (appendTreeIDs). A full check-in's
// files may instead be the symmetric difference of its set and base's,
// depth differences from a piece kept whole.
type treePiece struct {
	rid   libfossil.FslID
	size  int64
	b     uint32
	s     []uint32
	files []byte
	base  *treePiece
	depth int
	full  bool
}

// treeIndex is tree pieces of one store lineage, by name. It is immutable
// once a Memo holds it; clone it to extend it.
type treeIndex struct {
	names  treeNames
	pieces map[uint32]*treePiece
	// last is the full check-in added last and lastIDs its file set: the
	// next full check-in is kept as its difference from them.
	last    *treePiece
	lastIDs []uint32
	bytes   int64
}

func newTreeIndex() *treeIndex {
	return &treeIndex{names: treeNames{names: []string{""}, ids: map[string]uint32{}},
		pieces: map[uint32]*treePiece{}}
}

// clone returns a copy that x's readers do not see grow.
func (x *treeIndex) clone() *treeIndex {
	return &treeIndex{
		names:  treeNames{names: slices.Clip(x.names.names), ids: maps.Clone(x.names.ids)},
		pieces: maps.Clone(x.pieces), last: x.last, lastIDs: x.lastIDs, bytes: x.bytes,
	}
}

// piece returns the piece name names, or nil.
func (x *treeIndex) piece(name string) *treePiece {
	id, ok := x.names.ids[name]
	if !ok {
		return nil
	}
	return x.pieces[id]
}

func (x *treeIndex) intern(name string) uint32 {
	id, added := x.names.intern(name)
	if added {
		x.bytes += treeNameBytes + int64(len(name))
	}
	return id
}

// add keeps t as the piece name names, which x must not hold.
func (x *treeIndex) add(name string, t *plainTree) {
	id := x.intern(name)
	if x.pieces[id] != nil {
		panic("content.treeIndex.add: piece already held")
	}
	p := &treePiece{rid: t.rid, size: t.size, b: x.intern(t.b), full: t.checkin && t.b == ""}
	if len(t.s) > 0 {
		p.s = make([]uint32, len(t.s))
		for i, s := range t.s {
			p.s[i] = x.intern(s)
		}
	}
	own := make([]uint32, len(t.files))
	for i, f := range t.files {
		own[i] = x.intern(f)
	}
	slices.Sort(own)
	own = slices.Compact(own)
	p.files = appendTreeIDs(nil, own)
	if p.full {
		if x.last != nil && x.last.depth < treeChain {
			diff := appendTreeIDs(nil, xorTreeIDs(x.lastIDs, own))
			if len(diff) < len(p.files) {
				p.files, p.base, p.depth = diff, x.last, x.last.depth+1
			}
		}
		x.last, x.lastIDs = p, own
	}
	p.files = slices.Clip(p.files)
	x.pieces[id] = p
	x.bytes += treePieceBytes + int64(len(p.files)+4*len(p.s))
}

// appendTreeIDs appends ids, ascending and above zero, to dst as the
// uvarints of their gaps.
func appendTreeIDs(dst []byte, ids []uint32) []byte {
	var prev uint32
	for _, id := range ids {
		if id <= prev {
			panic("content.appendTreeIDs: ids not ascending")
		}
		dst = binary.AppendUvarint(dst, uint64(id-prev))
		prev = id
	}
	return dst
}

// decodeTreeIDs appends to dst the ids appendTreeIDs encoded as enc.
func decodeTreeIDs(dst []uint32, enc []byte) []uint32 {
	var prev uint64
	for len(enc) > 0 {
		gap, n := binary.Uvarint(enc)
		if n <= 0 || gap == 0 || prev+gap > math.MaxUint32 {
			panic("content.decodeTreeIDs: malformed ids")
		}
		prev += gap
		dst = append(dst, uint32(prev))
		enc = enc[n:]
	}
	return dst
}

// xorTreeIDs returns the ids in exactly one of a and b, both ascending.
func xorTreeIDs(a, b []uint32) []uint32 {
	out := make([]uint32, 0, max(len(a), len(b)))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}

// treeDecoder decodes pieces' file sets, keeping the full check-ins' most
// recently decoded, which a chain of differences decodes through. It is
// used by one goroutine.
type treeDecoder struct {
	recent [treeDecoded]decodedTree // most recently used first
}

type decodedTree struct {
	piece *treePiece
	ids   []uint32
}

// files returns p's file ids, ascending. The caller must not modify them.
func (d *treeDecoder) files(p *treePiece) []uint32 {
	for i, e := range d.recent {
		if e.piece == p {
			copy(d.recent[1:i+1], d.recent[:i])
			d.recent[0] = e
			return e.ids
		}
	}
	ids := decodeTreeIDs(nil, p.files)
	if p.base != nil {
		ids = xorTreeIDs(d.files(p.base), ids)
	}
	if p.full {
		copy(d.recent[1:], d.recent[:treeDecoded-1])
		d.recent[0] = decodedTree{piece: p, ids: ids}
	}
	return ids
}

// publishTrees keeps pieces, which a view of store lineage read, in the
// memo's trees: a later lineage replaces them, an earlier one is stale and
// is dropped. Pieces are added newest first, as a walk reads them. Past
// the memo's tree limit the trees start again from the piece that passed
// it. The caller holds m.mu.
func (m *Memo) publishTrees(lineage uint64, pieces map[string]*plainTree) {
	if lineage < m.treeLineage {
		return
	}
	if lineage > m.treeLineage {
		m.trees, m.treeLineage = nil, lineage
	}
	if len(pieces) == 0 {
		return
	}
	names := slices.SortedFunc(maps.Keys(pieces), func(a, b string) int {
		return cmp.Compare(pieces[b].rid, pieces[a].rid)
	})
	x := newTreeIndex()
	if m.trees != nil {
		x = m.trees.clone()
	}
	for _, name := range names {
		if x.piece(name) != nil {
			continue
		}
		x.add(name, pieces[name])
		if x.bytes > m.treeLimit {
			x = newTreeIndex()
			if x.add(name, pieces[name]); x.bytes > m.treeLimit {
				x = newTreeIndex()
			}
		}
	}
	m.trees = x
}

// expandTree has w expand the piece name names as the view's store holds
// it, and reports whether the view could: read before by this view, or by
// a published one. A piece read under a later prefix serves only when its
// rid is held at the view's.
func (v *MemoView) expandTree(w *treeWalk, name string) (bool, error) {
	if p := v.trees[name]; p != nil {
		if err := w.charge(p.size); err != nil {
			return true, err
		}
		w.take(p)
		return true, nil
	}
	m := v.memo
	m.mu.Lock()
	x, lineage := m.trees, m.treeLineage
	m.mu.Unlock()
	if x == nil || lineage != v.store.lineage {
		return false, nil
	}
	p := x.piece(name)
	if p == nil || p.rid > v.store.prefix.top || v.store.prefix.phantoms[p.rid] {
		return false, nil
	}
	if err := w.charge(p.size); err != nil {
		return true, err
	}
	for _, id := range v.decoder.files(p) {
		w.files[x.names.name(id)] = true
	}
	w.push(x.names.name(p.b))
	for _, s := range p.s {
		w.push(x.names.name(s))
	}
	return true, nil
}
