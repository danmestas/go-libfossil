package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// The store cone of draft-fossil-sparse-cone-00 §5 lives in a table of its
// own, created when a cone is set and dropped when it is cleared, so a dense
// repository's schema is untouched. `fossil rebuild` drops every table it
// does not know, which puts the cone in the disposable tier §5.2 recommends:
// losing it reclassifies every elided phantom as a transit phantom, and the
// store heals toward dense through ordinary synchronization.
const storeConeSchema = `CREATE TABLE IF NOT EXISTS storecone(
  id INTEGER PRIMARY KEY CHECK(id=1),
  cone BLOB NOT NULL
)`

// StoreCone returns the store cone, or nil when the store is dense. A cone
// this implementation cannot read -- a later serialization version, or bytes
// that do not parse -- is policy loss (§3.1, §5.2) and reads as dense, never
// as an error: the cost is bandwidth, not data.
func StoreCone(q db.Querier) (*cone.Cone, error) {
	if q == nil {
		panic("content.StoreCone: q must not be nil")
	}
	var n int
	if err := q.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='storecone'",
	).Scan(&n); err != nil {
		return nil, fmt.Errorf("content.StoreCone: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	var data []byte
	err := q.QueryRow("SELECT cone FROM storecone WHERE id=1").Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content.StoreCone: %w", err)
	}
	c, err := cone.Parse(data)
	if err != nil {
		return nil, nil
	}
	return c, nil
}

// SetStoreCone replaces the store cone; nil makes the store dense. Neither
// direction moves content: narrowing deletes nothing (§5.4), and widening
// only makes the newly covered elided phantoms wanted again, for the next
// synchronization to fetch. The boundary labels of a structural horizon
// (draft-fossil-horizon-00 §5.3) name what lies below one C, so a cone whose
// horizon differs drops them.
func SetStoreCone(q db.Querier, c *cone.Cone) error {
	if q == nil {
		panic("content.SetStoreCone: q must not be nil")
	}
	old, err := StoreCone(q)
	if err != nil {
		return err
	}
	if horizonOf(old) != horizonOf(c) {
		if _, err := q.Exec("DROP TABLE IF EXISTS " + belowLabelTable); err != nil {
			return fmt.Errorf("content.SetStoreCone: %w", err)
		}
	}
	if c == nil {
		if _, err := q.Exec("DROP TABLE IF EXISTS storecone"); err != nil {
			return fmt.Errorf("content.SetStoreCone: %w", err)
		}
		return nil
	}
	if _, err := q.Exec(storeConeSchema); err != nil {
		return fmt.Errorf("content.SetStoreCone: %w", err)
	}
	if _, err := q.Exec("REPLACE INTO storecone(id, cone) VALUES(1, ?)", c.Serialize()); err != nil {
		return fmt.Errorf("content.SetStoreCone: %w", err)
	}
	return nil
}

// Absence classifies an artifact the store names: held, or absent for one
// of the three reasons of §5.2.
type Absence int

const (
	// Held is not absent: the store holds the content.
	Held Absence = iota
	// Transit is wanted and not yet arrived; synchronization requests it.
	Transit
	// Shunned is forbidden and never requested.
	Shunned
	// Elided is deliberately absent under the store cone. Synchronization
	// does not request it, and its absence is healthy.
	Elided
)

func (a Absence) String() string {
	switch a {
	case Held:
		return "held"
	case Transit:
		return "transit"
	case Shunned:
		return "shunned"
	case Elided:
		return "elided"
	}
	return fmt.Sprintf("Absence(%d)", int(a))
}

// Absences is the classification of every phantom in a store, computed from
// the store cone at load time. Elision is never stored on a phantom (§5.2):
// a cone change is a reload, not a migration.
type Absences struct {
	cone   *cone.Cone
	class  map[libfossil.FslID]Absence
	counts [Elided + 1]int
	work   ReferenceWork // what classification read, for tests to bound
	// below are the elided phantoms a structural horizon proves to be
	// manifests below it: labelled boundary hashes and the targets only a
	// tag or cherry-pick names (draft-fossil-horizon-00 §5.4).
	below map[libfossil.FslID]bool
}

// LoadAbsences classifies the store's phantoms. Shunned names win; raw F/S
// provenance proves elision only outside every covered alias and structural
// reference. Retained delta ancestors remain transit. Crosslink references
// supplement raw provenance for historical names, including prior rename paths.
// A partial tree need not be crosslinked to classify its outside S children.
// A content horizon in the cone also elides the blobs below it and the
// clusters the store was sent.
func LoadAbsences(q db.Querier) (*Absences, error) {
	return LoadAbsencesContext(context.Background(), q)
}

// LoadAbsencesContext carries cancellation through raw provenance discovery.
func LoadAbsencesContext(ctx context.Context, q db.Querier) (*Absences, error) {
	return loadAbsences(ctx, q, defaultReferenceLimits)
}

func loadAbsences(ctx context.Context, q db.Querier, limits referenceLimits) (*Absences, error) {
	if ctx == nil || q == nil {
		panic("content.LoadAbsencesContext: nil context or querier")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := StoreCone(q)
	if err != nil {
		return nil, err
	}
	a := &Absences{cone: c, class: map[libfossil.FslID]Absence{}}
	hasHorizon := false
	if c != nil {
		_, hasHorizon = c.Horizon()
	}
	rows, err := q.Query(`
		SELECT p.rid,
		       EXISTS(SELECT 1 FROM shun WHERE uuid=b.uuid),
		       EXISTS(SELECT 1 FROM delta d JOIN blob h ON h.rid=d.rid
		              WHERE d.srcid=p.rid AND h.size>=0),
		       EXISTS(SELECT 1 FROM tagxref WHERE tagid=7 AND rid=p.rid)
		FROM phantom p JOIN blob b ON b.rid=p.rid`)
	if err != nil {
		return nil, fmt.Errorf("content.LoadAbsences: phantoms: %w", err)
	}
	candidates := map[libfossil.FslID]bool{}
	for rows.Next() {
		var rid libfossil.FslID
		var shunned, base, cluster bool
		if err := rows.Scan(&rid, &shunned, &base, &cluster); err != nil {
			rows.Close()
			return nil, fmt.Errorf("content.LoadAbsences: phantoms: %w", err)
		}
		switch {
		case shunned:
			a.class[rid] = Shunned
		case cluster && hasHorizon && !base:
			// A cluster the store was sent and did not keep (horizon §4.5):
			// the cluster tag outlives its content, and a horizon store
			// never asks for it back.
			a.class[rid] = Elided
		default:
			a.class[rid] = Transit
			if c != nil && !base {
				candidates[rid] = true
			}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("content.LoadAbsences: phantoms: %w", err)
	}
	if len(candidates) > 0 {
		if err := a.classify(ctx, q, candidates, limits); err != nil {
			return nil, err
		}
	}
	for _, k := range a.class {
		a.counts[k]++
	}
	return a, nil
}

// classify decides the candidates, the phantoms that are neither shunned
// nor a held delta's base. Path members elide by name. A content horizon
// (draft-fossil-horizon-00 §3.2, §4) then elides every blob no check-in in
// its held set names, so a kept blob is in both keep sets; it never elides
// structure, and under one path members do not either.
func (a *Absences) classify(
	ctx context.Context, q db.Querier, candidates map[libfossil.FslID]bool, limits referenceLimits,
) error {
	facts, work, err := loadConeReferenceFactsBounded(ctx, q, limits.refs, true)
	a.work = work
	if err != nil {
		return err
	}
	refs, err := facts.Classify(ctx, a.cone)
	if err != nil {
		return err
	}
	pool := maps.Clone(candidates)
	if err := keepRequiredPhantoms(q, candidates, refs); err != nil {
		return err
	}
	if err := a.elide(q, candidates); err != nil {
		return err
	}
	h, hasHorizon := a.cone.Horizon()
	for rid := range candidates {
		if hasHorizon && refs.structure(rid) {
			a.class[rid] = Transit
		} else if refs.Outside(rid) {
			a.class[rid] = Elided
		} else if refs.Required(rid) {
			a.class[rid] = Transit
		}
	}
	if !hasHorizon {
		return nil
	}
	cut, placed, err := horizonTrees(ctx, q, h, limits, &a.work)
	if err != nil || !placed {
		return err
	}
	for rid := range candidates {
		if refs.below(cut.above, rid) {
			a.class[rid] = Elided
		}
	}
	if h.Depth() != cone.DepthStructural {
		return nil
	}
	return a.classifyStructural(q, pool, refs, cut)
}

// classifyStructural applies a structural horizon (draft-fossil-horizon-00
// §5.1, §5.3) to the phantoms that are neither shunned nor a held delta's
// base. The store requests what a P, F, S or B card of a held artifact names,
// or an attachment carries (§4.5); of those, a parent the origin labelled
// below the horizon, a baseline or node no tree of K reaches, and a node the
// path members exclude (§7) are elided, and an unlabelled parent stays
// transit, since it may only be late. Every
// other phantom -- the target of a tag or a cherry-pick, a manifest dropped
// after it was linked -- is a manifest below the horizon that nothing in K
// reaches, and is elided. A name an origin announces is still requested
// during its synchronization; the classification is of the store at rest.
func (a *Absences) classifyStructural(
	q db.Querier, pool map[libfossil.FslID]bool, refs *ConeReferences, cut horizonCut,
) error {
	labels, err := BelowLabels(q)
	if err != nil {
		return err
	}
	a.below = map[libfossil.FslID]bool{}
	parents, err := ridSet(q, "SELECT pid FROM plink")
	if err != nil {
		return err
	}
	baselines, err := ridSet(q, "SELECT baseid FROM plink WHERE baseid>0 UNION SELECT baseline FROM orphan")
	if err != nil {
		return err
	}
	attached, err := ridSet(q, "SELECT b.rid FROM attachment a JOIN blob b ON b.uuid=a.src")
	if err != nil {
		return err
	}
	for rid := range pool {
		uuid := refs.names[rid]
		switch {
		case labels[uuid]:
			a.class[rid] = Elided
			a.below[rid] = true
		case parents[rid]:
		case refs.tree[uuid] || baselines[rid]:
			// A node of K's trees that path members exclude is elided too:
			// an origin may withhold it, as a path cone's origin does.
			if !cut.reach[uuid] && !refs.file(rid) || refs.tree[uuid] && refs.Outside(rid) {
				a.class[rid] = Elided
			}
		case refs.file(rid) || attached[rid]:
		default:
			a.class[rid] = Elided
			a.below[rid] = true
		}
	}
	return nil
}

// ridSet reads one column of rids.
func ridSet(q db.Querier, query string) (map[libfossil.FslID]bool, error) {
	rows, err := q.Query(query)
	if err != nil {
		return nil, fmt.Errorf("content.LoadAbsences: %w", err)
	}
	out := map[libfossil.FslID]bool{}
	for rows.Next() {
		var rid sql.NullInt64
		if err := rows.Scan(&rid); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if rid.Valid {
			out[libfossil.FslID(rid.Int64)] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("content.LoadAbsences: %w", err)
	}
	return out, nil
}

// An artifact may be both a file's content and a structural reference or
// delta ancestor. Its out-of-cone filename does not permit either hole.
func keepRequiredPhantoms(q db.Querier, candidates map[libfossil.FslID]bool, refs *ConeReferences) error {
	var haveForum bool
	if err := q.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM sqlite_master WHERE name='forumpost' AND type='table')`).
		Scan(&haveForum); err != nil {
		return fmt.Errorf("content.LoadAbsences: forum table: %w", err)
	}
	query := `
		WITH RECURSIVE retained(rid) AS (
		  SELECT d.srcid FROM delta d JOIN blob b ON b.rid=d.rid WHERE b.size>=0
		  UNION SELECT d.srcid FROM delta d JOIN retained r ON d.rid=r.rid
		)
		SELECT rid FROM retained
		UNION SELECT objid FROM event
		UNION SELECT mid FROM mlink
		UNION SELECT pid FROM plink
		UNION SELECT cid FROM plink
		UNION SELECT baseid FROM plink
		UNION SELECT rid FROM orphan
		UNION SELECT srcid FROM tagxref
		UNION SELECT origid FROM tagxref
		UNION SELECT rid FROM tagxref
		UNION SELECT attachid FROM attachment
		UNION SELECT b.rid FROM attachment a JOIN blob b ON b.uuid=a.src
		UNION SELECT parentid FROM cherrypick
		UNION SELECT childid FROM cherrypick`
	if haveForum {
		query += `
		UNION SELECT fpid FROM forumpost
		UNION SELECT froot FROM forumpost
		UNION SELECT fprev FROM forumpost
		UNION SELECT firt FROM forumpost`
	}
	rows, err := q.Query(query + " INTERSECT SELECT rid FROM phantom")
	if err != nil {
		return fmt.Errorf("content.LoadAbsences: structural references: %w", err)
	}
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return fmt.Errorf("content.LoadAbsences: structural references: %w", err)
		}
		delete(candidates, rid)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("content.LoadAbsences: structural references: %w", err)
	}
	// orphan.baseline also records deferred S children, not just B baselines.
	// Only raw tree provenance can distinguish a healthy outside subtree.
	rows, err = q.Query("SELECT baseline FROM orphan")
	if err != nil {
		return err
	}
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return errors.Join(err, rows.Close())
		}
		if !refs.tree[refs.names[rid]] || !refs.Outside(rid) {
			delete(candidates, rid)
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// elide marks the candidates F cards name only outside the cone.
func (a *Absences) elide(q db.Querier, candidates map[libfossil.FslID]bool) error {
	rows, err := q.Query(`
		SELECT m.fid, f.name FROM phantom p
		  JOIN mlink m ON m.fid=p.rid JOIN filename f ON f.fnid=m.fnid
		UNION
		SELECT m.pid, f.name FROM phantom p
		  JOIN mlink m ON m.pid=p.rid
		  JOIN filename f ON f.fnid=CASE WHEN m.pfnid>0 THEN m.pfnid ELSE m.fnid END`)
	if err != nil {
		return fmt.Errorf("content.LoadAbsences: references: %w", err)
	}
	out := map[libfossil.FslID]bool{}
	in := map[libfossil.FslID]bool{}
	for rows.Next() {
		var rid libfossil.FslID
		var name string
		if err := rows.Scan(&rid, &name); err != nil {
			rows.Close()
			return fmt.Errorf("content.LoadAbsences: references: %w", err)
		}
		if !candidates[rid] {
			continue
		}
		if a.cone.Covers(name) {
			in[rid] = true
		} else {
			out[rid] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("content.LoadAbsences: references: %w", err)
	}
	for rid := range out {
		if !in[rid] {
			a.class[rid] = Elided
		}
	}
	return nil
}

// BelowHorizon counts the elided phantoms that are check-ins below a
// structural horizon (draft-fossil-horizon-00 §5.4): its boundary hashes and
// the manifests only a tag or cherry-pick names.
func (a *Absences) BelowHorizon() int { return len(a.below) }

// Below reports rid as a check-in below a structural horizon.
func (a *Absences) Below(rid libfossil.FslID) bool { return a.below[rid] }

// Cone is the store cone the classification used, nil for a dense store.
func (a *Absences) Cone() *cone.Cone { return a.cone }

// Class reports why rid is absent, or Held when it is not a phantom.
func (a *Absences) Class(rid libfossil.FslID) Absence {
	if k, ok := a.class[rid]; ok {
		return k
	}
	return Held
}

// Count is the number of phantoms in class k. Held is not counted.
func (a *Absences) Count(k Absence) int {
	if k == Held {
		panic("content.Absences.Count: held artifacts are not counted")
	}
	return a.counts[k]
}
