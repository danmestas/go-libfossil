package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// What the operator sees and what crosses the wire when a horizon is chosen
// (draft-fossil-horizon-00 §3.1, §3.4): the CLI states the cost of the leaves
// rule, and a horizon is always a full hash and a rule name.

var (
	horizonLeavesWarning = Case{ID: "horizon.3.1.leaves-warning", Req: "HZ-3.1-2"}
	horizonNoCountOrDate = Case{ID: "horizon.3.4.no-count-or-date", Req: "HZ-3.4-2"}
)

func horizonCLICases() []Case { return []Case{horizonLeavesWarning, horizonNoCountOrDate} }

func TestHorizonCLI(t *testing.T) {
	check(t, horizonLeavesWarning, func() error { return hzLeavesWarning(t) })
	check(t, horizonNoCountOrDate, func() error { return hzNoCountOrDate(t) })
}

// hzStderr runs fn with os.Stderr redirected and returns what it wrote.
func hzStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	previous := os.Stderr
	os.Stderr = file
	runErr := fn()
	os.Stderr = previous
	out, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

// hzStdoutDiscard runs fn with os.Stdout discarded.
func hzStdoutDiscard(fn func() error) error {
	previous := os.Stdout
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	os.Stdout = null
	defer func() { os.Stdout = previous }()
	return fn()
}

// hzCLISource serves a two-check-in repository and returns the URL and the
// second check-in's hash.
func hzCLISource(t *testing.T) (url, c2 string) {
	t.Helper()
	source, err := gofossil.Create(filepath.Join(t.TempDir(), "source.fossil"), gofossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	var rid int64
	for _, body := range []string{"one\n", "two\n"} {
		var uuid string
		rid, uuid, err = source.Commit(gofossil.CommitOpts{User: "u", ParentID: rid,
			Files: []gofossil.FileToCommit{{Name: "a", Content: []byte(body)}}})
		if err != nil {
			t.Fatal(err)
		}
		c2 = uuid
	}
	server := httptest.NewServer(source.XferHandler())
	t.Cleanup(server.Close)
	return server.URL, c2
}

// --leaves, on a clone and on a store cone, warns once on stderr that the
// rule holds nearly the whole history of a repository with old unmerged
// branches; the descendants rule is silent.
func hzLeavesWarning(t *testing.T) error {
	url, c2 := hzCLISource(t)
	run := func(cmd interface{ Run(*cli.Globals) error }, g *cli.Globals) (string, error) {
		return hzStderr(t, func() error { return hzStdoutDiscard(func() error { return cmd.Run(g) }) })
	}
	clone := func(leaves bool) (string, string, error) {
		path := filepath.Join(t.TempDir(), "h.fossil")
		got, err := run(&cli.RepoCloneCmd{URL: url, Path: path, Horizon: c2, Leaves: leaves}, &cli.Globals{})
		return path, got, err
	}
	path, got, err := clone(true)
	if err != nil {
		return err
	}
	if err := hzWarned("clone --leaves", got); err != nil {
		return err
	}
	if _, got, err = clone(false); err != nil || got != "" {
		return expectf("clone without --leaves wrote %q to stderr (err %v)", got, err)
	}
	got, err = run(&cli.RepoConeStoreCmd{Horizon: c2, Leaves: true}, &cli.Globals{Repo: path})
	if err != nil {
		return err
	}
	return hzWarned("cone store --leaves", got)
}

func hzWarned(what, stderr string) error {
	if strings.Count(stderr, "warning:") != 1 || strings.Count(stderr, "\n") != 1 ||
		!strings.Contains(stderr, "nearly the whole history") {
		return expectf("%s stderr = %q, want one warning naming the cost", what, stderr)
	}
	return nil
}

var hzFullHash = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// hzWireHorizon fails unless every request of peer carries one horizon
// pragma that is a full hash and a rule name, and no other horizon pragma.
func hzWireHorizon(peer *scSyncPeer, wantHash string) error {
	if len(peer.requests) == 0 {
		return expectf("no requests recorded")
	}
	for i, req := range peer.requests {
		for _, card := range req.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && strings.HasPrefix(p.Name, "horizon") && p.Name != "horizon" {
				return expectf("request %d carries pragma %q", i, p.Name)
			}
		}
		got := hzPragmas(req, "horizon")
		if len(got) != 1 || len(got[0]) != 2 || !hzFullHash.MatchString(got[0][0]) ||
			(got[0][1] != "descendants" && got[0][1] != "leaves") {
			return expectf("request %d horizon pragmas = %q, want one full hash and a rule name", i, got)
		}
		if wantHash != "" && got[0][0] != wantHash {
			return expectf("request %d horizon %s, want %s", i, got[0][0], wantHash)
		}
	}
	return nil
}

// A horizon clone and a later pull send the horizon as a full hash and a
// rule, never a count or a date. The CLI's clone refuses a name outright; its
// store cone resolves a branch name to the full hash before any request.
func hzNoCountOrDate(t *testing.T) error {
	g := newHZSyncGraph(t)
	c := hzUUID(t, g.f.r, g.c3)
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		peer, r, _, err := hzServeClone(t, g, hzStoreCone(t, c, rule, cone.DepthContent), sync.HandleOpts{})
		if err != nil {
			return err
		}
		if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
			return err
		}
		if err := hzWireHorizon(peer, c); err != nil {
			return err
		}
	}

	cloneErr := (&cli.RepoCloneCmd{URL: "http://127.0.0.1:1", Path: filepath.Join(t.TempDir(), "x.fossil"),
		Horizon: "trunk"}).Run(&cli.Globals{})
	if cloneErr == nil || !strings.Contains(cloneErr.Error(), "full check-in hash") {
		return expectf("clone --horizon trunk = %v, want a refusal asking for the full hash", cloneErr)
	}

	path := filepath.Join(t.TempDir(), "named.fossil")
	dense := &scSyncPeer{r: g.f.r}
	r, _, err := sync.Clone(context.Background(), path, dense, sync.CloneOpts{})
	if err != nil {
		return err
	}
	if err := r.Close(); err != nil {
		return err
	}
	if err := hzStdoutDiscard(func() error {
		return (&cli.RepoConeStoreCmd{Horizon: "trunk"}).Run(&cli.Globals{Repo: path})
	}); err != nil {
		return err
	}
	r, err = repo.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	sc, err := content.StoreCone(r.DB())
	if err != nil {
		return err
	}
	h, ok := sc.Horizon()
	if !ok || !hzFullHash.MatchString(h.Hash()) {
		return expectf("store cone horizon after --horizon trunk = %q, want a full hash", h.Hash())
	}
	peer := &scSyncPeer{r: g.f.r}
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	return hzWireHorizon(peer, h.Hash())
}
