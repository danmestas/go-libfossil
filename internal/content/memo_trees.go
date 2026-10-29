package content

import (
	"cmp"
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

// A Memo keeps every tree its horizons' walks read (draft-fossil-horizon-00
// §6.2), so no tree is read twice. The trees are kept as a treeIndex, which
// names each blob by a small id and keeps a full check-in's file set as its
// difference from the one kept before it: fossil-scm.org's check-ins name
// about 21M F-card entries between them, mostly the same few thousand
// blobs, and the index keeps every one of its trees in about 11MiB. A
// piece costs a few bytes per F card its manifest spells in about 70, so
// the index grows with the store, like the reference graph beside it.
//
// A piece is what its name's bytes say, the same in every store that
// holds it, so the index outlives a full read of the store. It serves a
// view only where a fresh read would give the same piece: the view's store
// holds the name at the piece's rid and can expand it (treeExpandable).

// memoTreeNames bounds the names a tree index interns, in place of a bound
// on its bytes: one store's trees name at most its artifacts, which a
// classification bounds by coneReferenceNodes, so the bound is passed only
// by an index that outlived stores which renamed most of what it read.
// A publish that would pass it starts the index again from the view's own
// pieces, and keeps none when even those pass it.
const memoTreeNames = coneReferenceNodes

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
	// uint64: on 32-bit targets (TinyGo wasm) MaxUint32 overflows int.
	if uint64(len(n.names)) >= math.MaxUint32 {
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
	// last is the full check-in added last and lastIDs its file set, which
	// bytes counts: the next full check-in is kept as its difference from
	// them.
	last    *treePiece
	lastIDs []uint32
	bytes   int64
}

func newTreeIndex() *treeIndex {
	return &treeIndex{names: treeNames{names: []string{""}, ids: map[string]uint32{}},
		pieces: map[uint32]*treePiece{}}
}

// clone returns a copy that x's readers do not see grow. A publish that
// read trees clones once, under the memo's lock: for an index of
// fossil-scm.org's whole tree history (45k names, 21k pieces, about 11MiB)
// that costs about 0.5ms (BenchmarkTreeIndexClone), and at the bound a few
// ms, against the tens of ms the walk spent reading the trees it adds.
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
		x.bytes += 4 * int64(len(own)-len(x.lastIDs))
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
		if n <= 0 || gap == 0 || gap > math.MaxUint32-prev {
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

// publishTrees keeps pieces, which a view read, in the memo's trees, past
// limit names (memoTreeNames) starting again. The caller holds m.mu.
func (m *Memo) publishTrees(pieces map[string]*plainTree, limit int) {
	if len(pieces) == 0 {
		return
	}
	names := slices.SortedFunc(maps.Keys(pieces), func(a, b string) int {
		return cmp.Compare(pieces[b].rid, pieces[a].rid)
	})
	if m.trees != nil {
		if x := m.trees.clone(); x.addPieces(names, pieces, limit) {
			m.trees = x
			return
		}
	}
	m.trees = nil
	if x := newTreeIndex(); x.addPieces(names, pieces, limit) {
		m.trees = x
	}
}

// addPieces adds pieces, by names in order (newest first, as a walk reads
// them), and reports whether x still holds no more than limit names,
// stopping at the first piece that passes it. A piece x holds at another
// rid, which a renumbered store holds, takes the new rid.
func (x *treeIndex) addPieces(names []string, pieces map[string]*plainTree, limit int) bool {
	for _, name := range names {
		t := pieces[name]
		if held := x.piece(name); held != nil {
			if held.rid != t.rid {
				moved := *held
				moved.rid = t.rid
				x.pieces[x.names.ids[name]] = &moved
			}
			continue
		}
		x.add(name, t)
		if len(x.names.names) > limit {
			return false
		}
	}
	return true
}

// treeExpandable reports whether the view's store holds name at rid and can
// expand it, as a fresh read of the piece would: rid's chain of delta
// sources ends at a blob held whole, not at a phantom, at a rid the store
// holds no row for, or in a loop. Within a lineage a rid's answer never
// changes, as a store goes on only by appending rows and filling phantoms
// nothing is a delta of (memoStore.settled), so the memo keeps the answers
// of a lineage (Memo.expands) and a chain is walked only to the first rid
// with one. Each delta step is read once per lineage, for the trees looked
// up; the store's phantoms, however many, are never listed.
func (v *MemoView) treeExpandable(ctx context.Context, q db.Querier, name string, rid libfossil.FslID) (bool, error) {
	if v.store.refs.facts.names[rid] != name {
		return false, nil
	}
	known := v.publishedExpands()
	seen := map[libfossil.FslID]bool{}
	settle := func(ok bool) (bool, error) {
		for r := range seen {
			v.expands[r] = ok
		}
		return ok, nil
	}
	// A chain is no longer than the store has rows.
	for r := rid; len(seen) <= len(v.store.refs.facts.names); {
		if ok, hit := v.expands[r]; hit {
			return settle(ok)
		}
		if ok, hit := known[r]; hit {
			return settle(ok)
		}
		if seen[r] {
			return settle(false) // a loop, which no expansion ends
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		seen[r] = true
		var size int64
		var src sql.NullInt64
		err := q.QueryRow(`SELECT b.size, d.srcid FROM blob b LEFT JOIN delta d ON d.rid=b.rid
			WHERE b.rid=?`, r).Scan(&size, &src)
		if errors.Is(err, sql.ErrNoRows) || err == nil && size < 0 {
			return settle(false)
		}
		if err != nil {
			return false, fmt.Errorf("classification memo: delta chain of rid=%d: %w", rid, err)
		}
		if !src.Valid {
			return settle(true)
		}
		r = libfossil.FslID(src.Int64)
	}
	panic("content.MemoView.treeExpandable: delta chain longer than the store")
}

// publishedExpands returns the expandability answers the memo keeps for the
// view's lineage, read once per view, or nil.
func (v *MemoView) publishedExpands() map[libfossil.FslID]bool {
	if !v.expandsRead {
		m := v.memo
		m.mu.Lock()
		if m.expandsLineage == v.store.lineage {
			v.expandsKnown = m.expands
		}
		m.mu.Unlock()
		v.expandsRead = true
	}
	return v.expandsKnown
}

// publishExpands keeps the expandability answers a view of store lineage
// found: a later lineage's replace them, an earlier one's are dropped. The
// kept map is replaced, never changed. The caller holds m.mu.
func (m *Memo) publishExpands(lineage uint64, found map[libfossil.FslID]bool) {
	if len(found) == 0 || lineage < m.expandsLineage {
		return
	}
	if lineage > m.expandsLineage {
		m.expands, m.expandsLineage = maps.Clone(found), lineage
		return
	}
	merged := maps.Clone(m.expands)
	if merged == nil {
		merged = make(map[libfossil.FslID]bool, len(found))
	}
	maps.Copy(merged, found)
	m.expands = merged
}

// expandTree has w expand the piece name names as the view's store holds
// it, and reports whether the view could: read before by this view, or by
// a published one. A published piece serves only where a fresh read would
// give it (treeExpandable): a piece read under a later prefix, in a store
// that numbered its rids otherwise, or whose content the store can no
// longer expand, is read again.
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
	x := m.trees
	m.mu.Unlock()
	if x == nil {
		return false, nil
	}
	p := x.piece(name)
	if p == nil {
		return false, nil
	}
	if ok, err := v.treeExpandable(w.ctx, w.q, name, p.rid); err != nil || !ok {
		return false, err
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
