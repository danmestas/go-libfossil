package manifest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// crosslinkDerivedTables are the tables a Crosslink sweep is responsible for.
// Emptying them puts a fully-built repository back in the state a
// freshly-transferred clone is in immediately before crosslinking.
var crosslinkDerivedTables = []string{
	"event", "plink", "leaf", "mlink", "tagxref", "forumpost",
	"attachment", "backlink", "cherrypick",
}

// TestFossilBinaryReadsCrosslinkedRepo drives the accelerated crosslink path
// end to end and checks two things our own tests cannot: that a repository
// whose relational tables were written by Crosslink -- expanding artifacts
// through the memoizing content cache rather than one full chain walk per
// blob -- is readable by canonical Fossil, and that what Crosslink wrote
// matches what Fossil itself derives from the same blobs.
//
// Method: build a history deep enough that the commit path deltifies it,
// empty the derived tables, run Crosslink over the untouched blobs, hand the
// result to the fossil binary, and finally let `fossil rebuild` re-derive the
// same tables from the same blobs so the two derivations can be compared.
//
// The rebuild runs last here so its output can be diffed against what
// Crosslink already wrote -- not because ordering matters anymore.
// TestCrosslinkAfterFossilRebuild below exercises the opposite order,
// where rebuild drops the on-demand tables (forumpost) before Crosslink
// ever runs.
func TestFossilBinaryReadsCrosslinkedRepo(t *testing.T) {
	bin := testutil.RequireFossilBin(t)

	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("fossil %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	path := filepath.Join(t.TempDir(), "crosslinked.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	incrementalHistory(t, r, 3, 25, 400)
	s := collectStorageStats(t, r)
	if s.deltaEncoded == 0 {
		t.Fatal("no deltas were produced; this test would prove nothing")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	for _, tbl := range crosslinkDerivedTables {
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r2, err := repo.Open(path)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	linked, err := Crosslink(r2)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked == 0 {
		t.Fatal("Crosslink linked nothing")
	}
	if err := r2.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	got := snapshotDerived(t, path)

	integrity := run("test-integrity", "-R", path)
	if !strings.Contains(integrity, "0 errors") {
		t.Fatalf("fossil test-integrity did not report 0 errors:\n%s", integrity)
	}
	if !strings.Contains(integrity, "low-level database integrity-check: ok") {
		t.Fatalf("fossil reported a low-level database problem:\n%s", integrity)
	}

	// timeline reads event and plink, the tables Crosslink just rewrote.
	timeline := run("timeline", "-R", path, "-n", "5")
	if !strings.Contains(timeline, "revision 24") {
		t.Fatalf("fossil timeline does not show the tip check-in:\n%s", timeline)
	}

	if stats := run("rebuild", path, "--stats"); !strings.Contains(stats, "Artifacts:") {
		t.Fatalf("fossil rebuild produced no statistics")
	}

	// Fossil has now re-derived the same tables from the same blobs. The two
	// derivations must agree row for row on the columns compared below.
	//
	// That is narrower than full-table equivalence: the event digest covers
	// objid, type, user and comment but not mtime or tagid, the tagxref
	// digest covers rid/tagid/tagtype/srcid/origid/value but not mtime, and
	// backlink, attachment and cherrypick are emptied above but never
	// compared. tagxref is the highest-risk table here -- it holds the
	// order-sensitive state that visiting candidates in delta-chain order,
	// rather than fossil's own crosslink order, could in principle disturb --
	// so it is included even though this fixture is single-branch and does
	// not exercise tag inheritance across a merge; see
	// TestFossilBinaryReadsCrosslinkedRepoBranchAndMerge for that.
	reference := snapshotDerived(t, path)
	for _, key := range []string{"event", "plink", "leaf", "mlink", "tagxref"} {
		if got[key] != reference[key] {
			t.Errorf("%s differs from what fossil derived\n fossil:    %s\n crosslink: %s",
				key, reference[key], got[key])
		}
	}
}

// TestFossilBinaryReadsCrosslinkedRepoBranchAndMerge is
// TestFossilBinaryReadsCrosslinkedRepo's companion for the topology that
// exercises tag propagation's order-sensitive paths: a feature branch
// diverging from trunk and a merge commit bringing it back. incrementalHistory
// above never branches, so on its own it cannot prove trunk's propagating
// branch tag stops at the feature branch's own declaration, or that a merge
// commit inherits its tag from the primary parent rather than the merged-in
// one.
func TestFossilBinaryReadsCrosslinkedRepoBranchAndMerge(t *testing.T) {
	bin, err := exec.LookPath("fossil")
	if err != nil {
		if os.Getenv("REQUIRE_FOSSIL_BIN") == "1" {
			t.Fatalf("REQUIRE_FOSSIL_BIN=1 but no fossil binary on PATH: %v", err)
		}
		t.Skip("fossil binary not on PATH; cannot verify canonical readability")
	}

	path := filepath.Join(t.TempDir(), "branchmerge.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	branchAndMergeHistory(t, r)
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	for _, tbl := range crosslinkDerivedTables {
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r2, err := repo.Open(path)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	linked, err := Crosslink(r2)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked == 0 {
		t.Fatal("Crosslink linked nothing")
	}
	if err := r2.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	got := snapshotDerived(t, path)

	integrity, err := exec.Command(bin, "test-integrity", "-R", path).CombinedOutput()
	if err != nil {
		t.Fatalf("fossil test-integrity failed: %v\n%s", err, integrity)
	}
	if !strings.Contains(string(integrity), "0 errors") {
		t.Fatalf("fossil test-integrity did not report 0 errors:\n%s", integrity)
	}

	if out, err := exec.Command(bin, "rebuild", path).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}

	// event, plink and tagxref are compared here -- tagxref is this test's
	// reason for existing, since it is the table repairTagPropagation
	// derives from the whole (now branched) plink graph rather than any one
	// artifact, and TestFossilBinaryReadsCrosslinkedRepo's single-branch
	// fixture cannot exercise a tag stopping at its own branch's declaration
	// or a merge inheriting from its primary parent.
	//
	// leaf and mlink are deliberately not compared here: this fixture
	// exposed that both have pre-existing gaps against canonical fossil that
	// predate and are unrelated to this fix (repairLeafTable counts any
	// plink edge, not just primary-parent edges, so a checkin that is only a
	// merge parent -- like branch2 here -- outlives its "leaf" status
	// differently than fossil does; insertCheckinMlinks, similarly
	// untouched by this fix, writes a row for every F-card rather than only
	// the ones that changed relative to the primary parent, which a partial
	// merge such as this one's trunk3/merge pair can trigger for an
	// unchanged file). Both are out of scope for #102/#103 and belong in
	// their own follow-up rather than this branch.
	reference := snapshotDerived(t, path)
	for _, key := range []string{"event", "plink", "tagxref"} {
		if got[key] != reference[key] {
			t.Errorf("%s differs from what fossil derived on a branch+merge history\n fossil:    %s\n crosslink: %s",
				key, reference[key], got[key])
		}
	}
}

// branchAndMergeHistory builds a small trunk/feature-branch/merge topology:
// two trunk commits, a feature branch diverging from the second, two commits
// on that branch, a third trunk commit running in parallel, and a merge
// commit that folds the feature branch back into trunk as a non-primary
// parent. This is deliberately the minimal shape that can distinguish
// "propagating tag stops at its own branch's declaration" and "a merge
// commit's own branch tag comes from its primary parent" from a
// single-branch history, which cannot exercise either.
//
// A Fossil check-in manifest's F-cards list the complete tree, not just what
// changed (CheckinOpts.Files is the full state, mirroring buildCheckinDeck),
// so every commit below re-supplies every file currently in the tree,
// touching only the one this step means to change -- omitting an untouched
// file would delete it instead of leaving it alone.
func branchAndMergeHistory(t *testing.T, r *repo.Repo) libfossil.FslID {
	t.Helper()

	body := func(tag string, rev int) []byte {
		return []byte(fmt.Sprintf("%s revision %d\nthe quick brown fox jumps over the lazy dog\n", tag, rev))
	}
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	clone := func(tree map[string][]byte) map[string][]byte {
		out := make(map[string][]byte, len(tree))
		for k, v := range tree {
			out[k] = v
		}
		return out
	}
	snapshot := func(tree map[string][]byte) []File {
		files := make([]File, 0, len(tree))
		for name, content := range tree {
			files = append(files, File{Name: name, Content: content})
		}
		return files
	}
	commit := func(name, comment string, tree map[string][]byte, parent libfossil.FslID, mergeParents []libfossil.FslID, tags []deck.TagCard, hour int) libfossil.FslID {
		t.Helper()
		rid, _, err := Checkin(r, CheckinOpts{
			Files:        snapshot(tree),
			Comment:      comment,
			User:         "testuser",
			Parent:       parent,
			MergeParents: mergeParents,
			Tags:         tags,
			Time:         base.Add(time.Duration(hour) * time.Hour),
		})
		if err != nil {
			t.Fatalf("Checkin(%s): %v", name, err)
		}
		return rid
	}

	// Each lineage gets its own tree snapshot from the point it diverges, so
	// a change made only on one branch does not leak into a checkin built
	// from a different, concurrently-diverging lineage.
	trunkTree := map[string][]byte{}
	trunkTree["trunk.txt"] = body("trunk", 1)
	trunkTree["shared.txt"] = body("shared", 1)
	trunk1 := commit("trunk1", "trunk revision 1", trunkTree, 0, nil, nil, 0)

	trunkTree = clone(trunkTree)
	trunkTree["trunk.txt"] = body("trunk", 2)
	trunkTree["shared.txt"] = body("shared", 2)
	trunk2 := commit("trunk2", "trunk revision 2", trunkTree, trunk1, nil, nil, 1)

	branchTree := clone(trunkTree)
	branchTree["feature.txt"] = body("feature", 1)
	branchTree["shared.txt"] = body("shared", 3)
	branch1 := commit("branch1", "start feature-x", branchTree, trunk2, nil, []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feature-x"},
		{Type: deck.TagSingleton, Name: "sym-feature-x", UUID: "*"},
	}, 2)

	branchTree = clone(branchTree)
	branchTree["feature.txt"] = body("feature", 2)
	branch2 := commit("branch2", "feature-x revision 2", branchTree, branch1, nil, nil, 3)

	// trunk3 continues from trunk2's own tree, not branch2's -- it must not
	// see feature.txt or branch1/2's shared.txt edit.
	trunkTree = clone(trunkTree)
	trunkTree["trunk.txt"] = body("trunk", 3)
	trunk3 := commit("trunk3", "trunk revision 3, parallel to feature-x", trunkTree, trunk2, nil, nil, 4)

	// Merge commit: primary parent stays on trunk, feature-x rides along as a
	// non-primary merge parent. No explicit tags -- the merge must inherit
	// trunk's propagating branch tag from its primary parent, not feature-x's
	// from the merged-in one. Base the merge tree on trunk3 (keeping trunk's
	// resolution of shared.txt) and fold in feature.txt from the branch.
	mergeTree := clone(trunkTree)
	mergeTree["trunk.txt"] = body("trunk", 4)
	mergeTree["feature.txt"] = body("feature", 2)
	merge := commit("merge", "merge feature-x into trunk", mergeTree, trunk3, []libfossil.FslID{branch2}, nil, 5)

	return merge
}

// TestCrosslinkAfterFossilRebuild is the #103 regression: a repository
// straight out of a canonical `fossil rebuild` has forumpost dropped (this
// history has no forum posts, so canonical never recreates it), and
// Crosslink's candidate query names that table unconditionally. It must
// still succeed, recreating the table the way canonical would if a forum
// artifact showed up.
func TestCrosslinkAfterFossilRebuild(t *testing.T) {
	bin, err := exec.LookPath("fossil")
	if err != nil {
		if os.Getenv("REQUIRE_FOSSIL_BIN") == "1" {
			t.Fatalf("REQUIRE_FOSSIL_BIN=1 but no fossil binary on PATH: %v", err)
		}
		t.Skip("fossil binary not on PATH; cannot verify against canonical rebuild")
	}

	path := filepath.Join(t.TempDir(), "rebuilt.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	incrementalHistory(t, r, 2, 10, 200)
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	// Canonical rebuild runs before Crosslink here -- the order Crosslink
	// could not previously tolerate, since rebuild drops forumpost for a
	// history that never populated it.
	if out, err := exec.Command(bin, "rebuild", path).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	var forumpostExists int
	if err := d.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='forumpost'").Scan(&forumpostExists); err != nil {
		t.Fatalf("check forumpost: %v", err)
	}
	if forumpostExists != 0 {
		t.Fatal("forumpost survived fossil rebuild; this test proves nothing without it gone")
	}
	for _, tbl := range crosslinkDerivedTables {
		if tbl == "forumpost" {
			continue // rebuild dropped it; nothing to clear
		}
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r2, err := repo.Open(path)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r2.Close()
	linked, err := Crosslink(r2)
	if err != nil {
		t.Fatalf("Crosslink after fossil rebuild: %v", err)
	}
	if linked == 0 {
		t.Fatal("Crosslink linked nothing")
	}
}

// snapshotDerived returns a per-table digest of the crosslink-derived tables,
// restricted to the columns Crosslink is responsible for writing.
func snapshotDerived(t *testing.T, path string) map[string]string {
	t.Helper()

	queries := map[string]string{
		"event": `SELECT group_concat(v, '|') FROM
		            (SELECT objid || ':' || type || ':' || coalesce(user,'') || ':' ||
		                    coalesce(comment,'') AS v FROM event ORDER BY objid)`,
		"plink": `SELECT group_concat(v, '|') FROM
		            (SELECT pid || '>' || cid || ':' || isprim AS v FROM plink ORDER BY pid, cid)`,
		"leaf": `SELECT group_concat(rid, '|') FROM (SELECT rid FROM leaf ORDER BY rid)`,
		"mlink": `SELECT group_concat(v, '|') FROM
		            (SELECT mid || ':' || fid || ':' || pid || ':' || fnid || ':' ||
		                    coalesce(pfnid,'') || ':' || coalesce(mperm,'') AS v
		               FROM mlink ORDER BY mid, fnid, fid)`,
		"tagxref": `SELECT group_concat(v, '|') FROM
		            (SELECT rid || ':' || tagid || ':' || tagtype || ':' ||
		                    coalesce(srcid,'') || ':' || coalesce(origid,'') || ':' ||
		                    coalesce(value,'') AS v
		               FROM tagxref ORDER BY rid, tagid)`,
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("snapshotDerived open: %v", err)
	}
	defer d.Close()

	out := make(map[string]string, len(queries))
	for name, q := range queries {
		var v any
		if err := d.QueryRow(q).Scan(&v); err != nil {
			t.Fatalf("snapshotDerived %s: %v", name, err)
		}
		s, ok := v.(string)
		if !ok || s == "" {
			// A NULL digest means the table is empty. Left unset, an empty
			// table would compare equal to another empty table and the
			// comparison would pass without having examined a single row.
			t.Fatalf("snapshotDerived %s: empty digest; the table has no rows "+
				"and the comparison would be vacuous", name)
		}
		out[name] = s
	}
	return out
}
