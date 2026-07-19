package checkout

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/danmestas/libfossil/internal/manifest"
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
