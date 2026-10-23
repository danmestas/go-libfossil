package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// horizonSource serves three check-ins; "old a" is named only by the first, so
// a horizon at the second elides it.
func horizonSource(t *testing.T) (url, c1, c2 string, requests *atomic.Int64) {
	t.Helper()
	source, err := libfossil.Create(filepath.Join(t.TempDir(), "source.fossil"), libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	commit := func(parent int64, a string) (int64, string) {
		rid, uuid, err := source.Commit(libfossil.CommitOpts{User: "test", ParentID: parent, Files: []libfossil.FileToCommit{
			{Name: "a", Content: []byte(a)}, {Name: "b", Content: []byte("shared b\n")},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return rid, uuid
	}
	r1, c1 := commit(0, "old a\n")
	r2, c2 := commit(r1, "new a\n")
	commit(r2, "newest a\n")
	requests = new(atomic.Int64)
	server := requiredHTTPServer(t, source, requests)
	t.Cleanup(server.Close)
	return server.URL, c1, c2, requests
}

func holdsBlob(t *testing.T, path, data string) bool {
	t.Helper()
	r, err := libfossil.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, held, err := content.CheckAvailableByUUID(r.Inner().DB(), hash.SHA1([]byte(data)))
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func storeConeOutput(t *testing.T, path string) string {
	t.Helper()
	return requiredHTTPStdout(t, func() error {
		return (&RepoConeStoreCmd{}).Run(&Globals{Repo: path})
	})
}

func TestRepoCloneHorizon(t *testing.T) {
	url, _, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "h.fossil")
	out := requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path, Horizon: strings.ToUpper(c2), Leaves: true}).Run(&Globals{})
	})
	if !strings.Contains(out, "Elided:") {
		t.Fatalf("clone output = %q", out)
	}
	if holdsBlob(t, path, "old a\n") || !holdsBlob(t, path, "new a\n") {
		t.Fatal("clone did not cut at the horizon")
	}
	if got, want := storeConeOutput(t, path), "horizon "+c2+" leaves\n"; got != want {
		t.Fatalf("store cone = %q, want %q", got, want)
	}
}

func TestRepoCloneHorizonNeedsFullHash(t *testing.T) {
	url, _, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "h.fossil")
	err := (&RepoCloneCmd{URL: url, Path: path, Horizon: c2[:10]}).Run(&Globals{})
	if err == nil || !strings.Contains(err.Error(), "full check-in hash") {
		t.Fatalf("error = %v", err)
	}
	err = (&RepoCloneCmd{URL: url, Path: path, Leaves: true}).Run(&Globals{})
	if err == nil || !strings.Contains(err.Error(), "--leaves needs --horizon") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepoConeStoreHorizonLifecycle(t *testing.T) {
	url, c1, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "h.fossil")
	if err := (&RepoCloneCmd{URL: url, Path: path, Cone: []string{"a"}, Horizon: c2}).Run(&Globals{}); err != nil {
		t.Fatal(err)
	}
	if got, want := storeConeOutput(t, path), "horizon "+c2+" descendants\na\n"; got != want {
		t.Fatalf("store cone = %q, want %q", got, want)
	}
	g := &Globals{Repo: path}
	// Deepen by an abbreviation: resolved locally to the full hash.
	out := requiredHTTPStdout(t, func() error {
		return (&RepoConeStoreCmd{Horizon: c1[:12], Leaves: true}).Run(g)
	})
	if !strings.HasPrefix(out, "fetched ") || out == "fetched 0 artifact(s)\n" {
		t.Fatalf("deepen output = %q", out)
	}
	if !holdsBlob(t, path, "old a\n") {
		t.Fatal("deepening did not fetch the newly kept blob")
	}
	if got, want := storeConeOutput(t, path), "horizon "+c1+" leaves\na\n"; got != want {
		t.Fatalf("store cone = %q, want %q", got, want)
	}
	if err := (&RepoConeStoreCmd{NoHorizon: true}).Run(g); err != nil {
		t.Fatal(err)
	}
	if got := storeConeOutput(t, path); got != "a\n" {
		t.Fatalf("after --no-horizon = %q", got)
	}
	if err := (&RepoConeStoreCmd{NoHorizon: true}).Run(g); err == nil {
		t.Fatal("--no-horizon without a horizon succeeded")
	}
}

func TestRepoConeStoreHorizonOnlyAndDense(t *testing.T) {
	url, _, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "h.fossil")
	if err := (&RepoCloneCmd{URL: url, Path: path, Horizon: c2}).Run(&Globals{}); err != nil {
		t.Fatal(err)
	}
	g := &Globals{Repo: path}
	err := (&RepoConeStoreCmd{NoHorizon: true}).Run(g)
	if err == nil || !strings.Contains(err.Error(), "--dense") {
		t.Fatalf("error = %v", err)
	}
	if err := (&RepoConeStoreCmd{Dense: true}).Run(g); err != nil {
		t.Fatal(err)
	}
	if got := storeConeOutput(t, path); got != "dense\n" {
		t.Fatalf("after --dense = %q", got)
	}
	if !holdsBlob(t, path, "old a\n") {
		t.Fatal("--dense did not fetch the elided blob")
	}
}

func TestRepoConeStoreHorizonFlagErrors(t *testing.T) {
	url, _, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "h.fossil")
	if err := (&RepoCloneCmd{URL: url, Path: path}).Run(&Globals{}); err != nil {
		t.Fatal(err)
	}
	g := &Globals{Repo: path}
	for name, cmd := range map[string]*RepoConeStoreCmd{
		"dense+horizon":      {Dense: true, Horizon: c2},
		"horizon+no-horizon": {Horizon: c2, NoHorizon: true},
		"leaves alone":       {Leaves: true},
		"unknown check-in":   {Horizon: "no-such-branch"},
	} {
		if err := cmd.Run(g); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// Setting a horizon on a dense store, with a path replaced by --horizon.
	if err := (&RepoConeStoreCmd{Horizon: c2}).Run(g); err != nil {
		t.Fatal(err)
	}
	if got, want := storeConeOutput(t, path), "horizon "+c2+" descendants\n"; got != want {
		t.Fatalf("store cone = %q, want %q", got, want)
	}
}

// captureStderr runs run with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, run func() error) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	previous := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = previous }()
	if err := run(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// §3.1: the leaves rule is chosen knowingly, so --leaves states its cost once
// on stderr, and the descendants rule says nothing.
func TestHorizonLeavesWarning(t *testing.T) {
	url, _, c2, _ := horizonSource(t)
	clone := func(leaves bool) string {
		path := filepath.Join(t.TempDir(), "h.fossil")
		return captureStderr(t, func() error {
			captured := requiredHTTPStdout(t, func() error {
				return (&RepoCloneCmd{URL: url, Path: path, Horizon: c2, Leaves: leaves}).Run(&Globals{})
			})
			_ = captured
			return nil
		})
	}
	got := clone(true)
	if strings.Count(got, "warning:") != 1 || !strings.Contains(got, "--leaves") ||
		!strings.Contains(got, "nearly the whole history") {
		t.Fatalf("clone --leaves stderr = %q", got)
	}
	if got := clone(false); got != "" {
		t.Fatalf("clone without --leaves wrote %q to stderr", got)
	}

	path := filepath.Join(t.TempDir(), "s.fossil")
	requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path, Horizon: c2}).Run(&Globals{})
	})
	got = captureStderr(t, func() error {
		requiredHTTPStdout(t, func() error {
			return (&RepoConeStoreCmd{Horizon: c2, Leaves: true}).Run(&Globals{Repo: path})
		})
		return nil
	})
	if strings.Count(got, "warning:") != 1 || !strings.Contains(got, "nearly the whole history") {
		t.Fatalf("cone store --leaves stderr = %q", got)
	}
}

// TestRepoConeCost: the command reports what each rule would hold for a
// horizon at a check-in (§3.4), from the local repository. The fork off the
// first check-in is a childless leaf whose ancestry meets the horizon's at
// that check-in, so the leaves rule holds all four check-ins where the
// descendants rule holds two.
func TestRepoConeCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	commit := func(parent int64, a string) (int64, string) {
		rid, uuid, err := r.Commit(libfossil.CommitOpts{User: "test", ParentID: parent, Files: []libfossil.FileToCommit{
			{Name: "a", Content: []byte(a)}, {Name: "b", Content: []byte("shared b\n")},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return rid, uuid
	}
	r1, _ := commit(0, "old a\n")
	r2, c2 := commit(r1, "new a\n")
	commit(r2, "newest a\n")
	commit(r1, "feat a\n")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	out := requiredHTTPStdout(t, func() error {
		return (&RepoConeCostCmd{Checkin: c2}).Run(&Globals{Repo: path})
	})
	for _, line := range []string{
		"descendants  2 check-ins, 3 blobs, 24 B expanded, ",
		"leaves  4 check-ins, 5 blobs, 37 B expanded, ",
	} {
		if !strings.Contains(out, "\n"+line) {
			t.Fatalf("output = %q, want a line starting %q", out, line)
		}
	}
	if err := (&RepoConeCostCmd{Checkin: "no-such-name"}).Run(&Globals{Repo: path}); err == nil {
		t.Fatal("an unknown check-in was accepted")
	}
}

// A horizon clone, and a deepen, print the history URL the origin names in
// horizon-served (draft-fossil-horizon-00 §6.2.5); an origin naming none
// gets no such line.
func TestRepoCloneHistoryURL(t *testing.T) {
	source, err := libfossil.Create(filepath.Join(t.TempDir(), "source.fossil"), libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var parent int64
	var names []string
	for _, a := range []string{"old a\n", "new a\n"} {
		rid, uuid, err := source.Commit(libfossil.CommitOpts{User: "test", ParentID: parent,
			Files: []libfossil.FileToCommit{{Name: "a", Content: []byte(a)}}})
		if err != nil {
			t.Fatal(err)
		}
		parent, names = rid, append(names, uuid)
	}
	server := requiredHTTPServer(t, source, new(atomic.Int64))
	defer server.Close()
	clone := func() (string, string) {
		path := filepath.Join(t.TempDir(), "h.fossil")
		return path, requiredHTTPStdout(t, func() error {
			return (&RepoCloneCmd{URL: server.URL, Path: path, Horizon: names[1]}).Run(&Globals{})
		})
	}
	if _, out := clone(); strings.Contains(out, "History:") {
		t.Fatalf("clone from an origin naming no history URL printed one:\n%s", out)
	}
	const url = "https://example.org/history"
	if err := source.SetConfig(libfossil.SettingHorizonHistoryURL, url); err != nil {
		t.Fatal(err)
	}
	path, out := clone()
	if !strings.Contains(out, "  History:      "+url+"\n") {
		t.Fatalf("clone output lacks the history URL:\n%s", out)
	}
	out = requiredHTTPStdout(t, func() error {
		return (&RepoConeDeepenCmd{Checkin: names[0]}).Run(&Globals{Repo: path})
	})
	if !strings.Contains(out, "history: "+url+"\n") {
		t.Fatalf("deepen output lacks the history URL:\n%s", out)
	}
}
