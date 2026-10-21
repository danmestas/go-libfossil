package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// A missing structural node must refuse before even creating the export root,
// regardless of crosslinked rows that still describe its files.
func TestRepoCoMissingStructureNoDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repo.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	for key, value := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
	} {
		if err := r.Inner().SetConfig(key, value); err != nil {
			t.Fatal(err)
		}
	}
	var files []libfossil.FileToCommit
	for i := 0; i < 32; i++ {
		name := fmt.Sprintf("file%04d", i)
		files = append(files, libfossil.FileToCommit{Name: name, Content: []byte(name)})
	}
	rid, _, err := r.Commit(libfossil.CommitOpts{User: "u", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	root, err := manifest.GetManifest(r.Inner(), fsltype.FslID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if len(root.S) == 0 {
		t.Fatal("fixture requires actual submanifest structure")
	}
	if _, err := r.Inner().DB().Exec(
		"UPDATE blob SET size=-1, content=NULL WHERE uuid=?", root.S[0].UUID); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := (&RepoCoCmd{Dir: out, Force: true}).Run(&Globals{Repo: path}); err == nil {
		t.Fatal("missing structure unexpectedly exported")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("export created destination before structural refusal: %v", err)
	}
}
