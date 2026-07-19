package libfossil_test

// Repro for: `fossil ls -R <repo> -r <branch-tip-rev>` returns EMPTY for
// commits written via libfossil's manifest.Checkin, even though
// `fossil open <repo> <rev>` materializes the files correctly.
//
// Surfaced by bones #283 (synthetic-slot apply path). bones works around
// it today by walking the open'd checkout dir; this test is the minimal
// repro the libfossil maintainer can iterate on.
//
// Prereqs:
// - /opt/homebrew/bin/fossil (or `fossil` in PATH), Fossil 2.28
//
// Run:
//   go test -run TestLsBranchTipReturnsEmpty -v ./...

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/branch"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func fossilBin(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/opt/homebrew/bin/fossil", "/usr/local/bin/fossil"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("fossil"); err == nil {
		return p
	}
	t.Skip("fossil binary not found in PATH")
	return ""
}

// runFossilLs returns the file paths printed by `fossil ls -R repo -r rev`
// (one path per line, blanks discarded), plus the raw stderr for diagnostics.
func runFossilLs(t *testing.T, fossil, repoPath, rev string) (paths []string, stderr string) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.Command(fossil, "ls", "-R", repoPath, "-r", rev)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		t.Logf("fossil ls (rev=%s) exit error: %v stderr=%q", rev, err, stderrBuf.String())
	}
	for _, line := range strings.Split(stdoutBuf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		paths = append(paths, line)
	}
	sort.Strings(paths)
	return paths, stderrBuf.String()
}

// runFossilOpenAndWalk opens repo @ rev into a temp dir, walks regular
// files, returns relative paths (excluding fossil's private state).
func runFossilOpenAndWalk(t *testing.T, fossil, repoPath, rev string) []string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(fossil, "open", repoPath, rev, "--workdir", dir)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fossil open: %v\n%s", err, out)
	}
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		base := filepath.Base(rel)
		if base == ".fslckout" || base == "_FOSSIL_" || base == ".fos" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, _ := d.Info()
		if info != nil && info.Mode().IsRegular() {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk checkout: %v", err)
	}
	sort.Strings(paths)
	return paths
}

// dumpRevSchema runs the SQL queries fossil's `ls -r` is believed to issue,
// so the diff between libfossil-written rows and fossil-binary-written rows
// is visible in the test log.
func dumpRevSchema(t *testing.T, fossil, repoPath, rev string) {
	t.Helper()
	queries := []struct {
		label string
		sql   string
	}{
		{"resolve symbol via tagxref", fmt.Sprintf(
			`SELECT t.tagid, t.tagname, x.rid, x.tagtype, x.value FROM tag t JOIN tagxref x USING(tagid) WHERE t.tagname IN ('sym-%s','branch') ORDER BY t.tagname, x.rid;`, rev)},
		{"event for rev (lookup by rid via tagxref)", fmt.Sprintf(
			`SELECT e.objid, e.type, e.user, e.comment FROM event e WHERE e.objid IN (SELECT x.rid FROM tagxref x JOIN tag t USING(tagid) WHERE t.tagname='sym-%s') ORDER BY e.mtime DESC LIMIT 5;`, rev)},
		{"mlink rows for tip", fmt.Sprintf(
			`SELECT m.mid, m.fid, m.pmid, m.pid, m.fnid, f.name FROM mlink m LEFT JOIN filename f USING(fnid) WHERE m.mid IN (SELECT x.rid FROM tagxref x JOIN tag t USING(tagid) WHERE t.tagname='sym-%s') ORDER BY f.name;`, rev)},
		{"filename table size", `SELECT count(*) FROM filename;`},
		{"mlink count", `SELECT count(*) FROM mlink;`},
		{"event count", `SELECT count(*) FROM event WHERE type='ci';`},
	}
	for _, q := range queries {
		var out bytes.Buffer
		cmd := exec.Command(fossil, "sql", "-R", repoPath, q.sql)
		cmd.Stdout = &out
		cmd.Stderr = &out
		_ = cmd.Run()
		t.Logf("[SQL %s]\n%s", q.label, strings.TrimSpace(out.String()))
	}
}

// TestLsBranchTipReturnsEmpty is the regression test documenting the bug.
// EXPECTED: ls output equals walked checkout. ACTUAL: ls returns empty.
//
// Fixed by libfossil#29: insertCheckinMlinks at
// internal/manifest/crosslink.go left the mlink parent columns (pmid, pid)
// NULL, and fossil's `compute_fileage` SQL filters mlink rows on
// `mlink.fid != mlink.pid` — `fid != NULL` is NULL (not TRUE), so the rows
// were dropped and `fossil ls -r` returned empty for any commit that
// arrived over the wire.
//
// To reproduce, run:
//
//	go test -run TestLsBranchTipReturnsEmpty -v ./...
func TestLsBranchTipReturnsEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test (uses fossil binary)")
	}
	fossil := fossilBin(t)

	t.Run("trunk_tip_rev_uuid", func(t *testing.T) {
		repoPath := filepath.Join(t.TempDir(), "trunk.fossil")
		r, err := repo.Create(repoPath, "test-user", simio.CryptoRand{}, "")
		if err != nil {
			t.Fatalf("repo.Create: %v", err)
		}
		_, uuid, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: []manifest.File{
				{Name: "hello.txt", Content: []byte("hello\n")},
				{Name: "dir/world.txt", Content: []byte("world\n")},
			},
			Comment: "trunk-only commit",
			User:    "test-user",
			Time:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("manifest.Checkin: %v", err)
		}
		// Triggers PR #28 WAL checkpoint on Close.
		if err := r.Close(); err != nil {
			t.Fatalf("repo.Close: %v", err)
		}

		dumpRevSchema(t, fossil, repoPath, uuid)
		lsPaths, _ := runFossilLs(t, fossil, repoPath, uuid)
		walkPaths := runFossilOpenAndWalk(t, fossil, repoPath, uuid)
		t.Logf("trunk uuid=%s\n  fossil ls -r:  %v\n  walk(open):   %v", uuid, lsPaths, walkPaths)
		if !equalSorted(lsPaths, walkPaths) {
			t.Errorf("trunk: fossil ls disagrees with fossil open\n  ls   = %v\n  walk = %v", lsPaths, walkPaths)
		}
	})

	t.Run("branch_tip_by_branch_name", func(t *testing.T) {
		repoPath := filepath.Join(t.TempDir(), "branch.fossil")
		r, err := repo.Create(repoPath, "test-user", simio.CryptoRand{}, "")
		if err != nil {
			t.Fatalf("repo.Create: %v", err)
		}
		// Trunk commit first (branch.Create needs a parent).
		trunkRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "root.txt", Content: []byte("root\n")}},
			Comment: "trunk seed",
			User:    "test-user",
			Time:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("trunk Checkin: %v", err)
		}
		_, branchUUID, err := branch.Create(r, branch.CreateOpts{
			Name:   "feature-x",
			Parent: trunkRid,
			User:   "test-user",
			Time:   time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("branch.Create: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("repo.Close: %v", err)
		}

		// Symbolic name path (this is what bones uses).
		dumpRevSchema(t, fossil, repoPath, "feature-x")
		lsByName, lsNameStderr := runFossilLs(t, fossil, repoPath, "feature-x")
		// UUID path — bypasses tagxref symbol resolution.
		lsByUUID, _ := runFossilLs(t, fossil, repoPath, branchUUID)
		walkByName := runFossilOpenAndWalk(t, fossil, repoPath, "feature-x")

		t.Logf("branch tip uuid=%s", branchUUID)
		t.Logf("  fossil ls -r feature-x:    %v  stderr=%q", lsByName, lsNameStderr)
		t.Logf("  fossil ls -r <uuid>:       %v", lsByUUID)
		t.Logf("  walk(open feature-x):      %v", walkByName)

		if !equalSorted(lsByName, walkByName) {
			t.Errorf("BUG: fossil ls -r <branch-name> empty/wrong vs walk\n  ls(name) = %v\n  walk     = %v", lsByName, walkByName)
		}
		if !equalSorted(lsByUUID, walkByName) {
			t.Errorf("BUG: fossil ls -r <uuid> empty/wrong vs walk\n  ls(uuid) = %v\n  walk     = %v", lsByUUID, walkByName)
		}
	})

	t.Run("trunk_tip_via_xfer_crosslink", func(t *testing.T) {
		// Same Crosslink ingestion path but on TRUNK with no parent.
		// Pinpoints whether the bug requires a parent (pid/pmid) to fire,
		// or whether trunk-via-Crosslink also breaks.
		repoPath := filepath.Join(t.TempDir(), "trunk-xfer.fossil")
		r, err := repo.Create(repoPath, "test-user", simio.CryptoRand{}, "")
		if err != nil {
			t.Fatalf("repo.Create: %v", err)
		}
		fileContent := []byte("trunk-via-xfer\n")
		fileUUID := hash.SHA1(fileContent)
		if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
			t.Fatalf("blob.Store(file): %v", err)
		}
		d := &deck.Deck{
			Type: deck.Checkin,
			C:    "first checkin via xfer",
			D:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			F:    []deck.FileCard{{Name: "ingested.txt", UUID: fileUUID}},
			U:    deck.Str("test-user"),
			T: []deck.TagCard{
				{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
				{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
			},
		}
		rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
			if uuid == fileUUID {
				return fileContent, nil
			}
			return nil, fmt.Errorf("unknown uuid: %s", uuid)
		})
		if err != nil {
			t.Fatalf("ComputeR: %v", err)
		}
		d.R = rHash
		manifestBytes, err := d.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		_, tipUUID, err := blob.Store(r.DB(), manifestBytes)
		if err != nil {
			t.Fatalf("blob.Store(manifest): %v", err)
		}
		if _, err := manifest.Crosslink(r); err != nil {
			t.Fatalf("Crosslink: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("repo.Close: %v", err)
		}

		dumpRevSchema(t, fossil, repoPath, "trunk")
		lsByName, _ := runFossilLs(t, fossil, repoPath, "trunk")
		lsByUUID, _ := runFossilLs(t, fossil, repoPath, tipUUID)
		walk := runFossilOpenAndWalk(t, fossil, repoPath, tipUUID)
		t.Logf("trunk-xfer tip uuid=%s\n  ls(name) = %v\n  ls(uuid) = %v\n  walk     = %v", tipUUID, lsByName, lsByUUID, walk)
		if !equalSorted(lsByName, walk) {
			t.Errorf("BUG (trunk-via-xfer): ls(name) != walk")
		}
		if !equalSorted(lsByUUID, walk) {
			t.Errorf("BUG (trunk-via-xfer): ls(uuid) != walk")
		}
	})

	t.Run("branch_tip_via_xfer_crosslink", func(t *testing.T) {
		// This is the bones path: an external client (libfossil acting as
		// a leaf) hands the hub a manifest blob + file blob, and the hub
		// writes the relational rows via manifest.Crosslink (the xfer
		// ingestion sweep). bones #283 surfaced because `fossil ls -r
		// <branch-tip>` came back empty against this exact state.
		repoPath := filepath.Join(t.TempDir(), "xfer.fossil")
		r, err := repo.Create(repoPath, "test-user", simio.CryptoRand{}, "")
		if err != nil {
			t.Fatalf("repo.Create: %v", err)
		}

		// 1. Establish a trunk seed via the direct-commit path so we have
		//    a parent for the branched checkin.
		trunkRid, trunkUUID, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "root.txt", Content: []byte("root\n")}},
			Comment: "trunk seed",
			User:    "test-user",
			Time:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("trunk Checkin: %v", err)
		}
		_ = trunkRid

		// 2. Build a branched checkin manifest by hand and ingest via
		//    blob.Store + Crosslink — exactly what HandleSync does for an
		//    inbound C-card it has never seen before.
		fileContent := []byte("ingested via xfer\n")
		fileUUID := hash.SHA1(fileContent)
		// File blob first (so Crosslink resolves it).
		if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
			t.Fatalf("blob.Store(file): %v", err)
		}
		d := &deck.Deck{
			Type: deck.Checkin,
			C:    "branched commit via xfer",
			D:    time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
			P:    []string{trunkUUID},
			F:    []deck.FileCard{{Name: "ingested.txt", UUID: fileUUID}},
			U:    deck.Str("test-user"),
			T: []deck.TagCard{
				{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feature-xfer"},
				{Type: deck.TagPropagating, Name: "sym-feature-xfer", UUID: "*"},
				{Type: deck.TagCancel, Name: "sym-trunk", UUID: "*"},
			},
		}
		rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
			if uuid == fileUUID {
				return fileContent, nil
			}
			return nil, fmt.Errorf("ComputeR unknown uuid: %s", uuid)
		})
		if err != nil {
			t.Fatalf("ComputeR: %v", err)
		}
		d.R = rHash
		manifestBytes, err := d.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		_, branchTipUUID, err := blob.Store(r.DB(), manifestBytes)
		if err != nil {
			t.Fatalf("blob.Store(manifest): %v", err)
		}

		linked, err := manifest.Crosslink(r)
		if err != nil {
			t.Fatalf("Crosslink: %v", err)
		}
		t.Logf("Crosslink linked %d manifest(s)", linked)

		if err := r.Close(); err != nil {
			t.Fatalf("repo.Close: %v", err)
		}

		dumpRevSchema(t, fossil, repoPath, "feature-xfer")
		lsByName, lsNameStderr := runFossilLs(t, fossil, repoPath, "feature-xfer")
		lsByUUID, lsUUIDStderr := runFossilLs(t, fossil, repoPath, branchTipUUID)
		walkByName := runFossilOpenAndWalk(t, fossil, repoPath, "feature-xfer")

		t.Logf("xfer-ingested branch tip uuid=%s", branchTipUUID)
		t.Logf("  fossil ls -r feature-xfer: %v stderr=%q", lsByName, lsNameStderr)
		t.Logf("  fossil ls -r <uuid>:       %v stderr=%q", lsByUUID, lsUUIDStderr)
		t.Logf("  walk(open feature-xfer):   %v", walkByName)

		if !equalSorted(lsByName, walkByName) {
			t.Errorf("BUG (xfer/Crosslink path): fossil ls -r <branch-name> != walk\n  ls(name) = %v\n  walk     = %v", lsByName, walkByName)
		}
		if !equalSorted(lsByUUID, walkByName) {
			t.Errorf("BUG (xfer/Crosslink path): fossil ls -r <uuid> != walk\n  ls(uuid) = %v\n  walk     = %v", lsByUUID, walkByName)
		}
	})

	t.Run("branch_tip_after_rebuild", func(t *testing.T) {
		// If `fossil rebuild` repairs the visibility, the bug is in derived
		// tables that libfossil never populated. If rebuild doesn't fix it,
		// the bug is structural (manifest cards / blob bytes).
		repoPath := filepath.Join(t.TempDir(), "rebuild.fossil")
		r, err := repo.Create(repoPath, "test-user", simio.CryptoRand{}, "")
		if err != nil {
			t.Fatalf("repo.Create: %v", err)
		}
		trunkRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "root.txt", Content: []byte("root\n")}},
			Comment: "trunk seed",
			User:    "test-user",
			Time:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("trunk Checkin: %v", err)
		}
		_, branchUUID, err := branch.Create(r, branch.CreateOpts{
			Name:   "feature-y",
			Parent: trunkRid,
			User:   "test-user",
			Time:   time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("branch.Create: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("repo.Close: %v", err)
		}

		// `fossil rebuild` should rebuild every derived table from blob.
		if out, err := exec.Command(fossil, "rebuild", repoPath).CombinedOutput(); err != nil {
			t.Logf("fossil rebuild: %v\n%s", err, out)
		} else {
			t.Logf("fossil rebuild ok:\n%s", out)
		}

		lsAfter, _ := runFossilLs(t, fossil, repoPath, "feature-y")
		walkAfter := runFossilOpenAndWalk(t, fossil, repoPath, "feature-y")
		t.Logf("post-rebuild branch tip=%s\n  ls   = %v\n  walk = %v", branchUUID, lsAfter, walkAfter)
		if !equalSorted(lsAfter, walkAfter) {
			t.Errorf("BUG persists after fossil rebuild — bug is structural in the libfossil-written manifest, not in derived tables\n  ls   = %v\n  walk = %v", lsAfter, walkAfter)
		} else {
			t.Logf("INFO: rebuild repaired the bug — root cause is missing/incorrect derived-table writes in libfossil's commit path")
		}
	})
}

// silence unused import in case branch helpers shrink.
var _ = libfossil.FslID(0)

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
