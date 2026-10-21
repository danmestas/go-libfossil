package cli_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

type submanifestsTestCLI struct {
	cli.Globals
	Repo cli.RepoCmd `cmd:""`
}

// repo submanifests enable, retune and disable drive the three settings.
func TestRepoSubmanifestsCmd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{Comment: "c", User: "u", Time: time.Now(),
		Files: []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("a\n")}}}); err != nil {
		t.Fatal(err)
	}
	r.Close()

	run := func(args ...string) error {
		var c submanifestsTestCLI
		parser, err := kong.New(&c)
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := parser.Parse(append([]string{"-R", path, "repo", "submanifests"}, args...))
		if err != nil {
			return err
		}
		return ctx.Run(&c.Globals)
	}
	setting := func(name string) string {
		r, err := libfossil.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		v, _ := r.Config(name)
		return v
	}
	if err := run("enable"); err == nil {
		t.Fatal("enable selected k over a one-file check-in")
	}
	if err := run("retune"); err == nil {
		t.Fatal("retune without --k accepted")
	}
	if err := run("enable", "--k", "3"); err != nil {
		t.Fatal(err)
	}
	if setting("enable-submanifests") != "on" || setting("submanifest-k") != "3" {
		t.Fatal("enable --k 3 did not pin and enable")
	}
	if err := run("enable", "--k", "4"); err == nil {
		t.Fatal("enable --k 4 over a pin of 3 accepted")
	}
	if err := run("retune", "--k", "5"); err != nil || setting("submanifest-k") != "5" {
		t.Fatalf("retune --k 5: %v", err)
	}
	if err := run("disable"); err != nil || setting("enable-submanifests") != "off" {
		t.Fatalf("disable: %v", err)
	}
	if err := run("status"); err != nil {
		t.Fatal(err)
	}
}

// status reports the settings and whether the repository holds a tree.
func TestRepoSubmanifestsStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	status := func() string {
		g := &cli.Globals{Repo: path}
		out := captureStdout(t, func() error { return (&cli.RepoSubmanifestsStatusCmd{}).Run(g) })
		return out
	}
	r.Close()
	if got := status(); !strings.Contains(got, "enable-submanifests:     off") ||
		!strings.Contains(got, "(unpinned)") || !strings.Contains(got, "trees in repository:     no") {
		t.Fatalf("status of a new repository:\n%s", got)
	}
	r, err = libfossil.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.EnableSubmanifests(3); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{Comment: "tree", User: "u", Time: time.Now(),
		Files: []libfossil.FileToCommit{
			{Name: "README.md", Content: []byte("# Example\n")},
			{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
			{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
			{Name: "src/util.c", Content: []byte("/* util */\n")},
		}}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if got := status(); !strings.Contains(got, "enable-submanifests:     on") ||
		!strings.Contains(got, "submanifest-k:           3") || !strings.Contains(got, "trees in repository:     yes") {
		t.Fatalf("status after a tree commit:\n%s", got)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	runErr := fn()
	wr.Close()
	os.Stdout = old
	out, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	return string(out)
}

// §8.1: an explicit k outside 1..12 is refused, --k 0 included; only an
// absent --k selects.
func TestRepoSubmanifestsEnableExplicitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{Comment: "c", User: "u", Time: time.Now(),
		Files: []libfossil.FileToCommit{
			{Name: "README.md", Content: []byte("# Example\n")},
			{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
			{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
			{Name: "src/util.c", Content: []byte("/* util */\n")},
		}}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	run := func(args ...string) error {
		var c submanifestsTestCLI
		parser, err := kong.New(&c)
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := parser.Parse(append([]string{"-R", path, "repo", "submanifests", "enable"}, args...))
		if err != nil {
			return err
		}
		return ctx.Run(&c.Globals)
	}
	if err := run("--k", "0"); err == nil {
		t.Fatal("enable --k 0 accepted")
	}
	if err := run(); err != nil {
		t.Fatalf("enable with no --k: %v", err)
	}
}

// status shows a stored k outside 1..12 as invalid, and fails rather than
// print defaults when the settings cannot be read.
func TestRepoSubmanifestsStatusInvalidAndUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetConfig("submanifest-k", "13"); err != nil {
		t.Fatal(err)
	}
	r.Close()
	g := &cli.Globals{Repo: path}
	got := captureStdout(t, func() error { return (&cli.RepoSubmanifestsStatusCmd{}).Run(g) })
	if !strings.Contains(got, "13 (invalid") || !strings.Contains(got, "flat") {
		t.Fatalf("status with k 13:\n%s", got)
	}
	r, err = libfossil.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// The sub-manifest rows fail to read; every other config row reads, so
	// the repository still opens.
	if _, err := r.Inner().DB().Exec(`ALTER TABLE config RENAME TO config_rows;
		CREATE VIEW config AS SELECT name, CASE WHEN name LIKE '%submanifest%'
		THEN json('not json') ELSE value END AS value, mtime FROM config_rows`); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := (&cli.RepoSubmanifestsStatusCmd{}).Run(g); err == nil || !strings.Contains(err.Error(), "reading") {
		t.Fatalf("status over unreadable settings: %v", err)
	}
}
