package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
)

// memoCones and memoCuts bound what a Memo keeps beside its store graph:
// the classifications of the cones and the cuts of the horizons most
// recently published. Each costs about one classification's sets.
const (
	memoCones = 8
	memoCuts  = 8
)

// Memo is an origin's classification memo for one open store
// (draft-fossil-horizon-00 §6.2). It keeps the store's cone-independent
// references, read once and extended by the artifacts appended since; and,
// keyed by cone digest and by horizon, the classifications computed over
// them, each keyed by the store prefix it covers and extended the same way.
// It also keeps, bounded, the trees its horizons' walks read, so a horizon
// under another C or rule reads only the trees no earlier walk did.
// It recomputes in full only when an appended artifact carries a name a held
// P, B or S card cited while it was absent, or the store changed other than
// by appending and filling phantoms.
//
// A classification is read from a View, which brings the memo up to the
// store as one querier sees it, and the View is published back once what it
// read is known to be durable. A View that is never published changes
// nothing, so an origin classifying inside a request that rolls back leaves
// no trace. Every value a Memo hands out is immutable; extending copies.
type Memo struct {
	mu      sync.Mutex
	store   *memoStore
	cones   []*memoCone // least recently published first
	cuts    []*memoCut
	slots   []*memoSlots
	trees   treeCache
	lineage uint64 // the last store read in full
}

// NewMemo returns an empty memo.
func NewMemo() *Memo { return &Memo{} }

// memoStore is the store's cone-independent references at one prefix: the
// facts a path classification reads (kinds, F, P, B, S and A references),
// the check-in graph a held set is computed over, and the P, B and S names
// held artifacts cite that the store does not hold. Stores of one lineage
// extend one another; a full read starts a lineage.
type memoStore struct {
	lineage  uint64
	prefix   storePrefix
	refs     *referenceState
	graph    *horizon.Graph
	dangling map[string]bool
}

// storePrefix is what a store held when a memoStore read it: its highest
// rid, that rid's name, its row count, and its phantoms. The store goes on
// from it by appending rows and filling phantoms; anything else (a row
// deleted or renamed, an artifact made a phantom) is another store.
type storePrefix struct {
	top      libfossil.FslID
	topUUID  string
	count    int64
	phantoms map[libfossil.FslID]bool
	// links counts the check-in and plink rows of the rids at or below top:
	// an artifact is crosslinked when it is appended or filled, and these
	// moving otherwise means one was linked late, which an extension by the
	// arrivals would miss.
	links int64
}

// linkCountSQL counts the event and plink rows of the rids at or below ?1,
// leaving out the rids of the JSON array ?2.
const linkCountSQL = `SELECT
	(SELECT count(*) FROM event WHERE type='ci' AND objid<=?1
		AND objid NOT IN (SELECT value FROM json_each(?2))) +
	(SELECT count(*) FROM plink WHERE cid<=?1 AND cid NOT IN (SELECT value FROM json_each(?2)))`

func readPrefix(q db.Querier) (storePrefix, error) {
	p := storePrefix{phantoms: map[libfossil.FslID]bool{}}
	err := q.QueryRow(`SELECT coalesce(max(rid),0), count(*), coalesce((SELECT uuid FROM blob
		WHERE rid=(SELECT max(rid) FROM blob)),'') FROM blob`).Scan(&p.top, &p.count, &p.topUUID)
	if err != nil {
		return storePrefix{}, fmt.Errorf("classification memo: store prefix: %w", err)
	}
	if err := q.QueryRow(linkCountSQL, p.top, "[]").Scan(&p.links); err != nil {
		return storePrefix{}, fmt.Errorf("classification memo: store prefix: %w", err)
	}
	err = referenceRows(context.Background(), q, "SELECT rid FROM phantom", func(scan func(...any) error) error {
		var rid libfossil.FslID
		err := scan(&rid)
		p.phantoms[rid] = true
		return err
	})
	if err != nil {
		return storePrefix{}, fmt.Errorf("classification memo: phantoms: %w", err)
	}
	return p, nil
}

func (p storePrefix) equal(o storePrefix) bool {
	return p.top == o.top && p.topUUID == o.topUUID && p.count == o.count && p.links == o.links &&
		maps.Equal(p.phantoms, o.phantoms)
}

// MemoView is a Memo brought up to one store as one querier sees it. It is
// used by one goroutine; Publish hands what it computed back to the Memo.
type MemoView struct {
	memo  *Memo
	store *memoStore
	read  int
	cones map[string]*memoCone
	cuts  map[cutKey]*memoCut
	slots map[string]*memoSlots
	// trees holds the tree pieces this view's walks read from the store, for
	// the memo's tree cache on Publish; treeReads counts those reads.
	trees     map[string]*treePiece
	treeReads int
}

// View reads q's store prefix and returns the memo extended to it: by the
// artifacts appended since the memo's prefix and the phantoms filled since,
// or read in full when that is the only exact answer.
func (m *Memo) View(ctx context.Context, q db.Querier) (*MemoView, error) {
	if m == nil || ctx == nil || q == nil {
		panic("content.Memo.View: nil memo, context or querier")
	}
	m.mu.Lock()
	base := m.store
	m.mu.Unlock()
	now, err := readPrefix(q)
	if err != nil {
		return nil, err
	}
	v := &MemoView{memo: m, cones: map[string]*memoCone{}, cuts: map[cutKey]*memoCut{},
		slots: map[string]*memoSlots{}, trees: map[string]*treePiece{}}
	if base != nil && base.prefix.equal(now) {
		v.store = base
		return v, nil
	}
	if base != nil {
		s, read, err := base.extend(ctx, q, now)
		if err != nil {
			return nil, err
		}
		if s != nil {
			v.store, v.read = s, read
			return v, nil
		}
	}
	m.mu.Lock()
	m.lineage++
	lineage := m.lineage
	m.mu.Unlock()
	if v.store, err = loadMemoStore(ctx, q, now, lineage); err != nil {
		return nil, err
	}
	v.read = len(v.store.refs.facts.names)
	return v, nil
}

// Read is how many artifacts the view classified to bring the memo up to its
// store: every one on a full read, those appended or filled on an
// extension, none when the memo already covered the store.
func (v *MemoView) Read() int { return v.read }

// Publish makes what v computed the memo's, unless the memo has since been
// brought further. Call it only once the store v read is durable.
func (v *MemoView) Publish() {
	m := v.memo
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store != nil && m.store.prefix.top > v.store.prefix.top {
		return
	}
	m.store = v.store
	for key, c := range v.cones {
		m.cones = slices.DeleteFunc(m.cones, func(o *memoCone) bool { return o.key == key })
		m.cones = append(m.cones, c)
	}
	if over := len(m.cones) - memoCones; over > 0 {
		m.cones = slices.Delete(m.cones, 0, over)
	}
	for key, c := range v.cuts {
		m.cuts = slices.DeleteFunc(m.cuts, func(o *memoCut) bool { return o.key == key })
		m.cuts = append(m.cuts, c)
	}
	if over := len(m.cuts) - memoCuts; over > 0 {
		m.cuts = slices.Delete(m.cuts, 0, over)
	}
	for key, c := range v.slots {
		m.slots = slices.DeleteFunc(m.slots, func(o *memoSlots) bool { return o.key == key })
		m.slots = append(m.slots, c)
	}
	if over := len(m.slots) - memoCones; over > 0 {
		m.slots = slices.Delete(m.slots, 0, over)
	}
	m.trees.publish(v.store.lineage, v.trees)
}

func loadMemoStore(ctx context.Context, q db.Querier, prefix storePrefix, lineage uint64) (*memoStore, error) {
	refs, _, err := loadReferenceState(ctx, q, defaultReferenceLimits.refs, true)
	if err != nil {
		return nil, err
	}
	graph, err := horizon.LoadGraph(ctx, q)
	if err != nil {
		return nil, err
	}
	dangling, err := danglingNames(ctx, q, refs.facts.records, "")
	if err != nil {
		return nil, err
	}
	return &memoStore{lineage: lineage, prefix: prefix, refs: refs, graph: graph, dangling: dangling}, nil
}

// extend returns s brought up to the store now describes, and how many
// artifacts it classified, or nil when only a full read is exact.
func (s *memoStore) extend(ctx context.Context, q db.Querier, now storePrefix) (*memoStore, int, error) {
	if now.top < s.prefix.top || now.count < s.prefix.count {
		return nil, 0, nil
	}
	var uuid string
	var count int64
	err := q.QueryRow("SELECT coalesce((SELECT uuid FROM blob WHERE rid=?1),''), (SELECT count(*) FROM blob WHERE rid<=?1)",
		s.prefix.top).Scan(&uuid, &count)
	if err != nil {
		return nil, 0, fmt.Errorf("classification memo: store prefix: %w", err)
	}
	if uuid != s.prefix.topUUID || count != s.prefix.count {
		return nil, 0, nil
	}
	var filled []libfossil.FslID
	for rid := range s.prefix.phantoms {
		if !now.phantoms[rid] {
			filled = append(filled, rid)
		}
	}
	for rid := range now.phantoms {
		if rid <= s.prefix.top && !s.prefix.phantoms[rid] {
			return nil, 0, nil // made a phantom: not an append
		}
	}
	settled, err := s.settled(q, filled)
	if err != nil || !settled {
		return nil, 0, err
	}
	names := maps.Clone(s.refs.facts.names)
	fresh, cited, err := s.arrivals(q, filled, names)
	if err != nil || cited {
		return nil, 0, err
	}
	refs, exact, err := extendReferenceState(ctx, q, s.refs, fresh, names)
	if err != nil || !exact {
		return nil, 0, err
	}
	graph, err := s.graph.Extend(ctx, q, fresh)
	if err != nil {
		return nil, 0, err
	}
	ids, err := ridsJSON(fresh)
	if err != nil {
		return nil, 0, err
	}
	added, err := danglingNames(ctx, q, refs.facts.records[len(s.refs.facts.records):], ids)
	if err != nil {
		return nil, 0, err
	}
	dangling := maps.Clone(s.dangling)
	maps.Copy(dangling, added)
	return &memoStore{lineage: s.lineage, prefix: now, refs: refs, graph: graph, dangling: dangling}, len(fresh), nil
}

// settled reports whether the rids at or below s's prefix are as s read
// them but for the phantoms filled: none was crosslinked late, and none was
// stored as a delta of a filled phantom, which leaves it unreadable until the
// fill and readable after.
func (s *memoStore) settled(q db.Querier, filled []libfossil.FslID) (bool, error) {
	ids, err := ridsJSON(filled)
	if err != nil {
		return false, err
	}
	if ids == "" {
		ids = "[]"
	}
	var links int64
	var based bool
	err = q.QueryRow(linkCountSQL+`, EXISTS(SELECT 1 FROM delta WHERE srcid IN (SELECT value FROM json_each(?2)))`,
		s.prefix.top, ids).Scan(&links, &based)
	if err != nil {
		return false, fmt.Errorf("classification memo: store prefix: %w", err)
	}
	return links == s.prefix.links && !based, nil
}

// arrivals lists the rids appended past s's prefix and the phantoms filled
// since, ascending, adding the appended ones' names to names, and reports whether one now held carries a name a held
// P, B or S card cited while it was absent (§6.2): such an arrival can bring
// old check-ins into a held set and old trees into a reach.
func (s *memoStore) arrivals(
	q db.Querier, filled []libfossil.FslID, names map[libfossil.FslID]string,
) ([]libfossil.FslID, bool, error) {
	ids, err := ridsJSON(filled)
	if err != nil {
		return nil, false, err
	}
	if ids == "" {
		ids = "[]"
	}
	var fresh []libfossil.FslID
	cited := false
	err = referenceRows(context.Background(), q, `SELECT rid, uuid, size>=0 FROM blob
		WHERE rid>?1 OR rid IN (SELECT value FROM json_each(?2)) ORDER BY rid`,
		func(scan func(...any) error) error {
			var rid libfossil.FslID
			var uuid string
			var held bool
			err := scan(&rid, &uuid, &held)
			fresh = append(fresh, rid)
			names[rid] = uuid
			cited = cited || held && s.dangling[uuid]
			return err
		}, s.prefix.top, ids)
	if err != nil {
		return nil, false, fmt.Errorf("classification memo: arrivals: %w", err)
	}
	return fresh, cited, nil
}

// danglingNames returns the names P, B and S cards of records cite, and the
// parents and baselines plink names for the check-ins ids lists (every one
// when ids is empty), that the store does not hold.
func danglingNames(ctx context.Context, q db.Querier, records []coneReferenceRecord, ids string) (map[string]bool, error) {
	var cited []string
	for _, record := range records {
		d := record.deck
		cited = append(cited, d.P...)
		if d.B != "" {
			cited = append(cited, d.B)
		}
		for _, sc := range d.S {
			cited = append(cited, sc.UUID)
		}
	}
	out := map[string]bool{}
	add := func(scan func(...any) error) error {
		var uuid string
		err := scan(&uuid)
		out[uuid] = true
		return err
	}
	if len(cited) > 0 {
		names, err := jsonStrings(cited)
		if err != nil {
			return nil, err
		}
		err = referenceRows(ctx, q, `SELECT DISTINCT value FROM json_each(?1)
			WHERE NOT EXISTS(SELECT 1 FROM blob WHERE uuid=value AND size>=0)`, add, names)
		if err != nil {
			return nil, err
		}
	}
	for _, column := range []string{"pid", "baseid"} {
		query, args := restrictTo("SELECT b.uuid FROM plink JOIN blob b ON b.rid=plink."+column+
			" WHERE b.size<0%s", "plink.cid", ids)
		if err := referenceRows(ctx, q, query, add, args...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// jsonStrings spells names as a JSON array for json_each.
func jsonStrings(names []string) (string, error) {
	data, err := json.Marshal(names)
	if err != nil {
		return "", fmt.Errorf("classification memo: %w", err)
	}
	return string(data), nil
}

// References returns the path classification of c (see LoadConeReferences)
// over the view's store: the memo's classification of c extended by what
// the store gained since, or one computed over the store graph.
func (v *MemoView) References(ctx context.Context, c *cone.Cone) (*ConeReferences, error) {
	if v == nil || ctx == nil {
		panic("content.MemoView.References: nil view or context")
	}
	facts := v.store.refs.facts
	if c == nil {
		return facts.Classify(ctx, nil)
	}
	key := c.Digest()
	prev := v.cones[key]
	if prev == nil {
		prev = v.memo.lookupCone(key)
	}
	if prev != nil && prev.lineage == v.store.lineage && prev.mark == markAt(facts, prev.mark.visited) {
		if prev.refs.facts == facts {
			return prev.refs, nil
		}
		// No list grew, but the store did: an artifact appended or filled
		// that is no record still gains a name, which the classification
		// reads by rid. Its membership is never written once handed out.
		same := *prev.refs
		same.names, same.held, same.nodes, same.facts = facts.names, facts.held, facts.nodes, facts
		v.cones[key] = &memoCone{key: key, lineage: v.store.lineage, mark: prev.mark, refs: &same}
		return &same, nil
	}
	var p *ConeReferences
	from := factsMark{}
	if prev != nil && prev.lineage == v.store.lineage {
		p, from = facts.references(prev.refs), prev.mark
	} else {
		p = facts.references(nil)
	}
	through, err := facts.classifyFrom(ctx, c, p, from)
	if err != nil {
		return nil, err
	}
	v.cones[key] = &memoCone{key: key, lineage: v.store.lineage, mark: through, refs: p}
	return p, nil
}

// markAt is facts' mark with visited, which a mark compares on too.
func markAt(facts *ConeReferenceFacts, visited int) factsMark {
	m := facts.mark()
	m.visited = visited
	return m
}

// memoCone is a cone's classification over a store lineage, through mark.
type memoCone struct {
	key     string
	lineage uint64
	mark    factsMark
	refs    *ConeReferences
}

func (m *Memo) lookupCone(key string) *memoCone {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cones {
		if c.key == key {
			return c
		}
	}
	return nil
}

// cutKey names a horizon's cut, which neither its depth nor the request's
// path members change.
type cutKey struct {
	hash string
	rule cone.HorizonRule
}

// memoCut is a horizon's cut over a store lineage: the held set it walked
// and what the walk produced, with the tree bytes it was charged.
type memoCut struct {
	key       cutKey
	lineage   uint64
	placed    bool
	cut       horizonCut
	inK       map[libfossil.FslID]bool
	treeBytes int64
}

func (m *Memo) lookupCut(key cutKey) *memoCut {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cuts {
		if c.key == key {
			return c
		}
	}
	return nil
}

// cut returns h's cut over the view's store (see horizonTrees): K over the
// store graph, and the walk of K's trees extended from the memo's walk by
// the check-ins K gained, which reads only their trees. A walk is a
// reachability walk with one visited set, so walking the new roots into
// the old sets is walking every root.
func (v *MemoView) cut(ctx context.Context, q db.Querier, h cone.Horizon) (*memoCut, error) {
	key := cutKey{hash: h.Hash(), rule: h.Rule()}
	prev := v.cuts[key]
	if prev == nil {
		prev = v.memo.lookupCut(key)
	}
	if prev != nil && prev.lineage != v.store.lineage {
		prev = nil
	}
	var c libfossil.FslID
	err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", h.Hash()).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return v.keepCut(&memoCut{key: key, lineage: v.store.lineage}), nil
	}
	if err != nil {
		return nil, fmt.Errorf("horizon: C: %w", err)
	}
	k, err := v.store.graph.HeldSet(ctx, c, h.Rule())
	if errors.Is(err, horizon.ErrNotHeld) {
		return v.keepCut(&memoCut{key: key, lineage: v.store.lineage}), nil
	}
	if err != nil {
		return nil, err
	}
	out := &memoCut{key: key, lineage: v.store.lineage, placed: true, inK: make(map[libfossil.FslID]bool, len(k))}
	for _, rid := range k {
		out.inK[rid] = true
	}
	if prev != nil && prev.placed {
		for rid := range prev.inK {
			if !out.inK[rid] {
				prev = nil // K only grows within a lineage; walk it afresh if not
				break
			}
		}
	}
	var roots []libfossil.FslID
	for _, rid := range k {
		if prev == nil || !prev.inK[rid] {
			roots = append(roots, rid)
		}
	}
	if prev != nil && prev.placed && len(roots) == 0 && len(k) == len(prev.inK) {
		return prev, nil
	}
	walk := newTreeWalk(ctx, q, k, defaultReferenceLimits, &ReferenceWork{})
	walk.memo = v
	if prev != nil && prev.placed {
		walk.files, walk.visited = maps.Clone(prev.cut.above), maps.Clone(prev.cut.reach)
		walk.work.TreeBytes = prev.treeBytes
	}
	above, err := walk.named(roots)
	if err != nil {
		return nil, err
	}
	out.cut = horizonCut{k: k, above: above, reach: walk.visited}
	out.treeBytes = walk.work.TreeBytes
	return v.keepCut(out), nil
}

// tree returns the piece name names as the view's store holds it, read
// before by this view or by a published one, or nil. A piece read under a
// later prefix serves only when its rid is held at the view's.
func (v *MemoView) tree(name string) *treePiece {
	p := v.trees[name]
	if p == nil {
		m := v.memo
		m.mu.Lock()
		p = m.trees.get(v.store.lineage, name)
		m.mu.Unlock()
	}
	if p == nil || p.rid > v.store.prefix.top || v.store.prefix.phantoms[p.rid] {
		return nil
	}
	return p
}

func (v *MemoView) keepCut(c *memoCut) *memoCut {
	v.cuts[c.key] = c
	return c
}

// HorizonReach is HorizonReach over the view's store.
func (v *MemoView) HorizonReach(
	ctx context.Context, q db.Querier, h cone.Horizon,
) (k []libfossil.FslID, reach map[string]bool, placed bool, err error) {
	if v == nil || ctx == nil || q == nil {
		panic("content.MemoView.HorizonReach: nil view, context or querier")
	}
	c, err := v.cut(ctx, q, h)
	if err != nil || !c.placed {
		return nil, nil, false, err
	}
	return c.cut.k, c.cut.reach, true, nil
}

// HorizonJoinedAfter is HorizonJoinedAfter over the view's store graph.
func (v *MemoView) HorizonJoinedAfter(ctx context.Context, q db.Querier, h cone.Horizon, cursor int64) (bool, error) {
	if v == nil || ctx == nil || q == nil {
		panic("content.MemoView.HorizonJoinedAfter: nil view, context or querier")
	}
	return joinedAfter(ctx, q, v.store.graph, h, cursor)
}

// HorizonBelow is p.HorizonBelow over the view's store, p a classification
// the view returned.
func (v *MemoView) HorizonBelow(
	ctx context.Context, q db.Querier, p *ConeReferences, c *cone.Cone,
) (func(libfossil.FslID) bool, error) {
	if v == nil || ctx == nil || q == nil || p == nil || c == nil {
		panic("content.MemoView.HorizonBelow: nil view, context, querier, references or cone")
	}
	h, ok := c.Horizon()
	if !ok {
		return nil, nil
	}
	cut, err := v.cut(ctx, q, h)
	if err != nil || !cut.placed {
		return nil, err
	}
	return func(rid libfossil.FslID) bool { return p.below(cut.cut.above, rid) }, nil
}

// memoSlots is a cone's slot walk over a store lineage (see SlotKeep).
type memoSlots struct {
	key     string
	lineage uint64
	keep    *SlotKeep
}

func (m *Memo) lookupSlots(key string) *memoSlots {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.slots {
		if s.key == key {
			return s
		}
	}
	return nil
}

// SlotKeep is p.SlotKeep over the view's store, p a classification the view
// returned: the memo's walk of c extended by the trees the store gained
// since, which reads only theirs (§6.2), or a fresh walk when the gain is
// not an append of later roots.
func (v *MemoView) SlotKeep(
	ctx context.Context, p *ConeReferences, c *cone.Cone, reach map[string]bool,
) (*SlotKeep, error) {
	if v == nil || ctx == nil || p == nil || c == nil {
		panic("content.MemoView.SlotKeep: nil view, context, references or cone")
	}
	key := c.Digest()
	prev := v.slots[key]
	if prev == nil {
		prev = v.memo.lookupSlots(key)
	}
	var from *SlotKeep
	if prev != nil && prev.lineage == v.store.lineage {
		from = prev.keep
	}
	k, err := extendSlotKeep(ctx, p.facts, c, reach, from)
	if err != nil {
		return nil, err
	}
	v.slots[key] = &memoSlots{key: key, lineage: v.store.lineage, keep: k}
	return k, nil
}
