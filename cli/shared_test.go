package cli_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"
	libdb "github.com/danmestas/go-libfossil/db"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestGlobalsOpenRepo(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "test.fossil")

	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.Close()

	g := &cli.Globals{Repo: repoPath}
	opened, err := g.OpenRepo()
	if err != nil {
		t.Fatalf("OpenRepo: %v", err)
	}
	defer opened.Close()

	if opened.Path() != repoPath {
		t.Errorf("Path() = %q, want %q", opened.Path(), repoPath)
	}
}

func TestGlobalsOpenRepoNotFound(t *testing.T) {
	g := &cli.Globals{Repo: "/nonexistent/repo.fossil"}
	_, err := g.OpenRepo()
	if err == nil {
		t.Fatal("expected error for nonexistent repo")
	}
}

func TestGlobalsOpenRepoAutoFind(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "auto.fossil")

	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.Close()

	// Change to the temp dir so findRepo can discover the .fossil file.
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(tmp)

	g := &cli.Globals{}
	opened, err := g.OpenRepo()
	if err != nil {
		t.Fatalf("OpenRepo auto-find: %v", err)
	}
	defer opened.Close()
}
func TestRepoCiPreservesNestedRelativePaths(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "nested.fossil")

	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.Close()

	work := filepath.Join(tmp, "work")
	nested := filepath.Join(work, "src", "app.txt")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}

	cmd := &cli.RepoCiCmd{
		Message: "initial nested",
		Files:   []string{filepath.Join("src", "app.txt")},
		User:    "test",
	}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("RepoCiCmd.Run: %v", err)
	}

	opened, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer opened.Close()
	rid, err := opened.ResolveVersion("tip")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	files, err := opened.ListFiles(rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 || files[0].Name != "src/app.txt" {
		t.Fatalf("files = %+v, want one src/app.txt entry", files)
	}
}

func TestRepoOpenPopulatesVFileFromTip(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "open.fossil")

	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: "hello.txt", Content: []byte("hello\n")},
		},
		Comment: "initial",
		User:    "test",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	r.Close()

	checkoutDir := filepath.Join(tmp, "checkout")
	cmd := &cli.RepoOpenCmd{Dir: checkoutDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("RepoOpenCmd.Run: %v", err)
	}

	ckdb, err := libdb.OpenSQL(filepath.Join(checkoutDir, ".fslckout"), libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatalf("open checkout db: %v", err)
	}
	defer ckdb.Close()
	var count int
	if err := ckdb.QueryRow("SELECT count(*) FROM vfile WHERE pathname='hello.txt' AND rid > 0").Scan(&count); err != nil {
		t.Fatalf("query vfile: %v", err)
	}
	if count != 1 {
		t.Fatalf("vfile count = %d, want 1", count)
	}
}

func TestRepoCiRejectsOutsideCurrentDirectory(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "outside.fossil")
	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.Close()

	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}

	cmd := &cli.RepoCiCmd{
		Message: "outside",
		Files:   []string{filepath.Join("..", "outside.txt")},
		User:    "test",
	}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err == nil {
		t.Fatal("RepoCiCmd.Run accepted path outside current directory")
	}
}

// TestRepoTimelineRendersQuestionMarkForNullUser matches canonical fossil's
// TTY rendering (coalesce(euser,user,'?') in timeline_query_for_tty()): a
// check-in with no recorded user displays as "?", not blank. The library
// itself must still return "" (see internal/manifest.TestLogNullUser) — the
// substitution belongs at this presentation boundary only.
func TestRepoTimelineRendersQuestionMarkForNullUser(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "test.fossil")

	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rid1, _, err := r.Commit(libfossil.CommitOpts{
		Files:   []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("v1")}},
		Comment: "first",
		User:    "testuser",
	})
	if err != nil {
		t.Fatalf("Commit rid1: %v", err)
	}
	rid2, _, err := r.Commit(libfossil.CommitOpts{
		Files:    []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("v2")}},
		Comment:  "second",
		User:     "testuser",
		ParentID: rid1,
	})
	if err != nil {
		t.Fatalf("Commit rid2: %v", err)
	}
	r.Close()

	ckdb, err := libdb.OpenSQL(repoPath, libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatalf("open repo db: %v", err)
	}
	if _, err := ckdb.Exec("UPDATE event SET user=NULL WHERE objid=?", rid2); err != nil {
		ckdb.Close()
		t.Fatalf("UPDATE event SET user=NULL: %v", err)
	}
	ckdb.Close()

	stdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = pw

	cmd := &cli.RepoTimelineCmd{}
	runErr := cmd.Run(&cli.Globals{Repo: repoPath})

	pw.Close()
	os.Stdout = stdout
	var buf bytes.Buffer
	io.Copy(&buf, pr)

	if runErr != nil {
		t.Fatalf("RepoTimelineCmd.Run: %v", runErr)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "  ?  second") {
		t.Fatalf("NULL-user entry line = %q, want it to render user as %q", lines[0], "?")
	}
	if strings.Contains(lines[1], "  ?  ") {
		t.Fatalf("non-NULL-user entry line = %q, should not render %q", lines[1], "?")
	}
	if !strings.Contains(lines[1], "testuser") {
		t.Fatalf("non-NULL-user entry line = %q, want it to still show %q", lines[1], "testuser")
	}
}
