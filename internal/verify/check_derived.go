package verify

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// checkDerived verifies consistency of derived tables: event, mlink, plink, tagxref, filename, leaf.
func checkDerived(r *repo.Repo, report *Report) error {
	if r == nil {
		panic("verify: checkDerived: nil repo")
	}
	if report == nil {
		panic("verify: checkDerived: nil report")
	}

	// Parent history, sparse readiness and leaf findings must use one snapshot.
	return r.WithBoundTx(func(bound *repo.Repo) error {
		// A reparent may have removed a child without rechecking its old parent.
		detached := make(map[libfossil.FslID]struct{})
		if err := checkCheckins(bound, report, detached); err != nil {
			return err
		}
		return checkLeaves(bound, report, detached)
	})
}

// treeGapCacheBytes bounds the cache checkCheckins gives treeGap. It holds
// manifests and sub-manifests only, never file content, so it is far smaller
// than an expansion cache; it has not been measured.
const treeGapCacheBytes = 32 << 20

type checkinVerification struct {
	cache    *content.Cache
	cone     *cone.Cone
	refs     *content.ConeReferences
	detached map[libfossil.FslID]struct{}
	history  map[string][][]string
}

// checkCheckins verifies event and plink tables against checkin manifests.
// ExpandAll visits blobs in delta-tree order; stable rid sorting preserves
// the report order without expanding each delta chain independently.
func checkCheckins(r *repo.Repo, report *Report, detached map[libfossil.FslID]struct{}) error {
	if r == nil {
		panic("verify: checkCheckins: nil repo")
	}
	if report == nil {
		panic("verify: checkCheckins: nil report")
	}
	c, err := content.StoreCone(r.DB())
	if err != nil {
		return err
	}
	refs, err := content.LoadConeReferences(context.Background(), r.DB(), c)
	if err != nil {
		return err
	}
	history, err := parentHistory(r.DB())
	if err != nil {
		return err
	}
	found := &Report{}
	// ExpandAll hands each blob over once; the cache serves only treeGap's
	// baseline and sub-manifest reads, which many check-ins share.
	state := &checkinVerification{
		cache: content.NewCache(treeGapCacheBytes), cone: c, refs: refs,
		detached: detached, history: history,
	}
	err = content.ExpandAll(r.DB(), func(rid libfossil.FslID, uuid string, data []byte, err error) error {
		if err != nil {
			// Not expandable - skip silently (corrupt, reported by checkBlobs)
			return nil
		}
		return checkOneCheckin(r, found, rid, uuid, data, state)
	})
	if err != nil {
		return fmt.Errorf("checkCheckins: %w", err)
	}
	sort.SliceStable(found.Issues, func(i, j int) bool {
		return found.Issues[i].RID < found.Issues[j].RID
	})
	for _, issue := range found.Issues {
		report.addIssue(issue)
	}
	// found is a staging report: carry its counters over with its issues.
	report.MissingRefs += found.MissingRefs
	return nil
}

// checkOneCheckin verifies a single blob's derived data if it's a checkin.
// data is the blob's expanded content.
func checkOneCheckin(
	r *repo.Repo, report *Report, rid libfossil.FslID, uuid string, data []byte,
	state *checkinVerification,
) error {
	if r == nil {
		panic("verify: checkOneCheckin: nil repo")
	}
	if report == nil {
		panic("verify: checkOneCheckin: nil report")
	}

	d, err := deck.Parse(data)
	if err != nil {
		// Not a valid manifest - skip silently
		return nil
	}

	if err := collectReparents(r.DB(), d, uuid, state.detached); err != nil {
		return err
	}
	if d.Type != deck.Checkin {
		return nil
	}

	// Elided nodes can leave file derivation incomplete without damaging a
	// cone-complete store. Validate raw references and the covered effective
	// view, then still verify its spine metadata below.
	if missing, gap := treeGap(r, d, rid, state.cache); gap {
		ready, err := manifest.SparseCheckinReady(context.Background(), r.DB(), state.cache, d, state.cone, state.refs)
		if err != nil {
			return err
		}
		if !ready {
			report.MissingRefs++
			report.addIssue(Issue{
				Kind: IssueMissingReference, RID: rid, UUID: uuid, Table: "blob",
				Message: fmt.Sprintf("checkin rid=%d uuid=%s waits on sub-manifest %s", rid, uuid, missing),
			})
			return nil
		}
	}

	// Check event table
	if err := checkEvent(r.DB(), report, rid, uuid); err != nil {
		return err
	}

	parents, err := checkinParents(r.DB(), rid, uuid, d.P, state.history)
	if err != nil {
		return err
	}
	if !slices.Equal(parents, d.P) {
		for _, parentUUID := range d.P {
			if err := checkPlink(r.DB(), report, rid, uuid, parentUUID, true, state.detached); err != nil {
				return err
			}
		}
	}
	for _, parentUUID := range parents {
		if err := checkPlink(r.DB(), report, rid, uuid, parentUUID, false, state.detached); err != nil {
			return err
		}
	}

	return nil
}

// parentHashes follows manifest_reparent_checkin's reparent_abort: a nonempty
// list of full hashes, with exactly one separator between hashes. Malformed
// values leave the previous ancestry untouched.
func parentHashes(value string) []string {
	value, _, _ = strings.Cut(value, "\x00")
	var parents []string
	for len(value) > 0 {
		i := 0
		for i < len(value) && value[i] > ' ' {
			i++
		}
		hash := strings.ToLower(value[:i])
		if len(hash) != 40 && len(hash) != 64 {
			return nil
		}
		for _, c := range hash {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return nil
			}
		}
		parents = append(parents, hash)
		if i == len(value) {
			break
		}
		value = value[i+1:]
	}
	return parents
}

// parentHistory retains the possible earlier singleton overrides. A cancelled
// or malformed parent tag cannot tell us which earlier override had reached a
// crosslink-end pass, but the artifacts bound the possible retained ancestry.
func parentHistory(q db.Querier) (map[string][][]string, error) {
	var count int
	if err := q.QueryRow(`SELECT count(*) FROM tagxref JOIN tag USING(tagid)
	 WHERE tagname='parent'`).Scan(&count); err != nil {
		return nil, fmt.Errorf("parentHistory tags: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	history := make(map[string][][]string)
	// Preserve the feature's delta-tree-order scan rather than repeatedly
	// expanding shared chains through a bounded per-artifact cache.
	err := content.ExpandAll(q, func(_ libfossil.FslID, uuid string, data []byte, err error) error {
		if err != nil {
			return nil // Unreadable blobs are reported by checkBlobs.
		}
		d, err := deck.Parse(data)
		if err != nil {
			return nil
		}
		for _, tag := range d.T {
			if tag.Name != "parent" || tag.Type != deck.TagSingleton {
				continue
			}
			parents := parentHashes(tag.Value)
			if len(parents) == 0 {
				continue
			}
			target := tag.UUID
			if target == "*" {
				target = uuid
			}
			history[target] = append(history[target], parents)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("parentHistory blobs: %w", err)
	}
	return history, nil
}

func checkinParents(q db.Querier, rid libfossil.FslID, uuid string, original []string, history map[string][][]string) ([]string, error) {
	var tagtype int
	var value sql.NullString
	err := q.QueryRow(`SELECT tagtype, value FROM tagxref JOIN tag USING(tagid)
	 WHERE tagname='parent' AND rid=?`, rid).Scan(&tagtype, &value)
	if err == sql.ErrNoRows {
		return original, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkinParents tag: %w", err)
	}
	if parents := parentHashes(value.String); tagtype == 1 && len(parents) > 0 {
		return parents, nil
	}
	// A cancel, propagating tag, or malformed singleton does not rewrite
	// ancestry. Accept a retained override only if every parent link survives.
	for _, parents := range history[uuid] {
		complete := true
		for _, parent := range parents {
			var count int
			err := q.QueryRow(`SELECT count(*) FROM plink JOIN blob ON blob.rid=plink.pid
			 WHERE cid=? AND uuid=?`, rid, parent).Scan(&count)
			if err != nil {
				return nil, fmt.Errorf("checkinParents retained link: %w", err)
			}
			complete = complete && count > 0
		}
		if complete {
			return parents, nil
		}
	}
	return original, nil
}

// collectReparents adds to detached each parent that a parent tag in d names
// and its target's plink does not hold. A later parent tag replaces an
// earlier one's tagxref row, so only the artifacts still record a parent an
// earlier reparent linked and a later one dropped.
func collectReparents(q db.Querier, d *deck.Deck, uuid string, detached map[libfossil.FslID]struct{}) error {
	if q == nil {
		panic("verify: collectReparents: nil querier")
	}
	if d == nil {
		panic("verify: collectReparents: nil deck")
	}
	if detached == nil {
		panic("verify: collectReparents: nil detached")
	}
	for _, t := range d.T {
		if t.Name != "parent" || t.Type != deck.TagSingleton {
			continue
		}
		target := t.UUID
		if target == "*" {
			target = uuid
		}
		cid, ok := blob.Exists(q, target)
		if !ok {
			continue
		}
		for _, parent := range parentHashes(t.Value) {
			pid, ok := blob.Exists(q, parent)
			if !ok {
				continue
			}
			var count int
			err := q.QueryRow("SELECT COUNT(*) FROM plink WHERE pid = ? AND cid = ?", pid, cid).Scan(&count)
			if err != nil {
				return fmt.Errorf("collectReparents query: %w", err)
			}
			if count == 0 {
				detached[pid] = struct{}{}
			}
		}
	}
	return nil
}

// checkEvent verifies that a checkin has a corresponding event row.
func checkEvent(q db.Querier, report *Report, rid libfossil.FslID, uuid string) error {
	if q == nil {
		panic("verify: checkEvent: nil querier")
	}
	if report == nil {
		panic("verify: checkEvent: nil report")
	}

	var count int
	err := q.QueryRow("SELECT COUNT(*) FROM event WHERE objid = ?", rid).Scan(&count)
	if err != nil {
		return fmt.Errorf("checkEvent query: %w", err)
	}

	if count == 0 {
		report.addIssue(Issue{
			Kind:    IssueEventMissing,
			RID:     rid,
			UUID:    uuid,
			Table:   "event",
			Message: fmt.Sprintf("checkin rid=%d uuid=%s missing event row", rid, uuid),
		})
	}

	return nil
}

// checkPlink verifies that a parent relationship has a corresponding plink row,
// adding a parent that lacks one to detached. A missing row is an issue only
// when the child is not reparented.
func checkPlink(
	q db.Querier, report *Report, cid libfossil.FslID, childUUID string, parentUUID string,
	reparented bool, detached map[libfossil.FslID]struct{},
) error {
	if q == nil {
		panic("verify: checkPlink: nil querier")
	}
	if report == nil {
		panic("verify: checkPlink: nil report")
	}
	if detached == nil {
		panic("verify: checkPlink: nil detached")
	}

	pid, ok := blob.Exists(q, parentUUID)
	if !ok {
		// Parent doesn't exist in blob table - might be phantom
		return nil
	}

	var count int
	err := q.QueryRow("SELECT COUNT(*) FROM plink WHERE pid = ? AND cid = ?", pid, cid).Scan(&count)
	if err != nil {
		return fmt.Errorf("checkPlink query: %w", err)
	}

	if count == 0 {
		detached[pid] = struct{}{}
		if reparented {
			return nil
		}
		report.addIssue(Issue{
			Kind:    IssuePlinkMissing,
			RID:     cid,
			UUID:    childUUID,
			Table:   "plink",
			Message: fmt.Sprintf("checkin rid=%d uuid=%s missing plink to parent %s", cid, childUUID, parentUUID),
		})
	}

	return nil
}

// checkLeaves reports the leaf-table errors leaf.Audit can decide. Section 7.3
// lets the table depend on the order artifacts arrived in, so Verify does not
// recompute one expected table: it reports a row that a recheck has removed
// and a tagged check-in that a recheck has inserted, and a row no recheck
// writes. detached are the parents a reparent may have taken a child from, as
// checkPlink and collectReparents found them.
func checkLeaves(r *repo.Repo, report *Report, detached map[libfossil.FslID]struct{}) error {
	if r == nil {
		panic("verify: checkLeaves: nil repo")
	}
	if report == nil {
		panic("verify: checkLeaves: nil report")
	}

	f, err := leaf.Audit(r.DB(), detached)
	if err != nil {
		return fmt.Errorf("checkLeaves: %w", err)
	}
	for _, rid := range f.Stale {
		report.addIssue(Issue{
			Kind:    IssueLeafIncorrect,
			RID:     rid,
			Table:   "leaf",
			Message: fmt.Sprintf("leaf rid=%d has a same-branch child that carries a branch tag", rid),
		})
	}
	for _, rid := range f.Stray {
		report.addIssue(Issue{
			Kind:    IssueLeafIncorrect,
			RID:     rid,
			Table:   "leaf",
			Message: fmt.Sprintf("leaf rid=%d is no check-in a leaf recheck reaches", rid),
		})
	}
	for _, rid := range f.Missing {
		report.addIssue(Issue{
			Kind:    IssueLeafIncorrect,
			RID:     rid,
			Table:   "leaf",
			Message: fmt.Sprintf("leaf missing rid=%d, which carries a branch tag and has no same-branch child", rid),
		})
	}
	return nil
}

// treeGap reports the sub-manifest a check-in's tree -- its own, or its
// baseline's -- is waiting on, when that is why its file set cannot be
// listed. Only a tree can be waiting on one, so a flat manifest is answered
// without listing it.
func treeGap(r *repo.Repo, d *deck.Deck, rid libfossil.FslID, cache *content.Cache) (string, bool) {
	if len(d.S) == 0 && d.B == "" {
		return "", false
	}
	_, err := manifest.ListFilesCached(r, rid, cache)
	return manifest.MissingSubmanifest(err)
}
