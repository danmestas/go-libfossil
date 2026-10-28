package content

import (
	"container/list"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// memoTreeBytes bounds what a Memo keeps of its store's trees
// (draft-fossil-horizon-00 §6.2). A store's trees are read with its graph
// into a treeIndex, which names each blob by a small id and keeps a full
// check-in's file set as its difference from the one read before it:
// fossil-scm.org's check-ins name about 21M F-card entries between them,
// mostly the same few thousand blobs, which the index keeps in a few MiB.
// A store whose index would pass the bound keeps none; its horizons' walks
// read trees as they need them into a treeCache bounded alike, least
// recently used evicted. Either way a miss costs a read, never a wrong
// answer.
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

// plainTreeOf is d's piece. Its names share d's bytes until a cache or an
// index interns them.
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

func (p *plainTree) bytes() int64 {
	return treePieceBytes + int64(len(p.files)+len(p.s)+1)*16
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

// treeIndex is the trees of one store prefix: every held check-in's, and
// the baselines and nodes they reach. It is immutable once a memoStore
// holds it; clone it to extend it.
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

// readTrees returns x extended by the trees of the held check-ins ids
// lists (every one when ids is empty) and the baselines and nodes they
// reach that x lacks, or nil when the result would pass limit bytes. x is
// not changed. Check-ins are read newest first, as fossil stores older
// manifests as deltas of newer ones.
func readTrees(ctx context.Context, q db.Querier, x *treeIndex, ids string, limit int64) (*treeIndex, error) {
	type checkin struct {
		rid  libfossil.FslID
		name string
		size int64
	}
	var checkins []checkin
	query, args := restrictTo(`SELECT b.rid, b.uuid, b.size FROM blob b WHERE b.size>=0
		AND b.rid IN (SELECT objid FROM event WHERE type='ci')%s ORDER BY b.rid DESC`, "b.rid", ids)
	err := referenceRows(ctx, q, query, func(scan func(...any) error) error {
		var c checkin
		err := scan(&c.rid, &c.name, &c.size)
		if x.piece(c.name) == nil {
			checkins = append(checkins, c)
		}
		return err
	}, args...)
	if err != nil {
		return nil, fmt.Errorf("classification memo: trees: %w", err)
	}
	if len(checkins) == 0 {
		return x, nil
	}
	x = x.clone()
	cache := NewCache(treeCacheBytes)
	var pending []string
	keep := func(name string, rid libfossil.FslID, size int64) bool {
		t := &plainTree{rid: rid, size: size}
		if d := readTree(q, cache, rid); d != nil {
			t = plainTreeOf(rid, size, d)
		}
		x.add(name, t)
		if t.b != "" {
			pending = append(pending, t.b)
		}
		pending = append(pending, t.s...)
		return x.bytes <= limit
	}
	for _, c := range checkins {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !keep(c.name, c.rid, c.size) {
			return nil, nil
		}
	}
	absent := map[string]bool{}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if absent[name] || x.piece(name) != nil {
			continue
		}
		var rid libfossil.FslID
		var size int64
		err := q.QueryRow("SELECT rid, size FROM blob WHERE uuid=? AND size>=0", name).Scan(&rid, &size)
		if errors.Is(err, sql.ErrNoRows) {
			absent[name] = true
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("classification memo: tree piece %s: %w", name, err)
		}
		if !keep(name, rid, size) {
			return nil, nil
		}
	}
	return x, nil
}

// expandTree has w expand the piece name names as the view's store holds
// it, and reports whether the view could: from the store's tree index, or,
// for a store too large to index, from what this view or a published one
// read before.
func (v *MemoView) expandTree(w *treeWalk, name string) (bool, error) {
	x := v.store.trees
	if x == nil {
		p := v.tree(name)
		if p == nil {
			return false, nil
		}
		if err := w.charge(p.size); err != nil {
			return true, err
		}
		w.take(p)
		return true, nil
	}
	p := x.piece(name)
	if p == nil {
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

// treeCache is a Memo's least-recently-used tree pieces, by name, for one
// store lineage, kept only for a store too large to index: within a
// lineage a rid at or below a prefix's top that is no phantom there is
// held there, as read, so a piece read under any prefix serves every view
// whose prefix holds its rid. Names are interned, since consecutive trees
// name mostly the same blobs. A lookup refreshes a piece's recency, which
// a View that is never published may do: that changes what is evicted,
// never an answer.
type treeCache struct {
	lineage uint64
	pieces  map[string]*list.Element // of *cachedPiece
	order   list.List                // most recently used first
	names   map[string]internedName
	bytes   int64
}

type cachedPiece struct {
	name  string
	piece *plainTree
}

type internedName struct {
	name string
	refs int
}

// get returns the piece name names under lineage, or nil.
func (c *treeCache) get(lineage uint64, name string) *plainTree {
	if c.lineage != lineage {
		return nil
	}
	e := c.pieces[name]
	if e == nil {
		return nil
	}
	c.order.MoveToFront(e)
	return e.Value.(*cachedPiece).piece
}

// publish keeps pieces, read under lineage, evicting the least recently
// used past the cap. A later lineage replaces the cache; an earlier one is
// stale and is dropped.
func (c *treeCache) publish(lineage uint64, pieces map[string]*plainTree) {
	if lineage < c.lineage || len(pieces) == 0 {
		return
	}
	if lineage > c.lineage || c.pieces == nil {
		*c = treeCache{lineage: lineage, pieces: map[string]*list.Element{}, names: map[string]internedName{}}
	}
	for name, p := range pieces {
		if e := c.pieces[name]; e != nil {
			c.order.MoveToFront(e)
			continue
		}
		kept := &plainTree{rid: p.rid, size: p.size, checkin: p.checkin, b: c.intern(p.b)}
		kept.files = make([]string, len(p.files))
		for i, f := range p.files {
			kept.files[i] = c.intern(f)
		}
		kept.s = make([]string, len(p.s))
		for i, s := range p.s {
			kept.s[i] = c.intern(s)
		}
		c.pieces[name] = c.order.PushFront(&cachedPiece{name: name, piece: kept})
		c.bytes += kept.bytes()
		for c.bytes > memoTreeBytes && c.order.Len() > 0 {
			c.evict(c.order.Back())
		}
	}
}

func (c *treeCache) intern(name string) string {
	if name == "" {
		return ""
	}
	n, ok := c.names[name]
	if !ok {
		// A copy: the name may share the bytes of the whole tree it came from.
		n = internedName{name: strings.Clone(name)}
		c.bytes += treeNameBytes + int64(len(name))
	}
	n.refs++
	c.names[name] = n
	return n.name
}

func (c *treeCache) release(name string) {
	if name == "" {
		return
	}
	n := c.names[name]
	if n.refs <= 0 {
		panic("content.treeCache.release: name not interned")
	}
	if n.refs--; n.refs > 0 {
		c.names[name] = n
		return
	}
	delete(c.names, name)
	c.bytes -= treeNameBytes + int64(len(name))
}

func (c *treeCache) evict(e *list.Element) {
	cp := c.order.Remove(e).(*cachedPiece)
	delete(c.pieces, cp.name)
	c.bytes -= cp.piece.bytes()
	c.release(cp.piece.b)
	for _, f := range cp.piece.files {
		c.release(f)
	}
	for _, s := range cp.piece.s {
		c.release(s)
	}
	if c.bytes < 0 {
		panic("content.treeCache.evict: negative size")
	}
}
