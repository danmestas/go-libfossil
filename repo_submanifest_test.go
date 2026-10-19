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
	if _, _, err := r.EnableSubmanifests(13); err == nil {
		t.Fatal("k 13 accepted")
	}
	k, warning, err := r.EnableSubmanifests(0)
	if err != nil || k != 1 || !strings.Contains(warning, "do not announce sub-manifest support") {
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
	if err := r.DisableSubmanifests(); err != nil {
		t.Fatal(err)
	}
	third, _, err := r.Commit(CommitOpts{Files: files[:1], Comment: "flat again", User: "u",
		Time: at.Add(2 * time.Hour), ParentID: second})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := manifest.GetManifest(r.Inner(), fsltype.FslID(third)); err != nil || len(d.S) != 0 {
		t.Fatalf("a tree after DisableSubmanifests (%v)", err)
	}
}

// Selection analyzes the main-branch tip, not the newest check-in: a newer
// one-file check-in on another branch would leave nothing to select from.
func TestEnableSubmanifestsAnalyzesTrunk(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "t.fossil"), CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	at := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	_, _, err = r.Commit(CommitOpts{Comment: "trunk", User: "u", Time: at, Files: []FileToCommit{
		{Name: "README.md", Content: []byte("# Example\n")},
		{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
		{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
		{Name: "src/util.c", Content: []byte("/* util */\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(CommitOpts{Comment: "branch", User: "u", Time: at.Add(time.Hour),
		Tags:  []TagSpec{{Name: "branch", Value: "side"}, {Name: "sym-side"}},
		Files: []FileToCommit{{Name: "only.txt", Content: []byte("x\n")}}}); err != nil {
		t.Fatal(err)
	}
	if k, _, err := r.EnableSubmanifests(0); err != nil || k != 1 {
		t.Fatalf("EnableSubmanifests(0) = %d, %v; want trunk's A.1 set selected at 1", k, err)
	}
}

// With no check-in there is nothing to select from, and the refusal leaves
// emission off.
func TestEnableSubmanifestsWithoutHistory(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "t.fossil"), CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, _, err := r.EnableSubmanifests(0); err == nil {
		t.Fatal("selection over no check-in succeeded")
	}
	if v, _ := r.Config("enable-submanifests"); v != "" {
		t.Fatalf("enable-submanifests = %q after a refusal", v)
	}
	if k, _, err := r.EnableSubmanifests(4); err != nil || k != 4 {
		t.Fatalf("explicit k = %d, %v", k, err)
	}
}

// With no check-in on the main branch -- everything on "main" while the
// setting names "trunk" -- selection falls back to the newest check-in.
func TestEnableSubmanifestsFallsBackToNewest(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "t.fossil"), CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	at := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	if _, _, err := r.Commit(CommitOpts{Comment: "main", User: "u", Time: at,
		Tags: []TagSpec{{Name: "branch", Value: "main"}, {Name: "sym-main"}},
		Files: []FileToCommit{
			{Name: "README.md", Content: []byte("# Example\n")},
			{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
			{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
			{Name: "src/util.c", Content: []byte("/* util */\n")},
		}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CurrentTrunk(); err == nil {
		t.Fatal("fixture: a trunk check-in resolves")
	}
	if k, _, err := r.EnableSubmanifests(0); err != nil || k != 1 {
		t.Fatalf("EnableSubmanifests(0) = %d, %v; want the newest check-in's A.1 set at 1", k, err)
	}
}
