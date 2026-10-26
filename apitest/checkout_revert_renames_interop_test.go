//go:build !js

package apitest_test

// Interop tests for reverting renames whose old name another file now holds
// (#248): a swap, a chain, a new add or an untracked file at the old name.
// Revert saves each file it changes for undo first, as fossil's revert does,
// then puts every committed file back under its committed name.

import (
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// revertRenameCases are fossil-made checkouts of a.txt ("one\n") and b.txt
// ("two\n") where undoing a rename lands on a name that is taken.
var revertRenameCases = []struct {
	name  string
	setup func(t *testing.T, bin, ckDir string)
	files []string // revert these; nil reverts everything
	gone  []string // names that must not be on disk after the revert
}{
	{
		name: "swap",
		setup: func(t *testing.T, bin, ckDir string) {
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "tmp.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "b.txt", "a.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "tmp.txt", "b.txt")
		},
	},
	{
		name: "swap, by one name",
		setup: func(t *testing.T, bin, ckDir string) {
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "tmp.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "b.txt", "a.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "tmp.txt", "b.txt")
		},
		files: []string{"b.txt"},
	},
	{
		name: "chain",
		setup: func(t *testing.T, bin, ckDir string) {
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "c.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "b.txt", "a.txt")
		},
		gone: []string{"c.txt"},
	},
	{
		name: "chain, by its last name",
		setup: func(t *testing.T, bin, ckDir string) {
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "c.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "b.txt", "a.txt")
		},
		files: []string{"c.txt"},
		gone:  []string{"c.txt"},
	},
	{
		name: "chain, by its middle name",
		setup: func(t *testing.T, bin, ckDir string) {
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "c.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "b.txt", "a.txt")
		},
		files: []string{"a.txt"},
		gone:  []string{"c.txt"},
	},
	{
		name: "add at the old name",
		setup: func(t *testing.T, bin, ckDir string) {
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "z.txt")
			writeFile(t, filepath.Join(ckDir, "a.txt"), "brand new\n")
			fossilRun(t, bin, ckDir, "add", "a.txt")
		},
		gone: []string{"z.txt"},
	},
	{
		name: "add at the old name, by old name",
		setup: func(t *testing.T, bin, ckDir string) {
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "z.txt")
			writeFile(t, filepath.Join(ckDir, "a.txt"), "brand new\n")
			fossilRun(t, bin, ckDir, "add", "a.txt")
		},
		files: []string{"a.txt"},
		gone:  []string{"z.txt"},
	},
	{
		name: "untracked file at the old name",
		setup: func(t *testing.T, bin, ckDir string) {
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "z.txt")
			writeFile(t, filepath.Join(ckDir, "a.txt"), "mine, untracked\n")
		},
		gone: []string{"z.txt"},
	},
}

// TestRevertUndoesTakenNameRenames: after Revert, fossil sees no changes and
// every committed file is tracked under its committed name with its
// committed content.
func TestRevertUndoesTakenNameRenames(t *testing.T) {
	for _, tc := range revertRenameCases {
		t.Run(tc.name, func(t *testing.T) {
			bin, repoPath, ckDir := newFossilCheckout(t)
			tc.setup(t, bin, ckDir)
			ck := openLibfossilCheckout(t, repoPath, ckDir)

			if err := ck.Revert(libfossil.RevertOpts{Files: tc.files}); err != nil {
				t.Fatalf("Revert: %v", err)
			}
			assertCommittedTree(t, bin, ckDir, tc.gone)
		})
	}
}

// TestUndoReversesRevertOfTakenNameRenames: Undo, go-libfossil's or
// fossil's, after Revert puts back the files and the checkout as they were
// before it, as fossil's undo does after its own revert.
func TestUndoReversesRevertOfTakenNameRenames(t *testing.T) {
	undoers := []struct {
		name string
		undo func(t *testing.T, bin, ckDir string, ck *libfossil.Checkout)
	}{
		{"Undo", func(t *testing.T, _, _ string, ck *libfossil.Checkout) {
			if _, err := ck.Undo(); err != nil {
				t.Fatalf("Undo: %v", err)
			}
		}},
		{"fossil undo", func(t *testing.T, bin, ckDir string, _ *libfossil.Checkout) {
			fossilRun(t, bin, ckDir, "undo")
		}},
	}
	for _, tc := range revertRenameCases {
		for _, u := range undoers {
			t.Run(tc.name+"/"+u.name, func(t *testing.T) {
				bin, repoPath, ckDir := newFossilCheckout(t)
				tc.setup(t, bin, ckDir)
				before := revertUndoState(t, bin, ckDir)
				ck := openLibfossilCheckout(t, repoPath, ckDir)

				if err := ck.Revert(libfossil.RevertOpts{Files: tc.files}); err != nil {
					t.Fatalf("Revert: %v", err)
				}
				u.undo(t, bin, ckDir, ck)
				if got := revertUndoState(t, bin, ckDir); got != before {
					t.Fatalf("after undo:\n%s\nwant, as before the revert:\n%s", got, before)
				}
			})
		}
	}
}

// assertCommittedTree checks that ckDir is the seed check-in again: fossil
// reports no changes, tracks a.txt and b.txt, both hold their committed
// content, and no name in gone is left on disk.
func assertCommittedTree(t *testing.T, bin, ckDir string, gone []string) {
	t.Helper()
	if got := strings.TrimSpace(fossilRun(t, bin, ckDir, "changes")); got != "" {
		t.Fatalf("fossil changes after Revert = %q, want none", got)
	}
	if got := strings.Fields(fossilRun(t, bin, ckDir, "ls")); strings.Join(got, " ") != "a.txt b.txt" {
		t.Fatalf("fossil ls after Revert = %q, want a.txt b.txt", got)
	}
	for name, want := range map[string]string{"a.txt": "one\n", "b.txt": "two\n"} {
		data, err := os.ReadFile(filepath.Join(ckDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q after Revert, want its committed %q", name, data, want)
		}
	}
	for _, name := range gone {
		if _, err := os.Stat(filepath.Join(ckDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still on disk after Revert (stat err %v)", name, err)
		}
	}
}

// revertUndoState is what an undo of a revert must restore: the files on
// disk and what fossil's changes and ls report.
func revertUndoState(t *testing.T, bin, dir string) string {
	t.Helper()
	var files []string
	for name, body := range treeFiles(t, dir) {
		files = append(files, name+"="+body)
	}
	sort.Strings(files)
	return strings.Join(files, "\n") + "\n" + fossilRun(t, bin, dir, "changes") +
		fossilRun(t, bin, dir, "ls")
}

// TestRevertAddAtOldNameMatchesFossil: undoing a rename onto a new add at
// the old name leaves the files and the checkout as fossil's own revert
// does, by every name the revert can be given, and Undo after it restores
// what fossil undo restores after fossil's revert.
func TestRevertAddAtOldNameMatchesFossil(t *testing.T) {
	setup := func(t *testing.T, bin, ckDir string) {
		fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "z.txt")
		writeFile(t, filepath.Join(ckDir, "a.txt"), "brand new\n")
		fossilRun(t, bin, ckDir, "add", "a.txt")
	}
	for _, files := range [][]string{nil, {"a.txt"}, {"z.txt"}} {
		t.Run(strings.Join(append([]string{"revert"}, files...), " "), func(t *testing.T) {
			bin, _, fsDir := newFossilCheckout(t)
			setup(t, bin, fsDir)
			fossilRun(t, bin, fsDir, append([]string{"revert"}, files...)...)
			wantReverted := revertUndoState(t, bin, fsDir)
			fossilRun(t, bin, fsDir, "undo")
			wantUndone := revertUndoState(t, bin, fsDir)

			_, repoPath, ckDir := newFossilCheckout(t)
			setup(t, bin, ckDir)
			ck := openLibfossilCheckout(t, repoPath, ckDir)
			if err := ck.Revert(libfossil.RevertOpts{Files: files}); err != nil {
				t.Fatalf("Revert: %v", err)
			}
			if got := revertUndoState(t, bin, ckDir); got != wantReverted {
				t.Fatalf("after Revert:\n%s\nwant, as fossil's revert leaves it:\n%s", got, wantReverted)
			}
			if _, err := ck.Undo(); err != nil {
				t.Fatalf("Undo: %v", err)
			}
			if got := revertUndoState(t, bin, ckDir); got != wantUndone {
				t.Fatalf("after Undo:\n%s\nwant, as fossil undo leaves it:\n%s", got, wantUndone)
			}
		})
	}
}

// TestRevertUndoesRotation: three files rotated through each other's names
// all go back to their committed names, by any one of the names, and Undo
// puts the rotation back.
func TestRevertUndoesRotation(t *testing.T) {
	for _, files := range [][]string{nil, {"a.txt"}, {"c.txt"}} {
		t.Run(strings.Join(append([]string{"revert"}, files...), " "), func(t *testing.T) {
			bin, repoPath, ckDir := newFossilCheckout(t)
			writeFile(t, filepath.Join(ckDir, "c.txt"), "three\n")
			fossilRun(t, bin, ckDir, "add", "c.txt")
			fossilRun(t, bin, ckDir, "commit", "-m", "third")
			fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "tmp.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "c.txt", "a.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "b.txt", "c.txt")
			fossilRun(t, bin, ckDir, "mv", "--hard", "tmp.txt", "b.txt")
			before := revertUndoState(t, bin, ckDir)
			ck := openLibfossilCheckout(t, repoPath, ckDir)

			if err := ck.Revert(libfossil.RevertOpts{Files: files}); err != nil {
				t.Fatalf("Revert: %v", err)
			}
			if got := strings.TrimSpace(fossilRun(t, bin, ckDir, "changes")); got != "" {
				t.Fatalf("fossil changes after Revert = %q, want none", got)
			}
			want := map[string]string{"a.txt": "one\n", "b.txt": "two\n", "c.txt": "three\n"}
			if got := treeFiles(t, ckDir); !maps.Equal(got, want) {
				t.Fatalf("files after Revert = %q, want %q", got, want)
			}
			if _, err := ck.Undo(); err != nil {
				t.Fatalf("Undo: %v", err)
			}
			if got := revertUndoState(t, bin, ckDir); got != before {
				t.Fatalf("after Undo:\n%s\nwant, as before the revert:\n%s", got, before)
			}
		})
	}
}

// TestRevertChainByFirstNameKeepsTheOtherRename: reverting only the second
// rename of a chain (b.txt, now at a.txt) puts b.txt back and leaves the
// first rename (a.txt to c.txt) pending, its file intact.
func TestRevertChainByFirstNameKeepsTheOtherRename(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "c.txt")
	fossilRun(t, bin, ckDir, "mv", "--hard", "b.txt", "a.txt")
	ck := openLibfossilCheckout(t, repoPath, ckDir)

	if err := ck.Revert(libfossil.RevertOpts{Files: []string{"b.txt"}}); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	want := map[string]string{"b.txt": "two\n", "c.txt": "one\n"}
	if got := treeFiles(t, ckDir); !maps.Equal(got, want) {
		t.Fatalf("files after Revert = %q, want %q", got, want)
	}
	if got := strings.Fields(fossilRun(t, bin, ckDir, "changes")); strings.Join(got, " ") != "RENAMED a.txt -> c.txt" {
		t.Fatalf("fossil changes after Revert = %q, want only a.txt's rename to c.txt", got)
	}
}
