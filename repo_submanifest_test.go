package libfossil

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	fsltype "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// EnableSubmanifests pins k, turns emission on and hands back the operator
// warning; later commits are trees, and RetuneSubmanifests moves the pin.
func TestEnableSubmanifestsWritesTrees(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "t.fossil"), CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	files := []FileToCommit{
		{Name: "README.md", Content: []byte("# Example\n")},
		{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
		{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
		{Name: "src/util.c", Content: []byte("/* util */\n")},
	}
	at := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	first, _, err := r.Commit(CommitOpts{Files: files, Comment: "flat", User: "u", Time: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.EnableSubmanifests(first, 13); err == nil {
		t.Fatal("k 13 accepted")
	}
	k, warning, err := r.EnableSubmanifests(first, 0)
	if err != nil || k != 1 || !strings.Contains(warning, "Fossil 2.29") {
		t.Fatalf("EnableSubmanifests = %d, %q, %v", k, warning, err)
	}
	if err := r.RetuneSubmanifests(3); err != nil {
		t.Fatal(err)
	}
	second, _, err := r.Commit(CommitOpts{Files: files[:1], Comment: "tree", User: "u",
		Time: at.Add(time.Hour), ParentID: first})
	if err != nil {
		t.Fatal(err)
	}
	d, err := manifest.GetManifest(r.Inner(), fsltype.FslID(second))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.S) != 2 || len(d.F) != 0 {
		t.Fatalf("second commit: %d S cards, %d F cards", len(d.S), len(d.F))
	}
}
