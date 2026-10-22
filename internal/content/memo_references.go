package content

import (
	"context"
	"maps"
	"slices"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// extendReferenceState returns s extended by the artifacts fresh names, the
// rids appended since s was loaded and the phantoms s held that have been
// filled, as a load of the whole store would decide them; names is every
// artifact's name now. s is unchanged.
//
// A load reads raw only what the crosslink tables cannot answer, and which
// check-ins they answer is an induction over the whole store (answerable).
// The extension is that load's decisions for fresh alone, which equal a full
// load's while the decisions for the rest stand. exact is false when one
// may not: the store lost the non-artifact record or saw its first tree; a
// version the record calls no artifact and new mlink rows name was read raw
// to some effect; a fresh artifact is the store's first tree; or a linked
// check-in proves unreadable, which sends a full load to its third round.
// The non-artifact record appearing is taken the same way: what it names
// was read raw, and reading file content raw adds nothing.
// An arrival a held P, B or S card cited changes old decisions too; the
// caller recomputes in full on one before it gets here (§6.2).
func extendReferenceState(
	ctx context.Context, q db.Querier, s *referenceState, fresh []libfossil.FslID, names map[libfossil.FslID]string,
) (out *referenceState, exact bool, err error) {
	if ctx == nil || q == nil || s == nil {
		panic("content.extendReferenceState: nil context, querier or state")
	}
	ids, err := ridsJSON(fresh)
	if err != nil || ids == "" {
		return s, err == nil, err
	}
	added, err := loadReferenceTablesFor(ctx, q, ids)
	if err != nil || added.trees != s.tables.trees || s.tables.present && !added.present {
		return nil, false, err
	}
	in := make(map[libfossil.FslID]bool, len(fresh))
	for _, rid := range fresh {
		in[rid] = true
	}
	// The record appearing is every version it names being named anew.
	named := ids
	if !s.tables.present {
		named = ""
	}
	renamed, err := oldFilesNamed(ctx, q, added.present, s.tables.files, named, in)
	if err != nil || readRaw(s.facts, renamed) {
		return nil, false, err
	}
	r, err := referencePreflightFor(ctx, q, defaultReferenceLimits.refs, names, fresh)
	if err != nil {
		return nil, false, err
	}
	r.retainUnreadable = true
	old := s.facts
	f := r.facts
	f.held, f.nodes, f.byUUID = maps.Clone(old.held), maps.Clone(old.nodes), maps.Clone(old.byUUID)
	f.records, f.roots = slices.Clone(old.records), slices.Clone(old.roots)
	f.unreadable, f.spine, f.files = slices.Clone(old.unreadable), slices.Clone(old.spine), slices.Clone(old.files)
	tables := &referenceTables{files: maps.Clone(s.tables.files), linked: maps.Clone(s.tables.linked),
		present: added.present, trees: s.tables.trees}
	maps.Copy(tables.files, added.files)
	for _, rid := range renamed {
		tables.files[rid] = true
	}
	maps.Copy(tables.linked, added.linked)
	isLinked := func(rid libfossil.FslID) bool { _, ok := tables.linked[rid]; return ok }
	if _, err := r.read(func(rid libfossil.FslID) bool {
		return in[rid] && !tables.files[rid] && !isLinked(rid)
	}); err != nil {
		return nil, false, err
	}
	if treeRecords(f.records[len(old.records):]) && !treeRecords(old.records) {
		return nil, false, nil
	}
	answer := tables.answerable(f)
	failed, err := r.read(func(rid libfossil.FslID) bool { return in[rid] && isLinked(rid) && !answer[rid] })
	if err != nil || failed {
		return nil, false, err
	}
	answered := map[libfossil.FslID]bool{}
	for rid := range answer {
		if in[rid] {
			answered[rid] = true
		}
	}
	out = &referenceState{facts: f, tables: tables, seen: maps.Clone(s.seen)}
	if err := f.addLinked(ctx, q, answered, fresh, out.seen); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// treeRecords reports a sub-manifest or an S-bearing artifact among records:
// what makes answerable answer nothing.
func treeRecords(records []coneReferenceRecord) bool {
	for _, record := range records {
		if record.deck.Type == deck.Submanifest || len(record.deck.S) > 0 {
			return true
		}
	}
	return false
}

// oldFilesNamed lists the artifacts outside fresh and files that the
// non-artifact record holds and that the mlink rows of the check-ins in ids
// name, or any mlink row when ids is empty: a load now takes them for file
// content, where the load being extended read them raw.
func oldFilesNamed(
	ctx context.Context, q db.Querier, present bool, files map[libfossil.FslID]bool, ids string,
	fresh map[libfossil.FslID]bool,
) ([]libfossil.FslID, error) {
	if !present {
		return nil, nil
	}
	query, args := "SELECT rid FROM "+nonArtifactTable+" WHERE rid IN (SELECT fid FROM mlink UNION SELECT pid FROM mlink)", []any(nil)
	if ids != "" {
		query = "SELECT rid FROM " + nonArtifactTable + ` WHERE rid IN
			(SELECT fid FROM mlink WHERE mid IN (SELECT value FROM json_each(?1))
			UNION SELECT pid FROM mlink WHERE mid IN (SELECT value FROM json_each(?1)))`
		args = []any{ids}
	}
	var renamed []libfossil.FslID
	err := referenceRows(ctx, q, query, func(scan func(...any) error) error {
		var rid libfossil.FslID
		err := scan(&rid)
		if !files[rid] && !fresh[rid] {
			renamed = append(renamed, rid)
		}
		return err
	}, args...)
	return renamed, err
}

// readRaw reports whether reading one of rids raw added to facts: a record,
// or an unreadable name. Reading file content raw adds neither, so a load
// that skips it decides nothing differently.
func readRaw(facts *ConeReferenceFacts, rids []libfossil.FslID) bool {
	if len(rids) == 0 {
		return false
	}
	added := map[string]bool{}
	for _, record := range facts.records {
		added[record.uuid] = true
	}
	for _, uuid := range facts.unreadable {
		added[uuid] = true
	}
	for _, rid := range rids {
		if added[facts.names[rid]] {
			return true
		}
	}
	return false
}
