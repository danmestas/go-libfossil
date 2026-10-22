package cli

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// A structural clone (draft-fossil-horizon-00 §5): --structural takes the
// structural cut and the clone says so; the store cone shows it; info names
// the horizon and the check-ins below it rather than leaving bare phantoms;
// verify counts the manifests below the horizon as elided, never as damage
// (§5.4); and checking out, diffing or annotating a check-in below C fails
// naming the horizon (§5.5).
func TestRepoCloneStructural(t *testing.T) {
	url, c1, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "s.fossil")
	if err := (&RepoCloneCmd{URL: url, Path: path, Structural: true}).Run(&Globals{}); err == nil {
		t.Fatal("--structural without --horizon was accepted")
	}
	out := requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path, Horizon: c2, Structural: true}).Run(&Globals{})
	})
	if !strings.Contains(out, "Horizon:      structural\n") {
		t.Fatalf("clone output = %q", out)
	}
	if got, want := storeConeOutput(t, path), "horizon "+c2+" descendants structural\n"; got != want {
		t.Fatalf("store cone = %q, want %q", got, want)
	}
	info := requiredHTTPStdout(t, func() error { return (&RepoInfoCmd{}).Run(&Globals{Repo: path}) })
	for _, line := range []string{"horizon:       " + c2 + " descendants structural\n", "below horizon: 1 check-in(s)\n"} {
		if !strings.Contains(info, line) {
			t.Fatalf("info lacks %q:\n%s", line, info)
		}
	}
	var verifyErr error
	verified := requiredHTTPStdout(t, func() error {
		verifyErr = (&RepoVerifyCmd{}).Run(&Globals{Repo: path})
		return nil
	})
	if verifyErr != nil || !strings.Contains(verified, "0 transit phantoms") ||
		!strings.Contains(verified, "1 check-in(s) below the horizon") {
		t.Fatalf("verify: %v\n%s", verifyErr, verified)
	}
	for name, run := range map[string]func() error{
		"co":       func() error { return (&RepoCoCmd{Version: c1, Dir: t.TempDir()}).Run(&Globals{Repo: path}) },
		"diff":     func() error { return (&RepoDiffCmd{Version: c1, To: c2}).Run(&Globals{Repo: path}) },
		"annotate": func() error { return (&RepoAnnotateCmd{File: "a", Version: c1}).Run(&Globals{Repo: path}) },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "below the structural horizon at "+c2) {
			t.Errorf("%s below C: %v, want a diagnostic naming the horizon", name, err)
		}
	}
}

// `repo cone deepen` moves a structural store's horizon to a check-in below
// it, from the saved remote, and keeps the structural depth
// (draft-fossil-horizon-00 §5.8); the check-in below the old horizon can
// then be checked out.
func TestRepoConeDeepen(t *testing.T) {
	url, c1, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "s.fossil")
	requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path, Horizon: c2, Structural: true}).Run(&Globals{})
	})
	out := requiredHTTPStdout(t, func() error {
		return (&RepoConeDeepenCmd{Checkin: c1}).Run(&Globals{Repo: path})
	})
	if !strings.Contains(out, "horizon "+c1+" descendants structural\n") {
		t.Fatalf("deepen output = %q", out)
	}
	if got, want := storeConeOutput(t, path), "horizon "+c1+" descendants structural\n"; got != want {
		t.Fatalf("store cone = %q, want %q", got, want)
	}
	if !holdsBlob(t, path, "old a\n") {
		t.Fatal("the deepen fetched no blob the deeper horizon keeps")
	}
	requiredHTTPStdout(t, func() error {
		return (&RepoCoCmd{Version: c1, Dir: t.TempDir()}).Run(&Globals{Repo: path})
	})
}

// structuralMergeSource serves a history where a merge's base lies below a
// structural horizon at c2: f forks from c1 below it and g follows f, x
// merges c2 into g and y merges g into a, c2's child. fossil's merge base of
// x and y, their most recent common ancestor, is g.
func structuralMergeSource(t *testing.T) (url, c2, f, x, y string) {
	t.Helper()
	source, err := libfossil.Create(filepath.Join(t.TempDir(), "source.fossil"), libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	commit := func(a string, parents ...int64) (int64, string) {
		rid, uuid, err := source.Commit(libfossil.CommitOpts{User: "test", ParentID: parents[0], MergeParents: parents[1:],
			Files: []libfossil.FileToCommit{{Name: "a", Content: []byte(a)}, {Name: "b", Content: []byte("shared b\n")}}})
		if err != nil {
			t.Fatal(err)
		}
		return rid, uuid
	}
	r1, _ := commit("one\n", 0)
	rf, _ := commit("one\nfork\n", r1)
	r2, c2 := commit("zero\none\n", r1)
	ra, _ := commit("zero\none\nA\n", r2)
	rg, f := commit("one\nfork\ng\n", rf)
	_, x = commit("zero\none\nfork\ng\n", rg, r2)
	_, y = commit("zero\none\nA\nfork\ng\n", ra, rg)
	server := requiredHTTPServer(t, source, new(atomic.Int64))
	t.Cleanup(server.Close)
	return server.URL, c2, f, x, y
}

// `repo merge` across a structural horizon asks the saved remote for the
// base, deepens to reach it and merges over the real base
// (draft-fossil-horizon-00 §5.5); it never merges against a base it does
// not hold.
func TestRepoMergeDeepensAcrossHorizon(t *testing.T) {
	url, c2, f, x, y := structuralMergeSource(t)
	path := filepath.Join(t.TempDir(), "s.fossil")
	requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path, Horizon: c2, Structural: true}).Run(&Globals{})
	})
	dir := t.TempDir()
	r, err := libfossil.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	co, err := r.CreateCheckout(dir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	xrid, err := r.ResolveVersion(x)
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(xrid, libfossil.ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	co.Close()
	r.Close()
	out := requiredHTTPStdout(t, func() error {
		return (&RepoMergeCmd{Version: y, Dir: dir}).Run(&Globals{Repo: path})
	})
	if !strings.Contains(out, "MERGE a\n") {
		t.Fatalf("merge output = %q", out)
	}
	if got := storeConeOutput(t, path); !strings.Contains(got, "structural") || strings.Contains(got, c2) {
		t.Fatalf("store cone after the merge = %q, want a deeper structural horizon", got)
	}
	if !holdsBlob(t, path, "one\nfork\ng\n") {
		t.Fatalf("the base %s's content is not held after the merge", f)
	}
}

// `repo verify --seed` verifies the structural store's seed by deepening
// (draft-fossil-horizon-00 §5.9): it fetches the history below the horizon
// from the saved remote, compares, and leaves a content-horizon store.
func TestRepoVerifySeed(t *testing.T) {
	url, _, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "s.fossil")
	requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path, Horizon: c2, Structural: true}).Run(&Globals{})
	})
	out := requiredHTTPStdout(t, func() error { return (&RepoVerifyCmd{Seed: true}).Run(&Globals{Repo: path}) })
	if !strings.Contains(out, "seed verified:") || !strings.Contains(out, "repository integrity verified") {
		t.Fatalf("verify --seed output = %q", out)
	}
	if got, want := storeConeOutput(t, path), "horizon "+c2+" descendants\n"; got != want {
		t.Fatalf("store cone after verify --seed = %q, want %q", got, want)
	}
}

// A symbolic name on a check-in below the structural horizon resolves to
// that check-in, because the tag's control artifact is held, and operating
// on it fails naming the horizon and the deepen (draft-fossil-horizon-00
// §5.5).
func TestRepoNameBelowHorizon(t *testing.T) {
	source, err := libfossil.Create(filepath.Join(t.TempDir(), "source.fossil"), libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	r1, c1, err := source.Commit(libfossil.CommitOpts{User: "test", Files: []libfossil.FileToCommit{{Name: "a", Content: []byte("old a\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	_, c2, err := source.Commit(libfossil.CommitOpts{User: "test", ParentID: r1, Files: []libfossil.FileToCommit{{Name: "a", Content: []byte("new a\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Tag(libfossil.TagOpts{Name: "old", TargetID: r1, User: "test"}); err != nil {
		t.Fatal(err)
	}
	server := requiredHTTPServer(t, source, new(atomic.Int64))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "s.fossil")
	requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: server.URL, Path: path, Horizon: c2, Structural: true}).Run(&Globals{})
	})
	r, err := libfossil.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "tag:old"} {
		rid, err := r.ResolveVersion(name)
		if err != nil {
			r.Close()
			t.Fatalf("%s below the horizon does not resolve: %v", name, err)
		}
		uuid, err := r.UUIDFromRID(rid)
		if err != nil || uuid != c1 {
			r.Close()
			t.Fatalf("%s resolves to %s (%v), want %s", name, uuid, err, c1)
		}
	}
	r.Close()
	err = (&RepoCoCmd{Version: "old", Dir: t.TempDir()}).Run(&Globals{Repo: path})
	if err == nil || !strings.Contains(err.Error(), "below the structural horizon at "+c2) ||
		!strings.Contains(err.Error(), "repo cone deepen "+c1) {
		t.Fatalf("co old: %v, want a diagnostic naming the horizon and the deepen", err)
	}
}
