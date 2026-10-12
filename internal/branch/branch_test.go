package branch

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

func setupTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestCreate(t *testing.T) {
	r := setupTestRepo(t)

	parentRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	branchRid, _, err := Create(r, CreateOpts{
		Name:   "feature-x",
		Parent: parentRid,
		User:   "testuser",
		Time:   time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if branchRid <= 0 {
		t.Fatalf("branchRid=%d, want > 0", branchRid)
	}

	// Verify branch tag.
	var branchValue string
	err = r.DB().QueryRow(
		"SELECT value FROM tagxref JOIN tag USING(tagid) WHERE tagname='branch' AND rid=?", branchRid,
	).Scan(&branchValue)
	if err != nil {
		t.Fatalf("branch tag query: %v", err)
	}
	if branchValue != "feature-x" {
		t.Errorf("branch value=%q, want %q", branchValue, "feature-x")
	}

	// Verify sym-feature-x tag.
	var symCount int
	r.DB().QueryRow(
		"SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='sym-feature-x' AND rid=?", branchRid,
	).Scan(&symCount)
	if symCount != 1 {
		t.Errorf("sym-feature-x count=%d, want 1", symCount)
	}

	// Verify old sym-trunk cancelled on the branch checkin.
	var oldSymCount int
	r.DB().QueryRow(
		"SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='sym-trunk' AND tagtype>0 AND rid=?", branchRid,
	).Scan(&oldSymCount)
	if oldSymCount != 0 {
		t.Errorf("sym-trunk should be cancelled on branch, count=%d", oldSymCount)
	}
}

func TestList(t *testing.T) {
	r := setupTestRepo(t)

	parentRid, _, _ := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("a")}},
		Comment: "initial",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})

	Create(r, CreateOpts{Name: "feature-a", Parent: parentRid, User: "testuser", Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)})
	Create(r, CreateOpts{Name: "feature-b", Parent: parentRid, User: "testuser", Time: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)})

	branches, err := List(r)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	names := map[string]bool{}
	for _, b := range branches {
		names[b.Name] = true
		t.Logf("branch: %s closed=%v checkins=%d", b.Name, b.IsClosed, b.CheckinCount)
	}
	if !names["feature-a"] {
		t.Error("missing branch feature-a")
	}
	if !names["feature-b"] {
		t.Error("missing branch feature-b")
	}
}

func TestClose(t *testing.T) {
	r := setupTestRepo(t)

	parentRid, _, _ := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("a")}},
		Comment: "initial",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})

	Create(r, CreateOpts{Name: "done-branch", Parent: parentRid, User: "testuser", Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)})

	if err := Close(r, "done-branch", "testuser"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	branches, _ := List(r)
	for _, b := range branches {
		if b.Name == "done-branch" {
			if !b.IsClosed {
				t.Error("branch done-branch should be closed")
			}
			return
		}
	}
	t.Error("done-branch not found in List")
}

// TestCreate_RefusesOpenBranchName checks that creating a second branch with
// the name of an existing open branch fails and stores nothing, as fossil's
// `branch new` does.
func TestCreate_RefusesOpenBranchName(t *testing.T) {
	r := setupTestRepo(t)

	parentRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	if _, _, err := Create(r, CreateOpts{
		Name: "feature-x", Parent: parentRid, User: "testuser",
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	var before int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before); err != nil {
		t.Fatalf("count blobs: %v", err)
	}

	_, _, err = Create(r, CreateOpts{
		Name: "feature-x", Parent: parentRid, User: "testuser",
		Time: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("second Create of an open branch name = nil error, want a refusal")
	}

	var after int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&after); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if after != before {
		t.Errorf("blob count %d -> %d; a refused Create must store nothing", before, after)
	}
}

// TestCreate_AllowsClosedBranchName checks that a name whose only leaf is
// closed can be reused, as fossil allows once a branch is closed.
func TestCreate_AllowsClosedBranchName(t *testing.T) {
	r := setupTestRepo(t)

	parentRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	branchRid, _, err := Create(r, CreateOpts{
		Name: "feature-x", Parent: parentRid, User: "testuser",
		Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Close the branch's only leaf (tag 9 is 'closed').
	if _, err := r.DB().Exec(
		"INSERT INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) VALUES(9, 1, ?, ?, NULL, 0, ?)",
		int64(branchRid), int64(branchRid), int64(branchRid)); err != nil {
		t.Fatalf("close branch: %v", err)
	}

	if _, _, err := Create(r, CreateOpts{
		Name: "feature-x", Parent: parentRid, User: "testuser",
		Time: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Create after close: %v, want success (branch was closed)", err)
	}
}

// TestCreate_ConcurrentSameNameOneWins races several Creates of one name on
// one repository. The open-name check and the check-in must be one
// transaction, so exactly one Create commits and the rest are refused.
func TestCreate_ConcurrentSameNameOneWins(t *testing.T) {
	const racers = 8
	r := setupTestRepo(t)
	parentRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Distinct users give distinct manifests, so no two racers
			// store the same artifact.
			_, _, errs[i] = Create(r, CreateOpts{
				Name: "feature-x", Parent: parentRid, User: fmt.Sprintf("user%d", i),
				Time: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
			})
		}()
	}
	close(start)
	wg.Wait()

	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent Creates of one name succeeded, want 1: %v", won, racers, errs)
	}
	var leaves int
	const openLeaves = `SELECT count(*) FROM leaf JOIN tagxref USING(rid)
		JOIN tag USING(tagid) WHERE tagname='branch' AND value='feature-x'`
	if err := r.DB().QueryRow(openLeaves).Scan(&leaves); err != nil {
		t.Fatalf("count feature-x leaves: %v", err)
	}
	if leaves != 1 {
		t.Errorf("feature-x has %d open leaves, want 1", leaves)
	}
}
