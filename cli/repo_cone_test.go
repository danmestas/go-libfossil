package cli_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"
)

// repo open --cone makes a sparse checkout; repo cone shows it, widens it,
// narrows it, tests paths against it and returns it to dense.
func TestRepoConeCmd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	var files []libfossil.FileToCommit
	for _, name := range []string{"README", "src/a.c", "src/vendor/v.c", "tools/t.c"} {
		files = append(files, libfossil.FileToCommit{Name: name, Content: []byte(name + "\n")})
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{Comment: "c", User: "u", Time: time.Now(), Files: files}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	work := filepath.Join(dir, "work")

	run := func(args ...string) (string, error) {
		var c submanifestsTestCLI
		parser, err := kong.New(&c)
		if err != nil {
			t.Fatal(err)
		}
		var runErr error
		out := captureStdout(t, func() error {
			ctx, err := parser.Parse(append([]string{"-R", path, "repo"}, args...))
			if err != nil {
				runErr = err
				return nil
			}
			runErr = ctx.Run(&c.Globals)
			return nil
		})
		return out, runErr
	}
	present := func(name string) bool {
		_, err := os.Stat(filepath.Join(work, name))
		return err == nil
	}

	if _, err := run("open", work, "--cone", "src", "--cone-not", "src/vendor"); err != nil {
		t.Fatal(err)
	}
	if !present("src/a.c") || present("src/vendor/v.c") || present("README") {
		t.Fatal("open --cone did not materialize exactly the cone")
	}
	if out, err := run("cone", "-d", work); err != nil || out != "src\n!src/vendor\n" {
		t.Fatalf("cone = %q, %v", out, err)
	}
	// Status works within the cone: the uncovered files are not missing,
	// and an uncovered file on disk is not extra.
	if err := os.WriteFile(filepath.Join(work, "stray.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run("status", "-d", work); err != nil || out != "" {
		t.Fatalf("status of a clean sparse checkout = %q, %v", out, err)
	}
	if err := os.Remove(filepath.Join(work, "stray.txt")); err != nil {
		t.Fatal(err)
	}
	out, err := run("cone", "test", "-d", work, "src/a.c", "src/vendor/v.c", "README")
	if !errors.Is(err, cli.ErrConeTestElided) {
		t.Fatalf("cone test with elided paths: %v", err)
	}
	// zit's layout: the verdict, the path and the deciding member, aligned.
	want := fmt.Sprintf("%-8s%-16s%s\n%-8s%-16s%s\n%-8s%-16s%s\n",
		"covered", "src/a.c", "src", "elided", "src/vendor/v.c", "src/vendor",
		"elided", "README", "(no member covers)")
	if out != want {
		t.Fatalf("cone test =\n%s\nwant\n%s", out, want)
	}
	if _, err := run("cone", "add", "-d", work, "tools"); err != nil || !present("tools/t.c") {
		t.Fatalf("cone add tools: %v", err)
	}
	if _, err := run("cone", "remove", "-d", work, "src/vendor"); err != nil || !present("src/vendor/v.c") {
		t.Fatalf("cone remove src/vendor (an exclusion): %v", err)
	}
	if _, err := run("cone", "add", "-d", work, "--not", "src/vendor"); err != nil || present("src/vendor/v.c") {
		t.Fatalf("cone add --not src/vendor: %v", err)
	}
	if _, err := run("cone", "set", "-d", work, "README"); err != nil || present("src/a.c") || !present("README") {
		t.Fatalf("cone set README: %v", err)
	}
	if out, err := run("add", "-d", work, filepath.Join(work, "src/a.c")); err == nil ||
		!strings.Contains(err.Error(), "sparse cone") {
		t.Fatalf("add outside the cone: %q, %v", out, err)
	}
	if _, err := run("cone", "dense", "-d", work); err != nil || !present("src/a.c") {
		t.Fatalf("cone dense: %v", err)
	}
	if out, err := run("cone", "-d", work); err != nil || out != "dense\n" {
		t.Fatalf("cone after dense = %q, %v", out, err)
	}
}

// repo cone compact refuses a dense store, as zit's pack --drop-elided does,
// and after narrowing drops the blob only an out-of-cone name references.
func TestRepoConeCompactCmd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	var files []libfossil.FileToCommit
	for _, name := range []string{"src/a.c", "tools/t.c"} {
		files = append(files, libfossil.FileToCommit{Name: name, Content: []byte(name + "\n")})
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{Comment: "c", User: "u", Time: time.Now(), Files: files}); err != nil {
		t.Fatal(err)
	}
	// Unsent content has no peer to restore it; pretend it was pushed.
	if _, err := r.DB().Exec("DELETE FROM unsent"); err != nil {
		t.Fatal(err)
	}
	r.Close()
	run := func(args ...string) (string, error) {
		var c submanifestsTestCLI
		parser, err := kong.New(&c)
		if err != nil {
			t.Fatal(err)
		}
		var runErr error
		out := captureStdout(t, func() error {
			ctx, err := parser.Parse(append([]string{"-R", path, "repo", "cone"}, args...))
			if err != nil {
				runErr = err
				return nil
			}
			runErr = ctx.Run(&c.Globals)
			return nil
		})
		return out, runErr
	}
	if out, err := run("compact"); !errors.Is(err, libfossil.ErrNoStoreCone) || out != "" {
		t.Fatalf("dense compact = %q, %v", out, err)
	}
	r, err = libfossil.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := libfossil.NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetStoreCone(narrow); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if out, err := run("compact"); err != nil || out != "compacted 1 artifact\n" {
		t.Fatalf("compact = %q, %v", out, err)
	}
	if out, err := run("compact"); err != nil || out != "compacted 0 artifacts\n" {
		t.Fatalf("repeat compact = %q, %v", out, err)
	}
}
