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
// filled, as a load of the whole store would decide them. s is unchanged.
//
// A load reads raw only what the crosslink tables cannot answer, and which
// check-ins they answer is an induction over the whole store (answerable).
// The extension is that load's decisions for fresh alone, which equal a full
// load's while the decisions for the rest stand. exact is false when one
// may not: the store gained the non-artifact record or saw its first tree; a
// new check-in's mlink rows name an old version the record calls no
// artifact; a fresh artifact is the store's first tree; or a linked
// check-in proves unreadable, which sends a full load to its third round.
// An arrival a held P, B or S card cited changes old decisions too; the
// caller recomputes in full on one before it gets here (§6.2).
func extendReferenceState(
	ctx context.Context, q db.Querier, s *referenceState, fresh []libfossil.FslID,
) (out *referenceState, exact bool, err error) {
	if ctx == nil || q == nil || s == nil {
		panic("content.extendReferenceState: nil context, querier or state")
	}
	ids, err := ridsJSON(fresh)
	if err != nil || ids == "" {
		return s, err == nil, err
	}
	added, err := loadReferenceTablesFor(ctx, q, ids)
	if err != nil || added.present != s.tables.present || added.trees != s.tables.trees {
		return nil, false, err
	}
	in := make(map[libfossil.FslID]bool, len(fresh))
	for _, rid := range fresh {
		in[rid] = true
	}
	if renamed, err := oldFilesNamed(ctx, q, s.tables, ids, in); err != nil || renamed {
		return nil, false, err
	}
	r, err := referencePreflight(ctx, q, defaultReferenceLimits.refs)
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
		present: s.tables.present, trees: s.tables.trees}
	maps.Copy(tables.files, added.files)
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

// oldFilesNamed reports an artifact outside fresh that the non-artifact
// record holds and that only the mlink rows of the check-ins in ids name: a
// load now takes it for file content where s's load read it raw.
func oldFilesNamed(
	ctx context.Context, q db.Querier, t *referenceTables, ids string, fresh map[libfossil.FslID]bool,
) (bool, error) {
	if !t.present {
		return false, nil
	}
	renamed := false
	err := referenceRows(ctx, q, "SELECT rid FROM "+nonArtifactTable+` WHERE rid IN
		(SELECT fid FROM mlink WHERE mid IN (SELECT value FROM json_each(?1))
		UNION SELECT pid FROM mlink WHERE mid IN (SELECT value FROM json_each(?1)))`,
		func(scan func(...any) error) error {
			var rid libfossil.FslID
			err := scan(&rid)
			renamed = renamed || !t.files[rid] && !fresh[rid]
			return err
		}, ids)
	return renamed, err
}
