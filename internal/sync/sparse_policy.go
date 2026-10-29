package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// SettingPartialMirror is the boolean repository setting that lets a sparse
// store serve clones its cone does not cover (§5.3).
const SettingPartialMirror = "partial-mirror"

// servesPartialMirror reads the per-call override, then the repository
// setting as fossil reads a boolean one, absent meaning off.
func (h *handler) servesPartialMirror() (bool, error) {
	if h.partialMirror {
		return true, nil
	}
	var value string
	err := h.repo.DB().QueryRow("SELECT value FROM config WHERE name=?", SettingPartialMirror).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("handler: read %s: %w", SettingPartialMirror, err)
	}
	return repo.SettingBoolean(value, false), nil
}

// sparseKeep is request-local. Unknown non-file artifacts remain dense; a file
// shared by covered and uncovered names is kept under either name. A clone
// walks its round (walk); a pull's inventory classifies the store (refs). A
// content horizon (draft-fossil-horizon-00 §3.2, §4.5) also drops the blobs
// below it and every cluster: the keep set is the intersection of the two.
type sparseKeep struct {
	refs     *content.ConeReferences
	below    func(libfossil.FslID) bool // nil without a horizon the store can place
	clusters map[libfossil.FslID]bool   // nil without a horizon
	walk     *coneWalk
	// unreached are the manifests and nodes a structural request does not
	// keep (horizon §5.1, §6.2); nil for any other request.
	unreached map[libfossil.FslID]bool
	// slots are the nodes a horizon request with path members keeps of the
	// trees its horizon keeps (horizon §6.2); nil for any other request.
	slots *content.SlotKeep
	// read is how many artifacts the classification read from the store.
	read int
	// joined reports a cursor from before the held set gained a check-in
	// at or below it (horizon §6.2.6); nil without a horizon.
	joined func(cursor int64) (bool, error)
}

// withStructure makes k keep, of the store's manifests and nodes, only those
// a structural request's held set reaches (horizon §6.2).
func (k *sparseKeep) withStructure(q db.Querier, s *structuralServe) error {
	if s == nil {
		return nil
	}
	rows, err := q.Query("SELECT objid, uuid FROM event JOIN blob ON blob.rid=event.objid WHERE type='ci'")
	if err != nil {
		return fmt.Errorf("sparse keep: check-ins: %w", err)
	}
	k.unreached = map[libfossil.FslID]bool{}
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return errors.Join(err, rows.Close())
		}
		if !s.reach[uuid] {
			k.unreached[rid] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("sparse keep: check-ins: %w", err)
	}
	for rid, uuid := range k.refs.Nodes() {
		if !s.reach[uuid] {
			k.unreached[rid] = true
		}
	}
	return nil
}

// withSlots makes k withhold, for a horizon request that also carries path
// members, the nodes of the trees the horizon keeps that no slot that may
// hold an in-cone path reaches (horizon §6.2): every check-in's trees at the
// content depth, those s reaches at the structural one.
func (k *sparseKeep) withSlots(ctx context.Context, cl classifier, c *cone.Cone, s *structuralServe) error {
	if _, ok := c.Horizon(); !ok || len(c.Include()) == 0 {
		return nil
	}
	var reach map[string]bool
	if s != nil {
		reach = s.reach
	}
	var err error
	k.slots, err = cl.slots(ctx, k.refs, c, reach)
	return err
}

func newSparseKeep(ctx context.Context, q db.Querier, c *cone.Cone) (*sparseKeep, error) {
	return newSparseKeepWith(ctx, q, c, freshClassifier{})
}

// newSparseKeepWith is the keep set of c as cl classifies the store.
func newSparseKeepWith(ctx context.Context, q db.Querier, c *cone.Cone, cl classifier) (*sparseKeep, error) {
	if c == nil {
		return nil, nil
	}
	refs, err := cl.references(ctx, q, c)
	if err != nil {
		return nil, err
	}
	k := &sparseKeep{refs: refs, read: cl.read(refs)}
	if _, ok := c.Horizon(); !ok {
		return k, nil
	}
	if k.below, err = cl.below(ctx, q, refs, c); err != nil {
		return nil, err
	}
	if k.clusters, err = clusterRIDs(ctx, q); err != nil {
		return nil, err
	}
	h, _ := c.Horizon()
	k.joined = func(cursor int64) (bool, error) { return cl.joinedAfter(ctx, q, h, cursor) }
	return k, nil
}

// clusterRIDs names every cluster the store has crosslinked, held or not.
func clusterRIDs(ctx context.Context, q db.Querier) (map[libfossil.FslID]bool, error) {
	rows, err := q.Query("SELECT rid FROM tagxref WHERE tagid=7")
	if err != nil {
		return nil, fmt.Errorf("sparse keep: clusters: %w", err)
	}
	out := map[libfossil.FslID]bool{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out[rid] = true
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func (h *handler) ensureSparseKeep() error {
	if h.cone == nil || h.keep != nil {
		return nil
	}
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if h.walkClone {
		w := newConeWalk(ctx, h)
		if err := w.placeHorizon(); err != nil {
			return err
		}
		h.keep = &sparseKeep{walk: w}
		return nil
	}
	cl := h.judged
	if cl == nil {
		var err error
		if cl, err = h.classifier(ctx, h.repo.DB()); err != nil {
			return err
		}
	}
	keep, err := newSparseKeepWith(ctx, h.repo.DB(), h.cone, cl)
	if err != nil {
		return err
	}
	if err := keep.withStructure(h.repo.DB(), h.structural); err != nil {
		return err
	}
	if err := keep.withSlots(ctx, cl, h.cone, h.structural); err != nil {
		return err
	}
	h.keep = keep
	return nil
}

func (k *sparseKeep) classified() int {
	switch {
	case k == nil:
		return 0
	case k.walk != nil:
		return k.walk.classified
	}
	return k.read
}

// keeps filters a pull's inventory; a clone's walk never asks.
func (k *sparseKeep) keeps(rid libfossil.FslID) bool {
	if k == nil || k.walk != nil {
		return true
	}
	return !k.refs.Outside(rid) && (k.below == nil || !k.below(rid)) && !k.clusters[rid] && !k.unreached[rid] &&
		!k.slots.Withholds(rid)
}

// withholds reports a node a pull's request may not have even by name
// (horizon §6.2): one its slot walk withholds.
func (k *sparseKeep) withholds(rid libfossil.FslID) bool {
	return k != nil && k.walk == nil && k.slots.Withholds(rid)
}

// voids reports a cursor from before the classification kept something
// older (horizon §6.2.6): a node the slot walk kept, or a check-in that
// joined the held set.
func (k *sparseKeep) voids(cursor int64) (bool, error) {
	if k == nil || k.walk != nil {
		return false, nil
	}
	if k.slots.Voids(cursor) {
		return true, nil
	}
	if k.joined == nil {
		return false, nil
	}
	return k.joined(cursor)
}

func (k *sparseKeep) sourceKept(rid int) bool {
	if k != nil && k.walk != nil {
		return k.walk.sourceKept(rid)
	}
	return k.keeps(libfossil.FslID(rid))
}

// filterSparseReceived filters only artifacts this transfer newly acquired.
// Existing holdings survive a narrower policy; only explicit compaction may
// reclaim them. Reference discovery precedes filtering so arrival order cannot
// discard a blob before a later manifest names it inside the cone. Under a
// content horizon the store so classifies locally what a horizon-ignorant
// peer sent densely (draft-fossil-horizon-00 §6.3). A blob no held manifest
// names yet is kept, as an unreferenced holding, and a held delta's base is
// kept whatever its class (§4.2).
func filterSparseReceived(ctx context.Context, q db.Querier, received map[string]bool, cache *content.Cache) (int, error) {
	c, err := content.StoreCone(q)
	if err != nil || c == nil {
		return 0, err
	}
	k, err := newSparseKeep(ctx, q, c)
	if err != nil {
		return 0, err
	}
	drop := map[libfossil.FslID]bool{}
	for uuid := range received {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var rid libfossil.FslID
		var size int64
		err := q.QueryRow("SELECT rid,size FROM blob WHERE uuid=?", uuid).Scan(&rid, &size)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if size >= 0 && !k.keeps(rid) {
			drop[rid] = true
		}
	}
	if err := k.refs.RetainProvenance(ctx, drop); err != nil {
		return 0, err
	}
	if err := retainLocalReceived(ctx, q, drop); err != nil {
		return 0, err
	}
	if err := retainDeltaSources(ctx, q, drop); err != nil {
		return 0, err
	}
	rids := make([]libfossil.FslID, 0, len(drop))
	for rid := range drop {
		rids = append(rids, rid)
	}
	sort.Slice(rids, func(i, j int) bool { return rids[i] < rids[j] })
	for _, rid := range rids {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if _, err := q.Exec("DELETE FROM delta WHERE rid=?", rid); err != nil {
			return 0, err
		}
		if _, err := q.Exec("UPDATE blob SET content=NULL,size=-1 WHERE rid=?", rid); err != nil {
			return 0, err
		}
		if _, err := q.Exec("INSERT OR IGNORE INTO phantom(rid) VALUES(?)", rid); err != nil {
			return 0, err
		}
		if _, err := q.Exec("DELETE FROM unclustered WHERE rid=?", rid); err != nil {
			return 0, err
		}
		cache.Invalidate(rid)
	}
	a, err := content.LoadAbsencesContext(ctx, q)
	if err != nil {
		return 0, err
	}
	return a.Count(content.Elided), nil
}

// Private or unsent artifacts may have no peer that can restore their bytes.
func retainLocalReceived(ctx context.Context, q db.Querier, drop map[libfossil.FslID]bool) error {
	rows, err := q.Query("SELECT rid FROM unsent UNION SELECT rid FROM private")
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rows.Close())
		}
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return errors.Join(err, rows.Close())
		}
		delete(drop, rid)
	}
	return errors.Join(rows.Err(), rows.Close())
}

// Remove every retained delta's transitive sources from the drop set. Each
// held delta edge is visited at most once, even for shared bases or cycles.
func retainDeltaSources(ctx context.Context, q db.Querier, drop map[libfossil.FslID]bool) error {
	rows, err := q.Query("SELECT d.rid,d.srcid FROM delta d JOIN blob b ON b.rid=d.rid WHERE b.size>=0")
	if err != nil {
		return err
	}
	sources := map[libfossil.FslID]libfossil.FslID{}
	var queue []libfossil.FslID
	for rows.Next() {
		var rid, src libfossil.FslID
		if err := rows.Scan(&rid, &src); err != nil {
			return errors.Join(err, rows.Close())
		}
		sources[rid] = src
		if !drop[rid] {
			queue = append(queue, rid)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	seen := map[libfossil.FslID]bool{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		rid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[rid] {
			continue
		}
		seen[rid] = true
		if src, ok := sources[rid]; ok {
			delete(drop, src)
			queue = append(queue, src)
		}
	}
	return nil
}

func rememberSparseReceived(q db.Querier, received map[string]bool, uuid string) error {
	if received == nil {
		return nil
	}
	held, err := heldReal(q, uuid)
	if err != nil {
		return err
	}
	if !held {
		received[uuid] = true
	}
	return nil
}

// coversRequest reports whether this store, under its cone have, holds all a
// clone under the request's cone keeps (sparse-cone §5.3, horizon §6.4): the
// request's paths lie inside the store's, and under a store horizon the
// request names a horizon whose held set lies inside the store's own, both
// computed over the store. A horizon the store cannot place covers nothing.
// The horizon judged is the one asked: a structural request this store
// answers as content (prepareStructural) still asks for no manifest below C.
func (h *handler) coversRequest(have *cone.Cone) (bool, error) {
	if !coneContains(have, h.cone) {
		return false, nil
	}
	if have == nil {
		return true, nil
	}
	own, ok := have.Horizon()
	if !ok {
		return true, nil
	}
	if h.asked.Hash() == "" {
		return false, nil
	}
	// A structural store holds no manifest below its horizon, which a
	// content-horizon clone keeps.
	if own.Depth() == cone.DepthStructural && h.asked.Depth() != cone.DepthStructural {
		return false, nil
	}
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	held, placed, err := heldWithin(ctx, freshClassifier{}, h.repo.DB(), own, h.asked)
	return held && placed, err
}

// judgeHeld decides whether a horizon pull or sync asks for a held set this
// store's horizon does not contain (horizon §6.4), which refuseHorizon then
// refuses. It runs after the card loop, so only a pull card that passed its
// permission check is judged, and a peer's push is already stored. Only K
// is compared: a pull at the store's own C and rule is served whatever its
// depth, since a structural request is answered at the content level here.
// A requested C this store does not hold is a peer ahead of it, not one
// asking for history it lacks, and is served what the store holds. A
// partial mirror serves what it holds, as it does a clone it does not cover.
func (h *handler) judgeHeld(ctx context.Context) error {
	if ctx == nil {
		panic("handler.judgeHeld: nil context")
	}
	if !h.pullOK || h.asked.Hash() == "" {
		return nil
	}
	q := h.repo.DB()
	have, err := content.StoreCone(q)
	if err != nil {
		return err
	}
	own, ok := horizonOf(have)
	if !ok {
		return nil
	}
	mirror, err := h.servesPartialMirror()
	if err != nil || mirror {
		return err
	}
	// The inventory classifies through the same view: nothing is written
	// between here and there, so the store it reads is the one judged.
	if h.judged, err = h.classifier(ctx, q); err != nil {
		return err
	}
	held, placed, err := heldWithin(ctx, h.judged, q, own, h.asked)
	h.notHeld = placed && !held
	return err
}

// heldWithin reports whether want's held set is a subset of own's, both
// placed over q through cl, and whether q holds want's C at all. A store
// that cannot place its own horizon holds nothing.
func heldWithin(ctx context.Context, cl classifier, q db.Querier, own, want cone.Horizon) (held, placed bool, err error) {
	if ctx == nil || cl == nil || q == nil {
		panic("sync.heldWithin: nil context, classifier or querier")
	}
	// A peer at the store's own C and rule, the usual case in a mesh, asks
	// for the same set; the held-set walks are skipped for it.
	if own.Hash() == want.Hash() && own.Rule() == want.Rule() {
		return true, true, nil
	}
	asked, placed, err := cl.heldSet(ctx, q, want)
	if err != nil || !placed {
		return false, placed, err
	}
	k, ownPlaced, err := cl.heldSet(ctx, q, own)
	if err != nil || !ownPlaced {
		return false, true, err
	}
	inside := make(map[libfossil.FslID]bool, len(k))
	for _, rid := range k {
		inside[rid] = true
	}
	for _, rid := range asked {
		if !inside[rid] {
			return false, true, nil
		}
	}
	return true, true, nil
}

func coneContains(have, want *cone.Cone) bool {
	if have == nil {
		return true
	}
	if want == nil {
		return false
	}
	// A horizon-only request keeps every path, as a dense one does.
	if len(want.Include()) == 0 && len(have.Include()) > 0 {
		return false
	}
	// Widening beneath an excluded interval cannot be certified by the root
	// member alone. Probe every policy boundary, including re-inclusions.
	for _, m := range want.Include() {
		if !have.Covers(m) {
			return false
		}
	}
	for _, m := range have.Exclude() {
		if want.Covers(m) {
			return false
		}
	}
	return true
}

// localConePragmas reads policy at request time so a widening affects the
// ordinary synchronization path rather than needing a second transfer protocol.
func localConePragmas(q db.Querier) ([]xfer.Card, error) {
	c, err := content.StoreCone(q)
	if err != nil {
		return nil, err
	}
	offer := ""
	if h, ok := horizonOf(c); ok && h.Depth() == cone.DepthStructural {
		if offer, err = seedOffer(q); err != nil {
			return nil, err
		}
	}
	return conePragmas(c, offer), nil
}

// horizonOf is c's horizon, ok false for a dense store or a cone without one.
func horizonOf(c *cone.Cone) (cone.Horizon, bool) {
	if c == nil {
		return cone.Horizon{}, false
	}
	return c.Horizon()
}
