package cli_test

import (
	"path/filepath"
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
}
