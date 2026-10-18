package conformance

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	fsltype "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// zit as the oracle for go-libfossil's tree writer
// (draft-fossil-submanifests-00 §6). For each scenario -- a file set, its
// edits and a pinned k -- zit commits the files twice; go-libfossil then
// commits the same files with the comment, user and time zit recorded, and
// every check-in must come out byte-identical, so its sub-manifests do too.
// With zit present TestZitWriterInterop also has zit clone, verify and list
// go-libfossil's trees, and has go-libfossil commit a tree on top of zit's
// for zit to clone. CI has no zit, so TestZitWriterBundle replays the
// manifests zit wrote, recorded under zitWriterDir (ZIT_REGENERATE=1
// rewrites them).

const zitWriterDir = "testdata/zit-writer"

// zwFile is one file of a scenario commit.
type zwFile struct {
	name, content string
	exec          bool
}

// zwCommit is a scenario commit: its whole file set, and the renames it
// records, new name to old.
type zwCommit struct {
	comment string
	files   []zwFile
	renames map[string]string
}

type zwScenario struct {
	name    string
	k       int
	commits []zwCommit
}

// zwNames are the file sets the scenarios draw on, by name.
func zwNames(set string) []string {
	var names []string
	switch set {
	case "a1":
		return []string{"README.md", "src/main.c", "src/node11.c", "src/util.c"}
	case "small":
		for i := 0; i < 40; i++ {
			names = append(names, fmt.Sprintf("%s/f%02d.c", []string{"doc", "src", "src/sub", "test"}[i%4], i))
		}
	case "large":
		for i := 0; i < 3000; i++ {
			names = append(names, fmt.Sprintf("pkg%02d/sub%d/file%04d.go", i%37, i%5, i))
		}
	case "deep":
		for i := 0; i < 160; i++ {
			parts := []string{}
			for d := 0; d <= i%20; d++ {
				parts = append(parts, fmt.Sprintf("d%d", d))
			}
			names = append(names, strings.Join(append(parts, fmt.Sprintf("leaf%03d.c", i)), "/"))
		}
	case "edge":
		names = []string{"a b.c", "tab\there.c", "nl\nname.c", "ünï.c", "#hash.c", "-dash.c",
			"a", "a.b", "a-/b", "a b/c d.txt", "Upper.c", "lower.c", "日本語/ファイル.txt"}
		for i := 0; i < 60; i++ {
			names = append(names, fmt.Sprintf("bulk %02d/x\t%02d.c", i%6, i))
		}
	}
	return names
}

// zwSet is the first commit of a file set: every name holding "name v1\n",
// every tenth one executable.
func zwSet(set string) zwCommit {
	c := zwCommit{comment: "c1 " + set}
	for i, name := range zwNames(set) {
		c.files = append(c.files, zwFile{name: name, content: name + " v1\n", exec: i%10 == 7})
	}
	sortZwFiles(c.files)
	return c
}

func sortZwFiles(files []zwFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
}

// zwEdit is the second commit: every 23rd file edited, every 31st deleted,
// one renamed, and three added.
func zwEdit(set string, prev zwCommit) zwCommit {
	c := zwCommit{comment: "c2 " + set, renames: map[string]string{}}
	renamed := false
	for i, f := range prev.files {
		switch {
		case i%31 == 30:
			continue
		case i%23 == 11:
			f.content = f.name + " v2\n"
		case !renamed && i > len(prev.files)/2 && i%23 != 11:
			renamed = true
			c.renames["renamed/"+f.name] = f.name
			f.name = "renamed/" + f.name
		}
		c.files = append(c.files, f)
	}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("added/new %d.c", i)
		c.files = append(c.files, zwFile{name: name, content: name + "\n"})
	}
	sortZwFiles(c.files)
	return c
}

func zwScenarios() []zwScenario {
	two := func(set string) []zwCommit {
		first := zwSet(set)
		return []zwCommit{first, zwEdit(set, first)}
	}
	return []zwScenario{
		{name: "a1-k3", k: 3, commits: []zwCommit{zwSet("a1")}},
		{name: "small-k1", k: 1, commits: two("small")},
		{name: "small-k2", k: 2, commits: two("small")},
		{name: "large-k1", k: 1, commits: two("large")},
		{name: "large-k3", k: 3, commits: two("large")},
		{name: "large-k5", k: 5, commits: two("large")},
		{name: "small-k12", k: 12, commits: two("small")},
		{name: "deep-k2", k: 2, commits: two("deep")},
		{name: "edge-k1", k: 1, commits: two("edge")},
		{name: "edge-k3", k: 3, commits: two("edge")},
	}
}

// zwManifestFiles is a scenario commit as Checkin takes it.
func zwManifestFiles(c zwCommit) []manifest.File {
	var out []manifest.File
	for _, f := range c.files {
		mf := manifest.File{Name: f.name, Content: []byte(f.content), OldName: c.renames[f.name]}
		if f.exec {
			mf.Perm = "x"
		}
		out = append(out, mf)
	}
	return out
}

// zwReplay commits a scenario in a new go-libfossil repository with emission
// enabled at its k, taking each commit's comment, user and time from the
// manifest zit wrote, and requires every check-in byte-identical to zit's.
func zwReplay(t *testing.T, path string, sc zwScenario, zitManifests [][]byte) *repo.Repo {
	t.Helper()
	r, err := repo.Create(path, "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetHashPolicy("sha3"); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: strconv.Itoa(sc.k),
	} {
		if err := r.SetConfig(name, value); err != nil {
			t.Fatal(err)
		}
	}
	var parent fsltype.FslID
	for i, c := range sc.commits {
		want := zitManifests[i]
		zd, err := deck.Parse(want)
		if err != nil {
			t.Fatalf("%s c%d: zit's manifest: %v", sc.name, i+1, err)
		}
		rid, uuid, err := manifest.Checkin(r, manifest.CheckinOpts{Files: zwManifestFiles(c),
			Comment: zd.C, User: *zd.U, Time: zd.D, Parent: parent})
		if err != nil {
			t.Fatalf("%s c%d: Checkin: %v", sc.name, i+1, err)
		}
		if zitUUID := hash.SHA3(want); uuid != zitUUID {
			got, _ := manifest.GetManifest(r, rid)
			gotBytes, _ := got.Marshal()
			t.Fatalf("%s c%d: go-libfossil wrote %s, zit %s\n go  %q\n zit %q",
				sc.name, i+1, uuid[:12], zitUUID[:12], zwHead(gotBytes), zwHead(want))
		}
		parent = rid
	}
	return r
}

// zwHead is the first lines of an artifact, for a failure message.
func zwHead(data []byte) string {
	lines := strings.SplitN(string(data), "\n", 9)
	return strings.Join(lines[:len(lines)-1], "\n")
}

// zwTreeCount is the number of tree check-ins among a scenario's manifests.
func zwTreeCount(manifests [][]byte) int {
	n := 0
	for _, m := range manifests {
		if d, err := deck.Parse(m); err == nil && len(d.S) > 0 {
			n++
		}
	}
	return n
}

// zwWriteCommit writes a scenario commit's files into a zit checkout and
// records it, returning the manifest zit wrote.
func zwWriteCommit(t *testing.T, bin, home, work string, prev, c zwCommit) []byte {
	t.Helper()
	keep := map[string]bool{}
	for _, f := range c.files {
		keep[f.name] = true
	}
	for newName, oldName := range c.renames {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, newName)), 0o755); err != nil {
			t.Fatal(err)
		}
		zitRun(t, bin, home, work, "mv", oldName, newName)
	}
	for _, f := range prev.files {
		if !keep[f.name] && !isRenameSource(c, f.name) {
			if err := os.Remove(filepath.Join(work, f.name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range c.files {
		path := filepath.Join(work, f.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if f.exec {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(f.content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	zitRun(t, bin, home, work, "addremove")
	zitRun(t, bin, home, work, "commit", "-m", c.comment, "--user", "u", "--no-submanifest-advice")
	return []byte(zitRun(t, bin, home, work, "artifact", zitTip(t, bin, home, work), "--raw"))
}

func isRenameSource(c zwCommit, name string) bool {
	for _, old := range c.renames {
		if old == name {
			return true
		}
	}
	return false
}

// zwZitCommits has zit commit every commit of sc in a new repository at
// work and returns its manifests.
func zwZitCommits(t *testing.T, bin, home, work string, sc zwScenario) [][]byte {
	t.Helper()
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	zitRun(t, bin, home, work, "init")
	zitRun(t, bin, home, work, "submanifest", "enable", "--k", strconv.Itoa(sc.k))
	var out [][]byte
	prev := zwCommit{}
	for _, c := range sc.commits {
		out = append(out, zwWriteCommit(t, bin, home, work, prev, c))
		prev = c
	}
	return out
}

// zwAnalyzedOptimum is the k `zit submanifest analyze` reports as the
// analyzed optimum of the current check-in, 0 when it reports none.
func zwAnalyzedOptimum(t *testing.T, bin, home, work string) int {
	t.Helper()
	out := zitRun(t, bin, home, work, "submanifest", "analyze")
	m := regexp.MustCompile(`analyzed optimum: k=(\d+)`).FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	k, _ := strconv.Atoi(m[1])
	return k
}

// zwSelected is go-libfossil's §6.3 selection over a scenario commit.
func zwSelected(c zwCommit) int {
	var cards []deck.FileCard
	for _, f := range zwManifestFiles(c) {
		cards = append(cards, deck.FileCard{Name: f.Name, UUID: hash.SHA3(f.Content), Perm: f.Perm})
	}
	k, _ := manifest.SelectPartitionK(cards)
	return k
}

func TestZitWriterInterop(t *testing.T) {
	bin := zitBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	recorded := map[string][][]byte{}
	selections := map[string]int{}
	for _, sc := range zwScenarios() {
		work := filepath.Join(dir, sc.name)
		manifests := zwZitCommits(t, bin, home, work, sc)
		recorded[sc.name] = manifests
		r := zwReplay(t, filepath.Join(dir, sc.name+".fossil"), sc, manifests)
		if sc.name == "large-k3" || sc.name == "edge-k1" {
			zwZitReadsGoTrees(t, bin, home, dir, sc, r)
			zwGoCommitsOnZitTrees(t, bin, home, dir, work, sc)
		}
	}
	// §6.3: go-libfossil selects the k zit's analysis reports.
	for _, set := range []string{"a1", "small", "large", "deep", "edge"} {
		work := filepath.Join(dir, "select-"+set)
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		zitRun(t, bin, home, work, "init")
		zwWriteCommit(t, bin, home, work, zwCommit{}, zwSet(set))
		zk := zwAnalyzedOptimum(t, bin, home, work)
		if gk := zwSelected(zwSet(set)); gk != zk {
			t.Errorf("%s: go-libfossil selects k=%d, zit's analysis k=%d", set, gk, zk)
		}
		selections[set] = zk
	}
	if os.Getenv("ZIT_REGENERATE") == "1" {
		writeZitWriterBundle(t, recorded, selections)
	}
}

// zwZitReadsGoTrees serves go-libfossil's replayed trees to a zit clone,
// which must verify them and list every check-in as go-libfossil does.
func zwZitReadsGoTrees(t *testing.T, bin, home, dir string, sc zwScenario, r *repo.Repo) {
	t.Helper()
	pub, err := libfossil.Open(r.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	srv := httptest.NewServer(pub.XferHandler())
	defer srv.Close()
	clone := filepath.Join(dir, sc.name+"-zit-clone")
	zitRun(t, bin, home, dir, "clone", srv.URL, clone)
	zitRun(t, bin, home, clone, "verify")
	cis, err := checkinRIDs(r)
	if err != nil {
		t.Fatal(err)
	}
	for rid, uuid := range cis {
		got, err := goListing(r, rid)
		if err != nil {
			t.Fatal(err)
		}
		if want := zitListing(t, bin, home, clone, uuid); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s: zit lists go-libfossil's %s differently", sc.name, uuid[:10])
		}
	}
}

// zwGoCommitsOnZitTrees clones zit's tree repository into go-libfossil,
// commits a tree on top of zit's tip there, and has a zit clone of that
// verify it and list it as go-libfossil does.
func zwGoCommitsOnZitTrees(t *testing.T, bin, home, dir, work string, sc zwScenario) {
	t.Helper()
	url := zitServe(t, bin, home, work)
	path := filepath.Join(dir, sc.name+"-go-clone.fossil")
	cloned, _, err := libfossil.Clone(context.Background(), path, libfossil.NewHTTPTransport(url), libfossil.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone from zit: %v", err)
	}
	defer cloned.Close()
	tipUUID := zitTip(t, bin, home, work)
	tip, ok := blob.Exists(cloned.Inner().DB(), tipUUID)
	if !ok {
		t.Fatal("the clone lacks zit's tip")
	}
	if _, _, err := cloned.EnableSubmanifests(int64(tip), sc.k); err != nil {
		t.Fatal(err)
	}
	last := sc.commits[len(sc.commits)-1]
	files := zwManifestFiles(zwCommit{files: last.files})
	files[0].Content = []byte("edited by go-libfossil\n")
	// Millisecond time: zit refuses a D card with a longer fraction, which
	// a zero Time (time.Now) would write.
	rid, uuid, err := manifest.Checkin(cloned.Inner(), manifest.CheckinOpts{Files: files,
		Comment: "go-libfossil tree", User: "u", Parent: tip, Time: time.Now().UTC().Truncate(time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := manifest.GetManifest(cloned.Inner(), rid); err != nil || len(d.S) == 0 {
		t.Fatalf("go-libfossil's commit is not a tree (%v)", err)
	}
	srv := httptest.NewServer(cloned.XferHandler())
	defer srv.Close()
	clone := filepath.Join(dir, sc.name+"-zit-of-go")
	zitRun(t, bin, home, dir, "clone", srv.URL, clone)
	zitRun(t, bin, home, clone, "verify")
	got, err := goListing(cloned.Inner(), rid)
	if err != nil {
		t.Fatal(err)
	}
	if want := zitListing(t, bin, home, clone, uuid); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s: zit lists go-libfossil's commit on its tree differently", sc.name)
	}
}

// writeZitWriterBundle records every scenario's zit manifests and the
// selections for TestZitWriterBundle.
func writeZitWriterBundle(t *testing.T, recorded map[string][][]byte, selections map[string]int) {
	t.Helper()
	if err := os.RemoveAll(zitWriterDir); err != nil {
		t.Fatal(err)
	}
	for name, manifests := range recorded {
		for i, m := range manifests {
			path := filepath.Join(zitWriterDir, name, fmt.Sprintf("c%d.manifest", i+1))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, m, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	var b strings.Builder
	sets := make([]string, 0, len(selections))
	for set := range selections {
		sets = append(sets, set)
	}
	sort.Strings(sets)
	for _, set := range sets {
		fmt.Fprintf(&b, "%s %d\n", set, selections[set])
	}
	if err := os.WriteFile(filepath.Join(zitWriterDir, "selection.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestZitWriterBundle replays the recorded zit manifests: go-libfossil must
// write every one byte for byte, and select the k zit's analysis did.
func TestZitWriterBundle(t *testing.T) {
	trees := 0
	for _, sc := range zwScenarios() {
		var manifests [][]byte
		for i := range sc.commits {
			m, err := os.ReadFile(filepath.Join(zitWriterDir, sc.name, fmt.Sprintf("c%d.manifest", i+1)))
			if err != nil {
				t.Fatal(err)
			}
			manifests = append(manifests, m)
		}
		zwReplay(t, filepath.Join(t.TempDir(), sc.name+".fossil"), sc, manifests)
		trees += zwTreeCount(manifests)
	}
	if trees < 10 {
		t.Fatalf("only %d of the recorded check-ins are trees", trees)
	}
	data, err := os.ReadFile(filepath.Join(zitWriterDir, "selection.txt"))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		set, kText, _ := strings.Cut(sc.Text(), " ")
		zk, _ := strconv.Atoi(kText)
		if gk := zwSelected(zwSet(set)); gk != zk {
			t.Errorf("%s: go-libfossil selects k=%d, zit's analysis k=%d", set, gk, zk)
		}
	}
}
