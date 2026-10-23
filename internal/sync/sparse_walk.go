package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// The §8 bounds of single-name lookup, charged across one request.
const (
	coneWalkDepth  = 16
	coneWalkVisits = 1 << 20
)

// coneWalkCacheBytes bounds the expanded content a walk keeps for
// classifying rows. A row's kind is decided by parsing its bytes, and without
// a cache each row's whole delta chain is expanded again: on fossil-scm.org's
// repository that left a cone clone's first round unfinished after 20
// minutes. The cache is unchecked because classifying serves nothing: the
// bytes a row is sent with are expanded and checked against its name apart,
// and re-hashing every node rebuilt for classification cost a third of a
// cone clone's CPU.
const coneWalkCacheBytes = 64 << 20

type sparseKind uint8

const (
	sparseContent    sparseKind = iota // bytes no artifact parses from
	sparseNode                         // sub-manifest node: sent by a walk, or at its row under a horizon alone
	sparseStructural                   // held densely (§5.1)
	sparseCheckin
	sparseCluster // held densely, but never sent under a horizon (horizon §4.5)
)

// coneWalk is §6.2 keep-set discovery for one clone round. A row is read
// only when the round reaches it, and a check-in's tree is walked only when
// the round emits that check-in, so the work tracks the round's batch. Each
// check-in sends what its primary parent's tree does not already hold; the
// parent is a row of the same clone, so by induction every in-cone node and
// blob of every check-in is sent once its own check-in is.
//
// Under a content horizon (draft-fossil-horizon-00 §6.2) only a check-in in
// the held set walks its tree for blobs, and its parent counts as sent only
// when it is in the held set too: the blobs sent are those above the
// horizon. Every structural artifact but a cluster goes out at its row, as
// §4.1 holds them at every age, and so does every node of a horizon alone.
// With path members too, a node goes out only by a walk, as without a
// horizon: every other check-in the request keeps walks its tree for its
// nodes alone, so the nodes sent are those on paths into the cone of every
// tree the horizon keeps, and the rest are withheld.
type coneWalk struct {
	ctx        context.Context
	h          *handler
	cone       *cone.Cone
	horizon    bool
	paths      bool            // the request carries path members
	nodesOnly  bool            // the tree being walked sends its nodes and no blob
	held       map[int]bool    // the held set; nil when there is no horizon or the store cannot place C
	reach      map[string]bool // a structural request's kept manifests and nodes (§5.1); nil otherwise
	kinds      map[int]sparseKind
	decks      map[int]*deck.Deck
	reached    map[int]bool
	path       map[string]bool
	cache      *content.Cache // unchecked, for classifying; a walk serves one request
	fileRows   map[int]bool   // rows the crosslink tables prove are file content; nil until read
	keepSet    *sparseKeep    // the classification's keep set, for unreached bases; nil until read
	keepFailed bool           // the classification failed; unreached bases are not kept
	visits     int
	classified int
}

func newConeWalk(ctx context.Context, h *handler) *coneWalk {
	if ctx == nil || h == nil || h.cone == nil {
		panic("sync.newConeWalk: nil context, handler or cone")
	}
	_, horizon := h.cone.Horizon()
	w := &coneWalk{ctx: ctx, h: h, cone: h.cone, horizon: horizon, paths: len(h.cone.Include()) > 0,
		kinds: map[int]sparseKind{},
		decks: map[int]*deck.Deck{}, reached: map[int]bool{}, path: map[string]bool{},
		cache: content.NewUncheckedCache(coneWalkCacheBytes)}
	if h.structural != nil {
		w.reach = h.structural.reach
	}
	return w
}

// placeHorizon computes the held set over the store, bounded as every
// server-side graph walk is (§8). A C the store cannot place proves no blob
// below the horizon, so every check-in then walks its tree, as a request
// without one would.
func (w *coneWalk) placeHorizon() error {
	h, ok := w.cone.Horizon()
	if !ok {
		return nil
	}
	k, placed, err := horizon.Place(w.ctx, w.h.repo.DB(), h)
	if err != nil || !placed {
		return err
	}
	w.held = make(map[int]bool, len(k))
	for _, rid := range k {
		w.held[int(rid)] = true
	}
	return nil
}

// inHeld reports a check-in whose tree the request keeps.
func (w *coneWalk) inHeld(rid int) bool {
	return w.held == nil || w.held[rid]
}

// atRow reports a kind sent at its own row rather than by a walk.
func (w *coneWalk) atRow(kind sparseKind) bool {
	switch kind {
	case sparseStructural, sparseCheckin:
		return true
	case sparseNode:
		return w.horizon && !w.paths
	case sparseCluster:
		return !w.horizon
	}
	return false
}

// classify parses rid once per request. Unreadable bytes stay structural:
// they go out at their row and fail there exactly as a dense clone would.
func (w *coneWalk) classify(rid int) (sparseKind, *deck.Deck) {
	if kind, ok := w.kinds[rid]; ok {
		return kind, w.decks[rid]
	}
	w.classified++
	if w.knownFile(rid) {
		w.kinds[rid] = sparseContent
		return sparseContent, nil
	}
	kind, d := sparseStructural, (*deck.Deck)(nil)
	data, err := w.cache.ExpandUnchecked(w.h.repo.DB(), libfossil.FslID(rid))
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

// fileContentSQL selects the blobs the store's crosslink tables prove to be
// file content: named by mlink as a file version and claimed by no table an
// artifact's crosslink writes. A file whose bytes parse as an artifact was
// crosslinked as one, so it is claimed and falls through to its bytes.
const fileContentSQL = `SELECT fid FROM mlink WHERE fid>0
	EXCEPT SELECT objid FROM event
	EXCEPT SELECT cid FROM plink
	EXCEPT SELECT srcid FROM tagxref WHERE srcid>0
	EXCEPT SELECT rid FROM tagxref WHERE tagid=7
	EXCEPT SELECT attachid FROM attachment
	EXCEPT SELECT fpid FROM forumpost
	EXCEPT SELECT tkt_rid FROM ticketchng`

// knownFile reports a row the crosslink tables prove to be file content, so
// classify need not expand it. Expanding every file version of a large file
// (fossil-scm.org's repository carries hundreds of a 9 MB one) cost most of a
// cone clone's CPU. A store that has seen a sub-manifest answers nothing:
// nodes have no table to claim them. The set is read once per walk.
func (w *coneWalk) knownFile(rid int) bool {
	if w.fileRows == nil {
		w.fileRows = w.loadFileRows()
	}
	return w.fileRows[rid]
}

// loadFileRows reads fileContentSQL, or returns an empty set when the store has
// seen a sub-manifest or the read fails, which leaves every row to its bytes.
func (w *coneWalk) loadFileRows() map[int]bool {
	files := map[int]bool{}
	q := w.h.repo.DB()
	var trees bool
	if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM config WHERE name=?)",
		manifest.SeenSubmanifestSetting).Scan(&trees); err != nil || trees {
		return files
	}
	rows, err := q.Query(fileContentSQL)
	if err != nil {
		return files
	}
	defer rows.Close()
	for rows.Next() {
		var rid int
		if err := rows.Scan(&rid); err != nil {
			return map[int]bool{}
		}
		files[rid] = true
	}
	if rows.Err() != nil {
		return map[int]bool{}
	}
	return files
}

func deckKind(d *deck.Deck) sparseKind {
	switch d.Type {
	case deck.Submanifest:
		return sparseNode
	case deck.Checkin:
		return sparseCheckin
	case deck.Cluster:
		return sparseCluster
	}
	return sparseStructural
}

// unreached reports a manifest or node a structural request does not keep:
// one no tree of the held set reaches (horizon §5.1).
func (w *coneWalk) unreached(kind sparseKind, uuid string) bool {
	return w.reach != nil && (kind == sparseCheckin || kind == sparseNode) && !w.reach[uuid]
}

// sourceKept is the §6.2 keep-set test for a served delta's base.
func (w *coneWalk) sourceKept(rid int) bool {
	if w.reached[rid] {
		return true
	}
	kind, _ := w.classify(rid)
	if !w.atRow(kind) {
		return kind == sparseContent && w.contentKept(rid)
	}
	if w.reach == nil || kind != sparseCheckin && kind != sparseNode {
		return true
	}
	info, ok, err := w.h.loadCloneBlob(rid)
	return err == nil && ok && !w.unreached(kind, info.uuid)
}

// contentKept reports a blob the request's keep set holds although the walk
// has not reached it yet, by the store's classification of the cone (the
// one a pull's inventory reads, kept across requests by the origin memo).
// The walk reaches a file version by its check-in, in rid order, while
// fossil deltifies a version against the newer one: without this a held
// version's base was never yet reached and every version went out in full,
// a whole 9 MB text per version of fossil-scm.org's sqlite3.c. A base the
// classification cannot answer for is not kept, which costs bytes, never
// correctness (SC-6.2-2).
func (w *coneWalk) contentKept(rid int) bool {
	if w.keepSet == nil && !w.keepFailed {
		keep, err := w.classifiedKeep()
		if err != nil {
			w.keepFailed = true
			return false
		}
		w.keepSet = keep
	}
	return w.keepSet != nil && w.keepSet.keeps(libfossil.FslID(rid))
}

// classifiedKeep is the request's §6.2 keep set as the store's
// classification computes it, without the walk.
func (w *coneWalk) classifiedKeep() (*sparseKeep, error) {
	q := w.h.repo.DB()
	cl, err := w.h.classifier(w.ctx, q)
	if err != nil {
		return nil, err
	}
	return newSparseKeepWith(w.ctx, q, w.cone, cl)
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

// items returns, ascending, the rows the artifact d at structural row rid
// brings with it: an attachment's source, or a check-in's new in-cone tree.
func (w *coneWalk) items(rid int, d *deck.Deck) ([]int, error) {
	out := map[int]bool{}
	var err error
	switch {
	case d.A != nil && d.A.Source != "":
		err = w.add(d.A.Source, out)
	case d.Type == deck.Checkin && !w.inHeld(rid) && len(d.S) > 0 && !w.atRow(sparseNode):
		err = w.nodes(d, out)
	case d.Type == deck.Checkin && !w.inHeld(rid):
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
	for item := range out {
		w.reached[item] = true
		rids = append(rids, item)
	}
	sort.Ints(rids)
	return rids, nil
}

// nodes sends the in-cone nodes of the tree of d, a check-in whose blobs
// the request does not keep, that its primary parent's tree does not
// already carry over the same range. A parent's nodes went out when it did:
// every check-in's under a content horizon, a reached one's under a
// structural horizon.
func (w *coneWalk) nodes(d *deck.Deck, out map[int]bool) error {
	prior := coneTreeLevel{}
	if len(d.P) > 0 {
		p, _, err := w.checkin(d.P[0])
		if err != nil {
			return err
		}
		if p != nil && (w.reach == nil || w.reach[d.P[0]]) {
			prior.cards = p.S
		}
	}
	w.nodesOnly = true
	err := w.tree(coneTreeLevel{cards: d.S}, prior, nil, 1, out)
	w.nodesOnly = false
	return err
}

func (w *coneWalk) add(uuid string, out map[int]bool) error {
	rid, ok, err := w.servable(uuid)
	if ok {
		out[rid] = true
	}
	return err
}

// parent is the primary parent check-in when this clone sends its tree,
// else nil: then nothing is known to be sent already and the whole cone
// goes out.
func (w *coneWalk) parent(d *deck.Deck) (*deck.Deck, error) {
	if len(d.P) == 0 {
		return nil, nil
	}
	p, rid, err := w.checkin(d.P[0])
	if err != nil || p == nil || !w.inHeld(rid) {
		return nil, err
	}
	return p, nil
}

func (w *coneWalk) checkin(uuid string) (*deck.Deck, int, error) {
	rid, ok, err := w.servable(uuid)
	if err != nil || !ok {
		return nil, 0, err
	}
	if kind, d := w.classify(rid); kind == sparseCheckin {
		return d, rid, nil
	}
	return nil, 0, nil
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
	base, _, err := w.checkin(d.B)
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
		if err := w.ctx.Err(); err != nil {
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
