package content

import (
	"container/list"
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// memoTreeBytes caps what a Memo keeps of the trees its horizons' walks read
// (draft-fossil-horizon-00 §6.2), so a second horizon over the store reads
// only the trees the first did not. Uncapped it would grow with the store's
// whole history: fossil-scm.org's check-ins name about 21M F-card entries
// between them, over 300MiB at a string header each before the names
// themselves. The cap holds about 4M entries, a few thousand full trees of a
// repository that size, which covers the held sets of the horizons an
// origin is asked for at once; a miss costs a read, never a wrong answer.
const memoTreeBytes = 64 << 20

// treePieceBytes and treeNameBytes are what a cached piece and an interned
// name are charged beyond their references and bytes: list element, map
// entry and struct headers.
const (
	treePieceBytes = 160
	treeNameBytes  = 64
)

// treePiece is what a horizon's tree walk reads of one held check-in
// manifest, baseline or sub-manifest node: its own F-card targets and the
// pieces it reaches through its B and S cards. A delta manifest's entry
// holds only its own F cards; the baseline it shares with its siblings is
// an entry of its own, kept once.
type treePiece struct {
	rid   libfossil.FslID
	size  int64
	files []string
	b     string
	s     []string
}

// pieceOf is d's piece. Its names share d's bytes until a cache interns
// them.
func pieceOf(rid libfossil.FslID, size int64, d *deck.Deck) *treePiece {
	p := &treePiece{rid: rid, size: size, b: d.B, files: make([]string, 0, len(d.F))}
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

func (p *treePiece) bytes() int64 {
	return treePieceBytes + int64(len(p.files)+len(p.s)+1)*16
}

// treeCache is a Memo's least-recently-used tree pieces, by name, for one
// store lineage: within a lineage a rid at or below a prefix's top that is
// no phantom there is held there, as read, so a piece read under any
// prefix serves every view whose prefix holds its rid. Names are interned,
// since consecutive trees name mostly the same blobs. A lookup refreshes a
// piece's recency, which a View that is never published may do: that
// changes what is evicted, never an answer.
type treeCache struct {
	lineage uint64
	pieces  map[string]*list.Element // of *cachedPiece
	order   list.List                // most recently used first
	names   map[string]internedName
	bytes   int64
}

type cachedPiece struct {
	name  string
	piece *treePiece
}

type internedName struct {
	name string
	refs int
}

// get returns the piece name names under lineage, or nil.
func (c *treeCache) get(lineage uint64, name string) *treePiece {
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
func (c *treeCache) publish(lineage uint64, pieces map[string]*treePiece) {
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
		kept := &treePiece{rid: p.rid, size: p.size, b: c.intern(p.b)}
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
