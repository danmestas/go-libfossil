package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// nonArtifactTable is the crosslinker's record (internal/manifest) of blobs
// it expanded and found to be no artifact: either their bytes do not parse,
// or they parse as a card deck an artifact's required cards do not fit. A
// sub-manifest is the second kind, so the record alone does not prove a
// blob is file content; mlink naming it as a file version as well does.
const nonArtifactTable = "crosslink_nonartifact_v2"

// seenSubmanifestSetting is manifest.SeenSubmanifestSetting, set once any
// S-bearing artifact or sub-manifest has been parsed in this repository.
// The crosslinker records no tree, so with it set no check-in is answered
// from the tables.
const seenSubmanifestSetting = "seen-submanifest"

// linkedCheckin is a check-in the crosslinker linked completely: it has an
// event row, no orphan row, and a primary parent edge, so its mlink rows
// are the file transitions from that parent, and its plink, cherrypick and
// tagxref rows are its P, B, Q and T references.
type linkedCheckin struct {
	primary  libfossil.FslID
	baseline libfossil.FslID // 0 for a baseline manifest
}

// referenceTables are the crosslink facts that spare classification from
// reading file content and most manifests.
type referenceTables struct {
	files  map[libfossil.FslID]bool // Examined, no artifact, named by mlink.
	linked map[libfossil.FslID]linkedCheckin
	// present is whether the store has the non-artifact record, and trees
	// whether it has seen a tree, which makes it answer nothing.
	present, trees bool
}

const linkedCheckinSQL = `SELECT e.objid, p.pid, coalesce(p.baseid,0)
	FROM event e JOIN blob b ON b.rid=e.objid AND b.size>=0
	JOIN plink p ON p.cid=e.objid AND p.isprim
	WHERE e.type='ci' AND NOT EXISTS(SELECT 1 FROM orphan o WHERE o.rid=e.objid)`

func loadReferenceTables(ctx context.Context, q db.Querier) (*referenceTables, error) {
	return loadReferenceTablesFor(ctx, q, "")
}

// loadReferenceTablesFor is loadReferenceTables for the rids of the JSON
// array ids alone, or for every rid when ids is empty.
func loadReferenceTablesFor(ctx context.Context, q db.Querier, ids string) (*referenceTables, error) {
	t := &referenceTables{
		files: map[libfossil.FslID]bool{}, linked: map[libfossil.FslID]linkedCheckin{},
	}
	if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master
		WHERE type='table' AND name=?), EXISTS(SELECT 1 FROM config WHERE name=?)`,
		nonArtifactTable, seenSubmanifestSetting).Scan(&t.present, &t.trees); err != nil {
		return nil, fmt.Errorf("sparse preparation: %s: %w", nonArtifactTable, err)
	}
	if t.present {
		query, args := restrictTo("SELECT rid FROM "+nonArtifactTable+
			" WHERE rid IN (SELECT fid FROM mlink UNION SELECT pid FROM mlink)%s", "rid", ids)
		err := referenceRows(ctx, q, query, func(scan func(...any) error) error {
			var rid libfossil.FslID
			err := scan(&rid)
			t.files[rid] = true
			return err
		}, args...)
		if err != nil {
			return nil, err
		}
	}
	if t.trees {
		return t, nil
	}
	duplicate := map[libfossil.FslID]bool{}
	query, args := restrictTo(linkedCheckinSQL+"%s", "e.objid", ids)
	err := referenceRows(ctx, q, query, func(scan func(...any) error) error {
		var rid libfossil.FslID
		var c linkedCheckin
		err := scan(&rid, &c.primary, &c.baseline)
		if _, seen := t.linked[rid]; seen {
			duplicate[rid] = true // Two primary parents: not a transition to trust.
		}
		t.linked[rid] = c
		return err
	}, args...)
	for rid := range duplicate {
		delete(t.linked, rid)
	}
	return t, err
}

// referenceRows scans every row of query; each row runs ctx's check first.
func referenceRows(
	ctx context.Context, q db.Querier, query string, row func(scan func(...any) error) error, args ...any,
) error {
	rows, err := q.Query(query, args...)
	if err != nil {
		return fmt.Errorf("sparse preparation: %w", err)
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rows.Close())
		}
		if err := row(rows.Scan); err != nil {
			return errors.Join(fmt.Errorf("sparse preparation: %w", err), rows.Close())
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// answerable returns the linked check-ins whose raw F cards the tables can
// stand in for. mlink records a check-in's files only where they differ
// from its primary parent, and a delta manifest's only where its own cards
// say so; every other file of its tree is in the parent's tree or the
// baseline's. So the rows reproduce every F card exactly when both of those
// trees are themselves reproduced: by the tables, or by raw cards whose
// baseline chain is (covered). Trees (S cards) are not crosslinked, so a
// store that holds one answers nothing from the tables. Raw records come
// from the first round, which read everything the tables cannot answer.
func (t *referenceTables) answerable(facts *ConeReferenceFacts) map[libfossil.FslID]bool {
	raw := map[libfossil.FslID]string{} // A read check-in's B card.
	for _, record := range facts.records {
		if record.deck.Type == deck.Submanifest || len(record.deck.S) > 0 {
			return map[libfossil.FslID]bool{}
		}
		if record.deck.Type == deck.Checkin {
			raw[facts.held[record.uuid]] = record.deck.B
		}
	}
	rids := make(map[string]libfossil.FslID, len(t.linked)+len(raw))
	for rid := range t.linked {
		rids[facts.names[rid]] = rid
	}
	for rid := range raw {
		rids[facts.names[rid]] = rid
	}
	baseline := func(rid libfossil.FslID) (libfossil.FslID, bool) {
		if c, ok := t.linked[rid]; ok {
			return c.baseline, true
		}
		b, ok := raw[rid]
		if !ok || b == "" {
			return 0, ok
		}
		base, ok := rids[b]
		return base, ok
	}
	answer := map[libfossil.FslID]bool{}
	for rid, c := range t.linked {
		if covered(c.primary, baseline) && covered(rid, baseline) {
			answer[rid] = true
		}
	}
	return answer
}

// covered reports whether rid is a check-in whose whole tree is reproduced
// once its own cards are: a baseline manifest, or a delta manifest over a
// covered baseline. A baseline is a baseline manifest in every artifact the
// crosslinker accepts, so the chain is short; an unknown or overlong one is
// not covered.
func covered(rid libfossil.FslID, baseline func(libfossil.FslID) (libfossil.FslID, bool)) bool {
	for depth := 0; depth < coneReferenceDepth; depth++ {
		base, ok := baseline(rid)
		if !ok {
			return false
		}
		if base == 0 {
			return true
		}
		rid = base
	}
	return false
}

// linkedSpineSQL lists each linked check-in's references: P, B, Q targets
// and T targets. An absent parent or tag target still has a row, against a
// phantom, as an absent cherrypick target has none, exactly as the raw
// cards name it only by hash. Each query's %s is where the source column is
// restricted, or nothing.
var linkedSpineSQL = []struct{ query, src string }{
	{"SELECT cid, pid FROM plink%s", "cid"},
	{"SELECT cid, baseid FROM plink WHERE baseid IS NOT NULL%s", "cid"},
	{"SELECT childid, parentid FROM cherrypick%s", "childid"},
	{"SELECT srcid, rid FROM tagxref WHERE srcid>0%s", "srcid"},
}

// restrictTo spells query with src limited to the rids of the JSON array
// bound as ?1, or unrestricted when ids is empty, with the arguments that go
// with it.
func restrictTo(query, src, ids string) (string, []any) {
	if ids == "" {
		return fmt.Sprintf(query, ""), nil
	}
	join := " WHERE "
	if strings.Contains(query, " WHERE ") {
		join = " AND "
	}
	return fmt.Sprintf(query, join+src+" IN (SELECT value FROM json_each(?1))"), []any{ids}
}

// addLinked records for the linked check-ins what their raw cards would:
// each check-in itself, its references, and every (name, file) pair of its
// tree, from the mlink rows the induction in answerable relies on. only,
// when set, are the rids the tables are read for, a superset of linked; seen
// holds the pairs already recorded, and gains these.
func (facts *ConeReferenceFacts) addLinked(
	ctx context.Context, q db.Querier, linked map[libfossil.FslID]bool, only []libfossil.FslID,
	seen map[[2]int64]bool,
) error {
	if len(linked) == 0 {
		return nil
	}
	ids, err := ridsJSON(only)
	if err != nil {
		return err
	}
	for rid := range linked {
		facts.spine = append(facts.spine, facts.names[rid])
	}
	for _, spine := range linkedSpineSQL {
		query, args := restrictTo(spine.query, spine.src, ids)
		err := referenceRows(ctx, q, query, func(scan func(...any) error) error {
			var src, dst libfossil.FslID
			err := scan(&src, &dst)
			if uuid := facts.names[dst]; linked[src] && uuid != "" {
				facts.spine = append(facts.spine, uuid)
			}
			return err
		}, args...)
		if err != nil {
			return err
		}
	}
	return facts.addLinkedFiles(ctx, q, linked, ids, seen)
}

func (facts *ConeReferenceFacts) addLinkedFiles(
	ctx context.Context, q db.Querier, linked map[libfossil.FslID]bool, ids string, seen map[[2]int64]bool,
) error {
	names := map[int64]string{}
	query, args := "SELECT fnid, name FROM filename", []any(nil)
	if ids != "" {
		query += " WHERE fnid IN (SELECT fnid FROM mlink WHERE mid IN (SELECT value FROM json_each(?1)))"
		args = []any{ids}
	}
	err := referenceRows(ctx, q, query, func(scan func(...any) error) error {
		var fnid int64
		var name string
		err := scan(&fnid, &name)
		names[fnid] = name
		return err
	}, args...)
	if err != nil {
		return err
	}
	query, args = restrictTo("SELECT mid, fnid, fid FROM mlink WHERE fid>0%s", "mid", ids)
	return referenceRows(ctx, q, query,
		func(scan func(...any) error) error {
			var mid libfossil.FslID
			var key [2]int64
			if err := scan(&mid, &key[0], &key[1]); err != nil {
				return err
			}
			uuid := facts.names[libfossil.FslID(key[1])]
			if !linked[mid] || seen[key] || uuid == "" {
				return nil
			}
			if len(seen) >= coneReferenceNodes {
				return fmt.Errorf("sparse preparation: linked file budget exceeded")
			}
			seen[key] = true
			facts.files = append(facts.files, deck.FileCard{Name: names[key[0]], UUID: uuid})
			return nil
		}, args...)
}

// ridsJSON spells rids as a JSON array for json_each, "" for none. A rid is
// an integer, so the spelling carries nothing a query could misread.
func ridsJSON(rids []libfossil.FslID) (string, error) {
	if len(rids) == 0 {
		return "", nil
	}
	data, err := json.Marshal(rids)
	if err != nil {
		return "", fmt.Errorf("sparse preparation: %w", err)
	}
	return string(data), nil
}
