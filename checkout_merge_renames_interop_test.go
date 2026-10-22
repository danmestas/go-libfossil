package libfossil_test

// Fossil-oracle tests for Checkout.Merge following renames. Each case
// builds a history with the fossil binary, then merges feat into trunk
// with fossil's merge in one checkout and go-libfossil's in another, and
// compares the working trees file by file and fossil's changes for each.

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// renameBase is the file every rename case starts from; the cases edit its
// first or last line, which merge apart.
const renameBase = "1\nx\nx\n2\nx\nx\n3\n"

// mergeRenameCase is one history and what go-libfossil's merge reports.
type mergeRenameCase struct {
	name string
	// feat makes the feat branch from the base check-in; trunk then
	// commits on trunk. Each leaves its last commit made.
	feat, trunk func(f renameFossil)
	// pending, when set, runs in both checkouts before the merge.
	pending func(f renameFossil)
	// files is Merge's report, "action name" or "action name -> new".
	files []string
}

// renameFossil runs fossil in one checkout.
type renameFossil struct {
	t        *testing.T
	bin, dir string
}

func (f renameFossil) run(args ...string) string {
	f.t.Helper()
	return fossilRun(f.t, f.bin, f.dir, args...)
}

func (f renameFossil) write(name, content string) {
	f.t.Helper()
	writeFile(f.t, filepath.Join(f.dir, name), content)
}

func (f renameFossil) mv(from, to string) {
	f.t.Helper()
	f.run("mv", "--hard", from, to)
}

func (f renameFossil) commit(msg string, extra ...string) {
	f.t.Helper()
	f.run(append([]string{"commit", "-m", msg, "--no-warnings"}, extra...)...)
}

func mergeRenameCases() []mergeRenameCase {
	const first, last = "1f\nx\nx\n2\nx\nx\n3\n", "1\nx\nx\n2\nx\nx\n3t\n"
	return []mergeRenameCase{{
		name: "renamed there, edited here",
		feat: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.commit("rename", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.write("f.txt", last)
			f.commit("edit")
		},
		files: []string{"renamed f.txt -> r.txt"},
	}, {
		name: "renamed here, edited there",
		feat: func(f renameFossil) {
			f.write("f.txt", first)
			f.commit("edit", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.commit("rename")
		},
		files: []string{"updated r.txt"},
	}, {
		name: "renamed and edited there, edited here",
		feat: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.write("r.txt", first)
			f.commit("rename and edit", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.write("f.txt", last)
			f.commit("edit")
		},
		files: []string{"merged f.txt -> r.txt"},
	}, {
		name: "renamed to the same name on both sides",
		feat: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.write("r.txt", first)
			f.commit("rename and edit", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.write("r.txt", last)
			f.commit("rename and edit")
		},
		files: []string{"merged r.txt"},
	}, {
		name: "renamed to different names",
		feat: func(f renameFossil) {
			f.mv("f.txt", "s.txt")
			f.write("s.txt", first)
			f.commit("rename and edit", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.write("r.txt", last)
			f.commit("rename and edit")
		},
		files: []string{"merged r.txt"},
	}, {
		name: "rename chain there",
		feat: func(f renameFossil) {
			f.mv("f.txt", "g.txt")
			f.commit("rename", "--branch", "feat")
			f.mv("g.txt", "dir/h.txt")
			f.commit("rename again")
			f.write("dir/h.txt", first)
			f.commit("edit")
		},
		trunk: func(f renameFossil) {
			f.write("f.txt", last)
			f.commit("edit")
		},
		files: []string{"merged f.txt -> dir/h.txt"},
	}, {
		name: "rename chain here",
		feat: func(f renameFossil) {
			f.write("f.txt", first)
			f.commit("edit", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.mv("f.txt", "g.txt")
			f.commit("rename")
			f.mv("g.txt", "h.txt")
			f.write("h.txt", last)
			f.commit("rename and edit")
		},
		files: []string{"merged h.txt"},
	}, {
		name: "renamed there, deleted here",
		feat: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.commit("rename", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.run("rm", "--hard", "f.txt")
			f.commit("delete")
		},
		files: nil,
	}, {
		name: "deleted there, renamed here",
		feat: func(f renameFossil) {
			f.run("rm", "--hard", "f.txt")
			f.commit("delete", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.commit("rename")
		},
		files: []string{"deleted r.txt"},
	}, {
		name: "renamed there, merged here before",
		feat: func(f renameFossil) {
			f.mv("f.txt", "r.txt")
			f.commit("rename", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.run("merge", "feat")
			f.commit("merge feat")
			f.run("update", "feat")
			f.write("r.txt", first)
			f.commit("edit")
			f.run("update", "trunk")
			f.write("r.txt", last)
			f.commit("edit")
		},
		files: []string{"merged r.txt"},
	}, {
		name: "swapped there",
		feat: func(f renameFossil) {
			f.mv("f.txt", "t.txt")
			f.mv("a.txt", "f.txt")
			f.mv("t.txt", "a.txt")
			f.commit("swap", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.write("f.txt", last)
			f.commit("edit")
		},
		files: []string{"renamed a.txt -> f.txt", "renamed f.txt -> a.txt"},
	}, {
		name: "renamed here, not committed, edited there",
		feat: func(f renameFossil) {
			f.write("f.txt", first)
			f.commit("edit", "--branch", "feat")
		},
		trunk: func(f renameFossil) {
			f.write("a.txt", "one trunk\n")
			f.commit("edit")
		},
		pending: func(f renameFossil) { f.mv("f.txt", "r.txt") },
		files:   []string{"updated r.txt"},
	}, {
		name: "renamed there over a file deleted there",
		feat: func(f renameFossil) {
			f.run("rm", "--hard", "a.txt")
			f.commit("delete", "--branch", "feat")
			f.mv("f.txt", "a.txt")
			f.commit("rename")
		},
		trunk: func(f renameFossil) {
			f.write("f.txt", last)
			f.commit("edit")
		},
		files: []string{"deleted a.txt", "renamed f.txt -> a.txt"},
	}}
}

// TestMergeFollowsRenamesLikeFossil runs each rename case through fossil's
// merge and go-libfossil's and compares the results.
func TestMergeFollowsRenamesLikeFossil(t *testing.T) {
	for _, tc := range mergeRenameCases() {
		t.Run(tc.name, func(t *testing.T) {
			bin, repoPath, ckDir := newFossilCheckout(t)
			f := renameFossil{t: t, bin: bin, dir: ckDir}
			f.write("f.txt", renameBase)
			f.run("add", "f.txt")
			f.commit("base")
			tc.feat(f)
			f.run("update", "trunk")
			tc.trunk(f)
			libCk := openSecondCheckout(t, bin, repoPath)
			if tc.pending != nil {
				tc.pending(f)
				tc.pending(renameFossil{t: t, bin: bin, dir: libCk})
			}

			out := f.run("merge", "feat")
			res, _ := libfossilMerge(t, repoPath, libCk, "feat")

			if got, want := changeLines(t, bin, libCk), changeLines(t, bin, ckDir); !reflect.DeepEqual(got, want) {
				t.Errorf("fossil changes after go-libfossil merge:\n%s\nafter fossil merge:\n%s",
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if got, want := treeFiles(t, libCk), treeFiles(t, ckDir); !reflect.DeepEqual(got, want) {
				t.Errorf("tree after go-libfossil merge:\n%q\nafter fossil merge:\n%q", got, want)
			}
			var files []string
			for _, mf := range res.Files {
				line := mf.Action + " " + mf.Name
				if mf.RenamedTo != "" {
					line += " -> " + mf.RenamedTo
				}
				files = append(files, line)
			}
			if !reflect.DeepEqual(files, tc.files) {
				t.Errorf("Merge files = %q, want %q; fossil merge printed:\n%s", files, tc.files, out)
			}
			// Fossil commits each merge to the same files, renames recorded.
			if got, want := committedFiles(t, bin, libCk), committedFiles(t, bin, ckDir); !reflect.DeepEqual(got, want) {
				t.Errorf("fossil committed go-libfossil's merge as:\n%s\nits own as:\n%s",
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// committedFiles commits the checkout with fossil and returns the F cards
// of the check-in it made.
func committedFiles(t *testing.T, bin, ckDir string) []string {
	t.Helper()
	fossilRun(t, bin, ckDir, "commit", "-m", "merge", "--allow-fork", "--no-warnings")
	var hash string
	for _, line := range strings.Split(fossilRun(t, bin, ckDir, "info"), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == "checkout:" {
			hash = fields[1]
		}
	}
	if hash == "" {
		t.Fatal("fossil info has no checkout line")
	}
	var cards []string
	for _, line := range strings.Split(fossilRun(t, bin, ckDir, "artifact", hash), "\n") {
		if strings.HasPrefix(line, "F ") {
			cards = append(cards, line)
		}
	}
	return cards
}

// Where fossil's merge would rename a file over one the checkout keeps, or
// over an untracked file, relying on its undo copy, go-libfossil's refuses
// and changes nothing.
func TestMergeRenameRefusesToOverwrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local func(f renameFossil)
		want  string
	}{
		{"edited here, deleted there", func(f renameFossil) { f.write("a.txt", "one edited\n") }, "which this checkout tracks"},
		{"untracked here", func(f renameFossil) { f.write("c.txt", "mine\n") }, "untracked file c.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, repoPath, ckDir := newFossilCheckout(t)
			f := renameFossil{t: t, bin: bin, dir: ckDir}
			f.write("f.txt", renameBase)
			f.run("add", "f.txt")
			f.commit("base")
			if tc.name == "untracked here" {
				f.mv("f.txt", "c.txt")
				f.commit("rename", "--branch", "feat")
			} else {
				f.run("rm", "--hard", "a.txt")
				f.commit("delete", "--branch", "feat")
				f.mv("f.txt", "a.txt")
				f.commit("rename")
			}
			f.run("update", "trunk")
			f.write("b.txt", "two trunk\n")
			f.commit("edit")
			tc.local(f)
			before := treeFiles(t, ckDir)

			ck := openLibfossilCheckout(t, repoPath, ckDir)
			r, err := libfossil.Open(repoPath)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			tip, err := r.BranchTip("feat")
			if err != nil {
				t.Fatal(err)
			}
			_, err = ck.Merge(libfossil.CheckoutMergeOpts{Version: tip})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Merge err = %v, want a refusal naming %q", err, tc.want)
			}
			if after := treeFiles(t, ckDir); !reflect.DeepEqual(after, before) {
				t.Errorf("refused merge changed the tree:\n%q\nwas:\n%q", after, before)
			}
			if got := changeLines(t, bin, ckDir); len(got) > 1 || (len(got) == 1 && !strings.HasPrefix(got[0], "EDITED")) {
				t.Errorf("refused merge left fossil changes %q", got)
			}
		})
	}
}
