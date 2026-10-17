package conformance

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	fsltype "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/simio"
)

// Interoperation with zit, the other implementation of
// draft-fossil-submanifests-00 and the only writer of trees. With zit on
// PATH (or ZIT_BIN), TestZitInterop builds a history in go-libfossil, has zit
// re-mint it as sub-manifest trees, clones that over HTTP into go-libfossil,
// compares every check-in's file set with zit's own listing, verifies the
// clone, serves it back to a zit clone, and pulls a tree check-in zit commits
// afterwards. REQUIRE_ZIT_BIN=1 makes a missing zit a failure rather than a
// skip; CI has no zit, so there TestZitTreeBundle reads a recorded copy of
// what zit wrote instead. ZIT_REGENERATE=1 rewrites that copy.

const zitBundleDir = "testdata/zit-trees"

func zitBinary(t *testing.T) string {
	t.Helper()
	name := os.Getenv("ZIT_BIN")
	if name == "" {
		name = "zit"
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("REQUIRE_ZIT_BIN") == "1" {
			t.Fatal("REQUIRE_ZIT_BIN=1 but no zit binary found")
		}
		t.Skip("zit not found: the interop run needs it")
	}
	return bin
}

// zitRun runs zit in dir with a private home and a deadline.
func zitRun(t *testing.T, bin, home, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "USER=u")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("zit %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// zitServe starts `zit serve` in dir on a free port, stopped at cleanup and
// in any case by the context's deadline.
func zitServe(t *testing.T, bin, home, dir string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	cmd := exec.CommandContext(ctx, bin, "serve", "--port", fmt.Sprint(port))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "USER=u")
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return "http://" + addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("zit serve did not start")
	return ""
}

// zitSourceHistory writes a flat history with edits, a delete, a rename, a
// branch and a merge, served by go-libfossil for zit to import.
func zitSourceHistory(t *testing.T, path string) {
	t.Helper()
	r, err := repo.Create(path, "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	files := map[string]string{}
	for _, dir := range []string{"doc", "src", "src/sub", "test"} {
		for i := 0; i < 6; i++ {
			name := fmt.Sprintf("%s/f%d.c", dir, i)
			files[name] = name + " v1\n"
		}
	}
	files["README"] = "readme\n"
	commit := func(comment string, parent fsltype.FslID, merge ...fsltype.FslID) fsltype.FslID {
		var list []manifest.File
		for name, text := range files {
			list = append(list, manifest.File{Name: name, Content: []byte(text)})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		at = at.Add(time.Hour)
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{Files: list, Comment: comment,
			User: "u", Time: at, Parent: parent, MergeParents: merge})
		if err != nil {
			t.Fatalf("Checkin %s: %v", comment, err)
		}
		return rid
	}
	one := commit("one", 0)
	files["src/f1.c"] = "src/f1.c v2\n"
	delete(files, "test/f5.c")
	files["doc/moved.c"] = files["doc/f0.c"]
	delete(files, "doc/f0.c")
	two := commit("two", one)
	files["src/sub/f3.c"] = "branch edit\n"
	three := commit("three", two)
	files["README"] = "readme 4\n"
	commit("four", three, one)
}

// zitListing is zit's `ls --long` of a check-in as "name hash" lines.
func zitListing(t *testing.T, bin, home, dir, uuid string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(zitRun(t, bin, home, dir, "ls", "--long", uuid)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		out = append(out, strings.Join(fields[4:], " ")+" "+fields[0])
	}
	sort.Strings(out)
	return out
}

// goListing is ListFiles of a check-in in the same form.
func goListing(r *repo.Repo, rid fsltype.FslID) ([]string, error) {
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		out = append(out, f.Name+" "+f.UUID)
	}
	sort.Strings(out)
	return out, nil
}

// checkinRIDs lists the check-ins of r with their names.
func checkinRIDs(r *repo.Repo) (map[fsltype.FslID]string, error) {
	rows, err := r.DB().Query(`SELECT e.objid, b.uuid FROM event e JOIN blob b ON b.rid=e.objid
		WHERE e.type='ci'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[fsltype.FslID]string{}
	for rows.Next() {
		var rid int64
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return nil, err
		}
		out[fsltype.FslID(rid)] = uuid
	}
	return out, rows.Err()
}

// treeShape counts the tree manifests and interior sub-manifests r holds.
func treeShape(r *repo.Repo) (trees, interiors int, err error) {
	rows, err := r.DB().Query("SELECT rid FROM blob WHERE size>=0")
	if err != nil {
		return 0, 0, err
	}
	var rids []fsltype.FslID
	for rows.Next() {
		var rid int64
		rows.Scan(&rid)
		rids = append(rids, fsltype.FslID(rid))
	}
	rows.Close()
	for _, rid := range rids {
		data, err := content.Expand(r.DB(), rid)
		if err != nil {
			return 0, 0, err
		}
		d, err := deck.Parse(data)
		if err != nil {
			continue
		}
		switch {
		case d.Type == deck.Checkin && len(d.S) > 0:
			trees++
		case d.Type == deck.Submanifest && len(d.S) > 0:
			interiors++
		}
	}
	return trees, interiors, nil
}

// checkTreeRepo compares every check-in's file set with want, verifies the
// repository and every R card, and requires trees with interior nodes.
func checkTreeRepo(t *testing.T, r *repo.Repo, want func(uuid string) []string) {
	t.Helper()
	cis, err := checkinRIDs(r)
	if err != nil {
		t.Fatal(err)
	}
	for rid, uuid := range cis {
		got, err := goListing(r, rid)
		if err != nil {
			t.Fatalf("ListFiles %s: %v", uuid, err)
		}
		if w := want(uuid); strings.Join(got, "\n") != strings.Join(w, "\n") {
			t.Errorf("check-in %s:\n go  %q\n zit %q", uuid[:10], got, w)
		}
		d, err := manifest.GetManifest(r, rid)
		if err != nil {
			t.Fatal(err)
		}
		if d.R != "" {
			if err := r.WithTx(func(tx *db.Tx) error { return manifest.VerifyRCard(tx, nil, rid) }); err != nil {
				t.Errorf("R card of %s: %v", uuid[:10], err)
			}
		}
	}
	trees, interiors, err := treeShape(r)
	if err != nil {
		t.Fatal(err)
	}
	if trees < 4 || interiors == 0 {
		t.Fatalf("%d tree manifests and %d interior sub-manifests; the fixture should exercise both", trees, interiors)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("verify: %+v", report.Issues)
	}
}

func TestZitInterop(t *testing.T) {
	bin := zitBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}

	// A flat history served by go-libfossil, re-minted by zit as trees at
	// k = 1, which yields interior sub-manifests on a few dozen files.
	srcPath := filepath.Join(dir, "src.fossil")
	zitSourceHistory(t, srcPath)
	src, err := libfossil.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	srcServer := httptest.NewServer(src.XferHandler())
	defer srcServer.Close()
	trees := filepath.Join(dir, "trees")
	zitRun(t, bin, home, dir, "import-fossil", srcServer.URL, trees, "--submanifest", "--k", "1")
	zitURL := zitServe(t, bin, home, trees)

	// Clone zit's trees into go-libfossil over HTTP.
	clonePath := filepath.Join(dir, "clone.fossil")
	cloned, _, err := libfossil.Clone(context.Background(), clonePath, libfossil.NewHTTPTransport(zitURL), libfossil.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone from zit: %v", err)
	}
	listing := func(uuid string) []string { return zitListing(t, bin, home, trees, uuid) }
	checkTreeRepo(t, cloned.Inner(), listing)

	if os.Getenv("ZIT_REGENERATE") == "1" {
		writeZitBundle(t, cloned.Inner())
	}

	// Serve the clone back: a zit clone of it verifies and lists the same tip.
	back := httptest.NewServer(cloned.XferHandler())
	defer back.Close()
	relay := filepath.Join(dir, "relay")
	zitRun(t, bin, home, dir, "clone", back.URL, relay)
	zitRun(t, bin, home, relay, "verify")
	tip := zitTip(t, bin, home, trees)
	if a, b := listing(tip), zitListing(t, bin, home, relay, tip); strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("zit's clone of go-libfossil's clone lists the tip differently")
	}

	// zit commits a new tree check-in; go-libfossil pulls it.
	work := filepath.Join(dir, "work")
	zitRun(t, bin, home, dir, "clone", zitURL, work)
	zitRun(t, bin, home, work, "submanifest", "enable", "--k", "1")
	if err := os.WriteFile(filepath.Join(work, "src", "f2.c"), []byte("pulled edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zitRun(t, bin, home, work, "commit", "-m", "tree commit")
	zitRun(t, bin, home, work, "push", zitURL)
	if _, err := cloned.Pull(context.Background(), zitURL, libfossil.PullOpts{}); err != nil {
		t.Fatalf("Pull from zit: %v", err)
	}
	newTip := zitTip(t, bin, home, trees)
	if newTip == tip {
		t.Fatal("zit's commit did not reach its server")
	}
	checkTreeRepo(t, cloned.Inner(), listing)

	// zit pushes a second tree check-in to go-libfossil's server, which
	// stores and links it as it arrives.
	if err := os.WriteFile(filepath.Join(work, "doc", "f1.c"), []byte("pushed edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zitRun(t, bin, home, work, "commit", "-m", "pushed tree")
	if _, err := cloned.Inner().DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	zitRun(t, bin, home, work, "push", back.URL)
	pushed := zitTip(t, bin, home, work)
	rid, ok := blob.Exists(cloned.Inner().DB(), pushed)
	if !ok {
		t.Fatal("go-libfossil's server did not receive zit's pushed tree check-in")
	}
	got, err := goListing(cloned.Inner(), rid)
	if err != nil {
		t.Fatalf("ListFiles of the pushed tree: %v", err)
	}
	if want := zitListing(t, bin, home, work, pushed); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pushed tree lists %q, zit %q", got, want)
	}
	if err := cloned.Close(); err != nil {
		t.Fatal(err)
	}
}

// zitTip is the name of the newest check-in zit holds in dir: the first
// line of `zit info tip`.
func zitTip(t *testing.T, bin, home, dir string) string {
	t.Helper()
	fields := strings.Fields(zitRun(t, bin, home, dir, "info", "tip"))
	if len(fields) == 0 || len(fields[0]) < 40 {
		t.Fatalf("no hash in zit info tip: %q", fields)
	}
	return fields[0]
}

// writeZitBundle records every artifact of r, and each check-in's listing,
// under zitBundleDir for TestZitTreeBundle.
func writeZitBundle(t *testing.T, r *repo.Repo) {
	t.Helper()
	if err := os.RemoveAll(zitBundleDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(zitBundleDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows, err := r.DB().Query("SELECT rid, uuid FROM blob WHERE size>=0 ORDER BY uuid")
	if err != nil {
		t.Fatal(err)
	}
	type art struct {
		rid  fsltype.FslID
		uuid string
	}
	var arts []art
	for rows.Next() {
		var a art
		var rid int64
		rows.Scan(&rid, &a.uuid)
		a.rid = fsltype.FslID(rid)
		arts = append(arts, a)
	}
	rows.Close()
	for _, a := range arts {
		data, err := content.Expand(r.DB(), a.rid)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(zitBundleDir, "artifacts", a.uuid), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cis, err := checkinRIDs(r)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	var uuids []string
	byUUID := map[string]fsltype.FslID{}
	for rid, uuid := range cis {
		uuids = append(uuids, uuid)
		byUUID[uuid] = rid
	}
	sort.Strings(uuids)
	for _, uuid := range uuids {
		list, err := goListing(r, byUUID[uuid])
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "check-in %s\n", uuid)
		for _, line := range list {
			fmt.Fprintf(&b, "%s\n", line)
		}
	}
	if err := os.WriteFile(filepath.Join(zitBundleDir, "listings.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestZitTreeBundle reads the recorded zit artifacts, in an order that puts
// each tree before the sub-manifests it names, so the deferral and retry of
// §7.3 run on real zit output; then compares every check-in with the
// listing recorded from zit.
func TestZitTreeBundle(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(zitBundleDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Create(filepath.Join(t.TempDir(), "bundle.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.SetHashPolicy("sha3"); err != nil {
		t.Fatal(err)
	}
	// Manifests first, then everything else, each batch swept: a tree
	// arrives before its sub-manifests and is linked only once they do.
	var manifests, rest [][]byte
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(zitBundleDir, "artifacts", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if d, err := deck.Parse(data); err == nil && d.Type == deck.Checkin {
			manifests = append(manifests, data)
		} else {
			rest = append(rest, data)
		}
	}
	for _, batch := range [][][]byte{manifests, rest} {
		for _, data := range batch {
			if err := r.WithTx(func(tx *db.Tx) error {
				_, _, err := blob.Store(tx, data)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := manifest.Crosslink(r); err != nil {
			t.Fatal(err)
		}
	}
	want, err := readZitListings()
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("no recorded listings")
	}
	checkTreeRepo(t, r, func(uuid string) []string { return want[uuid] })
}

func readZitListings() (map[string][]string, error) {
	data, err := os.ReadFile(filepath.Join(zitBundleDir, "listings.txt"))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	var current string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if uuid, ok := strings.CutPrefix(line, "check-in "); ok {
			current = uuid
			out[current] = []string{}
			continue
		}
		out[current] = append(out[current], line)
	}
	return out, sc.Err()
}
