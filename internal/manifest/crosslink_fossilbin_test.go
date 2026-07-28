package manifest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	mergeRID := branchAndMergeHistory(t, r)
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	direct := snapshotDerived(t, path)
	if executableMlinks := countMlinksWithNonzeroMperm(t, path); executableMlinks == 0 {
		t.Fatal("direct checkin produced no mlink rows with nonzero mperm")
	}
	directAux := countAuxMlinks(t, path, mergeRID)

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

	crosslinked := snapshotDerived(t, path)
	crosslinkedAux := countAuxMlinks(t, path, mergeRID)

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

	canonical := snapshotDerived(t, path)
	canonicalAux := countAuxMlinks(t, path, mergeRID)

	// event, plink and tagxref remain this test's original branch/tag
	// propagation oracle. mlink additionally compares all three derivations:
	// the rows written by direct Checkin, the same artifacts re-crosslinked,
	// and canonical Fossil's rebuild.
	for _, key := range []string{"event", "plink", "tagxref"} {
		if crosslinked[key] != canonical[key] {
			t.Errorf("%s differs from what fossil derived on a branch+merge history\n fossil:    %s\n crosslink: %s",
				key, canonical[key], crosslinked[key])
		}
	}
	if crosslinked["mlink"] != canonical["mlink"] {
		t.Errorf("crosslinked mlink differs from fossil rebuild\n fossil:    %s\n crosslink: %s",
			canonical["mlink"], crosslinked["mlink"])
	}
	if direct["mlink"] != crosslinked["mlink"] {
		t.Errorf("direct mlink differs from crosslink\n direct:    %s\n crosslink: %s",
			direct["mlink"], crosslinked["mlink"])
	}
	if direct["mlink"] != canonical["mlink"] {
		t.Errorf("direct mlink differs from fossil rebuild\n direct: %s\n fossil: %s",
			direct["mlink"], canonical["mlink"])
	}
	if canonicalAux == 0 {
		t.Fatal("fossil rebuild produced no auxiliary mlink rows for merge check-in")
	}
	if crosslinkedAux != canonicalAux {
		t.Errorf("crosslinked auxiliary mlink rows = %d, fossil rebuild = %d",
			crosslinkedAux, canonicalAux)
	}
	if directAux != canonicalAux {
		t.Errorf("direct auxiliary mlink rows = %d, fossil rebuild = %d",
			directAux, canonicalAux)
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
	clone := func(tree map[string]File) map[string]File {
		out := make(map[string]File, len(tree))
		for k, v := range tree {
			out[k] = v
		}
		return out
	}
	snapshot := func(tree map[string]File) []File {
		files := make([]File, 0, len(tree))
		for _, file := range tree {
			files = append(files, file)
		}
		// Deliberately reverse canonical order: direct Checkin must canonicalize
		// before deriving mlink, or this parity test detects order-dependent filename IDs.
		sort.Slice(files, func(i, j int) bool { return deck.Compare(files[i].Name, files[j].Name) > 0 })
		return files
	}
	commit := func(name, comment string, tree map[string]File, parent libfossil.FslID, mergeParents []libfossil.FslID, tags []deck.TagCard, hour int) libfossil.FslID {
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
	trunkTree := map[string]File{
		"trunk.txt":  {Name: "trunk.txt", Content: body("trunk", 1)},
		"shared.txt": {Name: "shared.txt", Content: body("shared", 1)},
		"script.sh":  {Name: "script.sh", Content: []byte("#!/bin/sh\necho branch-and-merge\n"), Perm: "x"},
	}
	trunk1 := commit("trunk1", "trunk revision 1", trunkTree, 0, nil, nil, 0)

	trunkTree = clone(trunkTree)
	trunkTree["trunk.txt"] = File{Name: "trunk.txt", Content: body("trunk", 2)}
	trunkTree["shared.txt"] = File{Name: "shared.txt", Content: body("shared", 2)}
	trunk2 := commit("trunk2", "trunk revision 2", trunkTree, trunk1, nil, nil, 1)

	branchTree := clone(trunkTree)
	branchTree["feature.txt"] = File{Name: "feature.txt", Content: body("feature", 1)}
	branchTree["shared.txt"] = File{Name: "shared.txt", Content: body("shared", 3)}
	branch1 := commit("branch1", "start feature-x", branchTree, trunk2, nil, []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feature-x"},
		{Type: deck.TagSingleton, Name: "sym-feature-x", UUID: "*"},
	}, 2)

	branchTree = clone(branchTree)
	branchTree["feature.txt"] = File{Name: "feature.txt", Content: body("feature", 2)}
	branch2 := commit("branch2", "feature-x revision 2", branchTree, branch1, nil, nil, 3)

	// trunk3 continues from trunk2's own tree, not branch2's -- it must not
	// see feature.txt or branch1/2's shared.txt edit.
	trunkTree = clone(trunkTree)
	trunkTree["trunk.txt"] = File{Name: "trunk.txt", Content: body("trunk", 3)}
	trunk3 := commit("trunk3", "trunk revision 3, parallel to feature-x", trunkTree, trunk2, nil, nil, 4)

	// Merge commit: primary parent stays on trunk, feature-x rides along as a
	// non-primary merge parent. No explicit tags -- the merge must inherit
	// trunk's propagating branch tag from its primary parent, not feature-x's
	// from the merged-in one. Base the merge tree on trunk3 (keeping trunk's
	// resolution of shared.txt) and fold in feature.txt from the branch.
	mergeTree := clone(trunkTree)
	mergeTree["trunk.txt"] = File{Name: "trunk.txt", Content: body("trunk", 4)}
	mergeTree["feature.txt"] = File{Name: "feature.txt", Content: body("feature", 2)}
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

// TestFossilBinaryMlinkParity_DeletionAndRename is issue #157's canonical
// cross-check: a repository the fossil binary itself authored -- so its
// manifests carry a real rename card and a real dropped file -- must yield the
// same mlink rows from our Crosslink as fossil's own rebuild derives from the
// same blobs.
//
// Method: let the fossil binary build a two-commit history whose second commit
// deletes one file and renames-and-edits another, empty the crosslink-derived
// tables, re-derive them with our Crosslink, then run `fossil rebuild` and diff
// the two mlink digests. This is the one check our hand-built decks cannot make:
// the fixture's rename card and deletion are exactly what canonical Fossil
// emits, not what our test helpers approximate.
func TestFossilBinaryMlinkParity_DeletionAndRename(t *testing.T) {
	bin := testutil.RequireFossilBin(t)

	dir := t.TempDir()
	repoPath := filepath.Join(dir, "parity.fossil")
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	runIn := func(wd string, args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = wd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fossil %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	runIn(dir, "init", repoPath)
	runIn(work, "open", repoPath)

	// c1: two tracked files.
	writeFile("alpha.txt", "alpha original\n")
	writeFile("beta.txt", "beta original\n")
	runIn(work, "add", "alpha.txt", "beta.txt")
	runIn(work, "commit", "-m", "c1: add alpha and beta")

	// c2: delete beta, and rename alpha -> gamma while also editing its content.
	// This single child manifest carries both #157 cases: a dropped file and a
	// rename whose pid must trace the pre-rename path.
	runIn(work, "rm", "--hard", "beta.txt")
	runIn(work, "mv", "--hard", "alpha.txt", "gamma.txt")
	writeFile("gamma.txt", "alpha edited after the rename\n")
	runIn(work, "commit", "-m", "c2: drop beta, rename+edit alpha to gamma")

	// Put the repo back in a freshly-transferred clone's pre-crosslink state.
	d, err := db.Open(repoPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	for _, tbl := range crosslinkDerivedTables {
		// A freshly-initialized fossil repo creates some derived tables (e.g.
		// forumpost) lazily, so one may not exist yet. Clearing an absent table
		// is a no-op, not a failure -- Crosslink recreates whatever it needs.
		var exists int
		if err := d.QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", tbl, err)
		}
		if exists == 0 {
			continue
		}
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r2, err := repo.Open(repoPath)
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

	got := snapshotDerived(t, repoPath)

	// Canonical rebuild re-derives mlink from the same blobs; the two derivations
	// must agree row for row on the columns snapshotDerived compares.
	if out, err := exec.Command(bin, "rebuild", repoPath).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}
	reference := snapshotDerived(t, repoPath)

	if got["mlink"] != reference["mlink"] {
		t.Errorf("mlink differs from what fossil derived on a delete+rename history\n fossil:    %s\n crosslink: %s",
			reference["mlink"], got["mlink"])
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
		// pmid and isaux are part of the digest: without them a merge
		// check-in's auxiliary rows -- the per-merge-parent transitions
		// canonical records and this package used to omit entirely -- compare
		// equal to their primary-parent counterparts and the whole class of
		// difference issue #193 was about goes unnoticed.
		"mlink": `SELECT group_concat(v, '|') FROM
		            (SELECT mid || ':' || fid || ':' || pmid || ':' || pid || ':' || fnid || ':' ||
		                    coalesce(pfnid,'') || ':' || coalesce(mperm,'') || ':' || isaux AS v
		               FROM mlink ORDER BY mid, fnid, isaux, pmid, fid)`,
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

func countAuxMlinks(t *testing.T, path string, mid libfossil.FslID) int {
	t.Helper()
	if mid <= 0 {
		panic("countAuxMlinks: mid must be positive")
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("countAuxMlinks open: %v", err)
	}
	defer d.Close()

	var count int
	if err := d.QueryRow(
		"SELECT count(*) FROM mlink WHERE mid=? AND isaux=1 AND pmid>0",
		mid,
	).Scan(&count); err != nil {
		t.Fatalf("countAuxMlinks: %v", err)
	}
	return count
}

func countMlinksWithNonzeroMperm(t *testing.T, path string) int {
	t.Helper()

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("countMlinksWithNonzeroMperm open: %v", err)
	}
	defer d.Close()

	var count int
	if err := d.QueryRow("SELECT count(*) FROM mlink WHERE mperm<>0").Scan(&count); err != nil {
		t.Fatalf("countMlinksWithNonzeroMperm: %v", err)
	}
	return count
}
