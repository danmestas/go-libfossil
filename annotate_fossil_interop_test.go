package libfossil_test

// Repo.Annotate against `fossil annotate` (#256, #257): each history is
// committed with the fossil binary, then both annotate the tip and must
// credit every line to the same check-in. Histories of repeated and blank
// lines are where the two used to disagree, since which repeated line counts
// as new depends on the diff's tie-breaking.

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/testutil"
)

// commitHistory commits each content of f.txt in turn with the fossil
// binary and returns the repository and checkout paths.
func commitHistory(t *testing.T, contents []string) (bin, repoPath, ckDir string) {
	t.Helper()
	bin = testutil.RequireFossilBin(t)
	dir := t.TempDir()
	repoPath = filepath.Join(dir, "a.fossil")
	ckDir = filepath.Join(dir, "ck")
	if err := os.Mkdir(ckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilRun(t, bin, dir, "init", repoPath)
	fossilRun(t, bin, ckDir, "open", repoPath)
	path := filepath.Join(ckDir, "f.txt")
	mtime := time.Unix(1_700_000_000, 0)
	for i, c := range contents {
		writeFile(t, path, c)
		// fossil notices a change by mtime; same-size rewrites need a new one.
		mtime = mtime.Add(10 * time.Second)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			fossilRun(t, bin, ckDir, "add", "f.txt")
		}
		fossilRun(t, bin, ckDir, "commit", "-m", fmt.Sprintf("v%d", i))
	}
	return bin, repoPath, ckDir
}

// annotationsAgree compares the check-in each line is credited to.
func annotationsAgree(t *testing.T, contents []string) {
	t.Helper()
	bin, repoPath, ckDir := commitHistory(t, contents)
	var want []string
	out := fossilRun(t, bin, ckDir, "annotate", "--limit", "none", "f.txt")
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			want = append(want, fields[0])
		}
	}

	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	tip, err := r.ResolveVersion("trunk")
	if err != nil {
		t.Fatal(err)
	}
	lines, err := r.Annotate(libfossil.AnnotateOpts{FilePath: "f.txt", StartRID: tip})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.UUID[:len(want[0])])
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("history %q:\nAnnotate credits %v\nfossil credits   %v", contents, got, want)
	}
}

func TestAnnotateMatchesFossilRepeatedLines(t *testing.T) {
	annotationsAgree(t, []string{"\n\nc\n", "\n\na\n\nc\n"})
}

func TestAnnotateMatchesFossilTrailingBlankLines(t *testing.T) {
	annotationsAgree(t, []string{"\n", "b\n\n"})
}

// Seeded random histories over a four-line alphabet, so most lines repeat.
func TestAnnotateMatchesFossilRandomHistories(t *testing.T) {
	for seed := int64(0); seed < 12; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			annotationsAgree(t, randomHistory(rand.New(rand.NewSource(seed))))
		})
	}
}

// randomHistory edits a short file of repeated lines a few times.
func randomHistory(rnd *rand.Rand) []string {
	alphabet := []string{"a", "b", "{", ""}
	pick := func() string { return alphabet[rnd.Intn(len(alphabet))] }
	var lines []string
	for n := 2 + rnd.Intn(8); n > 0; n-- {
		lines = append(lines, pick())
	}
	var history []string
	for v := 0; v < 2+rnd.Intn(4); v++ {
		for e := rnd.Intn(4); v > 0 && e >= 0; e-- {
			at := rnd.Intn(len(lines) + 1)
			switch {
			case rnd.Intn(2) == 0:
				lines = append(lines[:at], append([]string{pick()}, lines[at:]...)...)
			case at < len(lines) && len(lines) > 1:
				lines = append(lines[:at], lines[at+1:]...)
			}
		}
		content := strings.Join(lines, "\n") + "\nend\n"
		if len(history) == 0 || history[len(history)-1] != content {
			history = append(history, content)
		}
	}
	return history
}
