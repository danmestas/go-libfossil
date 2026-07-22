package checkout

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/danmestas/libfossil/internal/content"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
)

// TestExecutableBitRoundTrip verifies that a file's owner-execute bit,
// captured at Manage (Add) time, survives Commit and is restored by
// Extract into a fresh checkout.
//
// Predicate under test matches Fossil's file_perm() (src/file.c:316):
// S_ISREG(st_mode) && (S_IXUSR & st_mode) != 0 — owner-execute on a regular
// file. Group/other-only execute (0645) does NOT count.
func TestExecutableBitRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bit is never detected on Windows")
	}

	cases := []struct {
		name     string
		mode     os.FileMode
		wantExec bool
	}{
		{"owner-exec-0755", 0o755, true},
		{"owner-exec-0711", 0o711, true},
		{"group-other-exec-only-0645", 0o645, false},
		{"no-exec-0644", 0o644, false},
	}

	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	// First checkout: extract the initial version, then add files at
	// various on-disk modes.
	dir1 := t.TempDir()
	co1, err := Create(r, dir1, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co1.Close()

	rid, _, err := co1.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co1.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	paths := make(map[string]string, len(cases))
	for _, tc := range cases {
		fname := tc.name + ".sh"
		fullPath := filepath.Join(dir1, fname)
		if err := os.WriteFile(fullPath, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fullPath, tc.mode); err != nil {
			t.Fatal(err)
		}
		paths[tc.name] = fname
	}

	toAdd := make([]string, 0, len(cases))
	for _, tc := range cases {
		toAdd = append(toAdd, paths[tc.name])
	}
	counts, err := co1.Manage(ManageOpts{Paths: toAdd})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Added != len(cases) {
		t.Fatalf("added = %d, want %d", counts.Added, len(cases))
	}

	// vfile.isexe must reflect the on-disk mode immediately after Manage.
	for _, tc := range cases {
		var isexe int
		err := co1.db.QueryRow(
			"SELECT CAST(isexe AS INTEGER) FROM vfile WHERE vid=? AND pathname=?",
			int64(rid), paths[tc.name],
		).Scan(&isexe)
		if err != nil {
			t.Fatalf("%s: query vfile isexe: %v", tc.name, err)
		}
		wantIsexe := 0
		if tc.wantExec {
			wantIsexe = 1
		}
		if isexe != wantIsexe {
			t.Errorf("%s: vfile.isexe after Manage = %d, want %d", tc.name, isexe, wantIsexe)
		}
	}

	newRID, _, err := co1.Commit(CommitOpts{Message: "add executables", User: "test"})
	if err != nil {
		t.Fatal(err)
	}

	// The manifest F-card must carry the executable marker.
	entries, err := manifest.ListFiles(r, newRID)
	if err != nil {
		t.Fatal(err)
	}
	permByName := make(map[string]string, len(entries))
	for _, e := range entries {
		permByName[e.Name] = e.Perm
	}
	for _, tc := range cases {
		perm := permByName[paths[tc.name]]
		gotExec := perm == "x"
		if gotExec != tc.wantExec {
			t.Errorf("%s: F-card Perm = %q, owner-exec = %v, want %v", tc.name, perm, gotExec, tc.wantExec)
		}
	}

	// Fresh checkout: extracting the new commit must restore owner-execute
	// for the executable cases and leave the rest at 0644.
	dir2 := t.TempDir()
	co2, err := Create(r, dir2, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co2.Close()

	if err := co2.Extract(newRID, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		fullPath := filepath.Join(dir2, paths[tc.name])
		info, err := os.Stat(fullPath)
		if err != nil {
			t.Fatalf("%s: stat extracted file: %v", tc.name, err)
		}
		gotExec := info.Mode()&0o100 != 0
		if gotExec != tc.wantExec {
			t.Errorf(
				"%s: extracted mode = %#o, owner-exec = %v, want %v",
				tc.name, info.Mode().Perm(), gotExec, tc.wantExec,
			)
		}
	}
}

// TestModeOnlyChangeRestatOnCommit is the decisive regression test for the
// mode-change-without-content-change bug: chmod +x (or -x) on an
// already-committed, content-unmodified tracked file, with Add never
// re-run, must still be reflected in the next commit's F-card.
//
// Change scanning's hash comparison never flags this file — its content
// hash is identical before and after the chmod — so vfile.chnged stays 0
// and vfile.isexe still holds whatever was captured at the last Add/Commit.
// Commit-time manifest generation must independently re-stat the on-disk
// mode for every file it is about to serialize, not trust that cached
// value, mirroring canonical Fossil's belt-and-braces re-stat in
// src/checkin.c.
func TestModeOnlyChangeRestatOnCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-execute bit is never detected on Windows")
	}

	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{"script.sh"}}); err != nil {
		t.Fatal(err)
	}

	firstRID, _, err := co.Commit(CommitOpts{Message: "add non-exec script", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	assertFilePerm(t, r, firstRID, "script.sh", "")

	// The decisive case: chmod +x on disk WITHOUT re-running Add.
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatal(err)
	}
	execRID, _, err := co.Commit(CommitOpts{Message: "chmod +x, no re-add", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	assertFilePerm(t, r, execRID, "script.sh", "x")

	// Reverse: chmod -x, again without re-running Add.
	if err := os.Chmod(scriptPath, 0o644); err != nil {
		t.Fatal(err)
	}
	nonExecRID, _, err := co.Commit(CommitOpts{Message: "chmod -x, no re-add", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	assertFilePerm(t, r, nonExecRID, "script.sh", "")

	// Combined: a genuine content change AND a mode change land in the
	// same commit — both must be reflected correctly.
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatal(err)
	}
	combinedRID, _, err := co.Commit(CommitOpts{Message: "content + mode change", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	assertFilePerm(t, r, combinedRID, "script.sh", "x")

	fileContent, err := content.Expand(r.DB(), fileRIDByName(t, r, combinedRID, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(fileContent) != "#!/bin/sh\necho updated\n" {
		t.Errorf("script.sh content after combined change = %q, want updated content", fileContent)
	}
}

// TestCommitFailsOnMissingFileDuringRestat is a regression test covering
// the intersection of the mode-change re-stat fix and issue #79: Stat-ing
// every in-scope tracked file at commit time (to catch a missed chmod) must
// surface a file that has gone missing from disk without Unmanage as a
// clear commit failure, not silently carry its last-committed content
// forward. An earlier version of this codebase treated the missing file as
// something to skip past here — that was the documented, deliberately
// deferred behavior issue #79 exists to resolve; this test now asserts the
// resolved behavior instead.
//
// Deletes README.md from disk without calling Unmanage, modifies an
// unrelated tracked file, and commits with the default implicit "commit
// everything" scope: the commit must fail, naming README.md.
func TestCommitFailsOnMissingFileDuringRestat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-execute bit is never detected on Windows")
	}

	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// README.md's permission before it goes missing.
	assertFilePerm(t, r, rid, "README.md", "")

	// Delete README.md from disk directly — Unmanage is never called, so
	// vfile still tracks it. Modify an unrelated file so the commit has
	// something genuine to record.
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello again\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err = co.Commit(CommitOpts{Message: "modify hello.txt, README.md missing", User: "test"})
	if err == nil {
		t.Fatal("Commit succeeded despite a missing tracked file in scope, want error naming README.md")
	}
	if !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("err = %v, want it to name the missing file README.md", err)
	}
}

// assertFilePerm looks up name in rid's manifest and asserts its Perm field.
func assertFilePerm(t *testing.T, r *repo.Repo, rid libfossil.FslID, name, want string) {
	t.Helper()
	entries, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == name {
			if e.Perm != want {
				t.Errorf("%s: Perm = %q, want %q", name, e.Perm, want)
			}
			return
		}
	}
	t.Fatalf("%s: not present in manifest for rid %d", name, rid)
}

// fileRIDByName resolves the blob RID for name within rid's manifest.
func fileRIDByName(t *testing.T, r *repo.Repo, rid libfossil.FslID, name string) libfossil.FslID {
	t.Helper()
	entries, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == name {
			var fileRID int64
			if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid = ?", e.UUID).Scan(&fileRID); err != nil {
				t.Fatal(err)
			}
			return libfossil.FslID(fileRID)
		}
	}
	t.Fatalf("%s: not present in manifest for rid %d", name, rid)
	return 0
}
