package libfossil

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestCheckoutCreateAndExtract(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "test.fossil")
	coDir := filepath.Join(dir, "work")

	r, err := Create(repoPath, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Create a commit with a file.
	rid, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "hello.txt", Content: []byte("hello world\n")},
		},
		Comment: "initial commit",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create checkout and extract.
	co, err := r.CreateCheckout(coDir, CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if co.Dir() != coDir {
		t.Errorf("Dir() = %q, want %q", co.Dir(), coDir)
	}

	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Verify the file exists on disk.
	data, err := os.ReadFile(filepath.Join(coDir, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world\n" {
		t.Errorf("file content = %q, want %q", string(data), "hello world\n")
	}

	// Verify Version returns the correct RID.
	vRID, vUUID, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if vRID != rid {
		t.Errorf("Version RID = %d, want %d", vRID, rid)
	}
	if vUUID == "" {
		t.Error("Version UUID is empty")
	}
}

func TestCheckoutStatus(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "test.fossil")
	coDir := filepath.Join(dir, "work")

	r, err := Create(repoPath, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "a.txt", Content: []byte("aaa\n")},
		},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	co, err := r.CreateCheckout(coDir, CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Initially no changes.
	has, err := co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("HasChanges = true before modification, want false")
	}

	// Modify the file on disk.
	if err := os.WriteFile(filepath.Join(coDir, "a.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Status should detect the modification.
	changes, err := co.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("Status returned %d changes, want 1", len(changes))
	}
	if changes[0].Name != "a.txt" {
		t.Errorf("changed file = %q, want %q", changes[0].Name, "a.txt")
	}
	if changes[0].Change != "modified" {
		t.Errorf("change type = %q, want %q", changes[0].Change, "modified")
	}

	// HasChanges should now be true (ScanChanges was called by Status).
	has, err = co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("HasChanges = false after modification, want true")
	}
}

func TestCheckoutCheckin(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "test.fossil")
	coDir := filepath.Join(dir, "work")

	r, err := Create(repoPath, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Initial commit.
	rid, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "a.txt", Content: []byte("aaa\n")},
		},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	co, err := r.CreateCheckout(coDir, CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Add a new file to the working directory and track it.
	newFile := filepath.Join(coDir, "b.txt")
	if err := os.WriteFile(newFile, []byte("bbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := co.Add([]string{"b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("Add returned %d, want 1", added)
	}

	// Commit from the checkout.
	newRID, newUUID, err := co.Checkin(CheckoutCommitOpts{
		Message: "add b.txt",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if newRID <= rid {
		t.Errorf("new RID %d should be > parent RID %d", newRID, rid)
	}
	if newUUID == "" {
		t.Error("Checkin returned empty UUID")
	}

	// Version should reflect the new commit.
	vRID, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if vRID != newRID {
		t.Errorf("Version RID = %d, want %d", vRID, newRID)
	}

	// The repo should now have both files in the new checkin.
	files, err := r.ListFiles(newRID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("ListFiles returned %d files, want 2", len(files))
	}
}

// badRCardChild commits two files and stores a child naming the same files
// under a wrong R card, as fossil's test-content-put would.
func badRCardChild(t *testing.T) (*Repo, int64) {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	parent, _, err := r.Commit(CommitOpts{User: "u", Comment: "good", Files: []FileToCommit{
		{Name: "src/a.c", Content: []byte("a\n")}, {Name: "tools/t.c", Content: []byte("t\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := manifest.GetManifest(r.inner, fsltype.FslID(parent))
	if err != nil {
		t.Fatal(err)
	}
	parentUUID, err := r.inner.UUIDByRID(parent)
	if err != nil {
		t.Fatal(err)
	}
	d.C, d.R, d.D, d.P, d.Z = "bad", strings.Repeat("0", 32), d.D.Add(time.Hour), []string{parentUUID}, ""
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var rid fsltype.FslID
	if err := r.inner.WithTx(func(tx *db.Tx) error {
		rid, _, err = blob.Store(tx, data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(r.inner); err != nil {
		t.Fatal(err)
	}
	return r, int64(rid)
}

func TestExtractWarnsOnBadRCard(t *testing.T) {
	for _, setting := range []string{"", "off"} {
		r, bad := badRCardChild(t)
		if setting != "" {
			if err := r.SetConfig("repo-cksum", setting); err != nil {
				t.Fatal(err)
			}
		}
		narrow, err := NewCone([]string{"src"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		co, err := r.CreateCheckout(t.TempDir(), CheckoutCreateOpts{Cone: narrow})
		if err != nil {
			t.Fatal(err)
		}
		var warned []string
		err = co.Extract(bad, ExtractOpts{Warn: func(m string) { warned = append(warned, m) }})
		co.Close()
		if err != nil {
			t.Fatalf("repo-cksum=%q: extract of a bad R card failed: %v", setting, err)
		}
		want := 1
		if setting == "off" {
			want = 0
		}
		if len(warned) != want {
			t.Fatalf("repo-cksum=%q: warnings %q, want %d", setting, warned, want)
		}
	}
}

// checkoutAt opens a checkout of r on rid; the test closes it.
func checkoutAt(t *testing.T, r *Repo, rid int64) *Checkout {
	t.Helper()
	co, err := r.CreateCheckout(filepath.Join(t.TempDir(), "co"), CheckoutCreateOpts{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	t.Cleanup(func() { _ = co.Close() })
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract %d: %v", rid, err)
	}
	return co
}

func TestCheckoutResolveVersion_Relative(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "a", []FileToCommit{{Name: "f", Content: []byte("a\n")}})
	b := commit(t, r, a, "b", []FileToCommit{{Name: "f", Content: []byte("b\n")}})
	c := commit(t, r, b, "c", []FileToCommit{{Name: "f", Content: []byte("c\n")}})
	co := checkoutAt(t, r, b)

	for name, want := range map[string]int64{
		"current": b, "prev": a, "previous": a, "next": c, "tip": c,
	} {
		got, err := co.ResolveVersion(name)
		if err != nil {
			t.Errorf("Checkout.ResolveVersion(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("Checkout.ResolveVersion(%q) = %d, want %d", name, got, want)
		}
	}
	// Without a checkout the keywords are ordinary names, which match nothing.
	if _, err := r.ResolveVersion("current"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("Repo.ResolveVersion(current) error = %v, want ErrVersionNotFound", err)
	}
}

// A keyword that names nothing falls through to the other forms: at a root,
// "prev" is looked up as a tag.
func TestCheckoutResolveVersion_KeywordFallsThrough(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "a", []FileToCommit{{Name: "f", Content: []byte("a\n")}})
	tagged := commitBranch(t, r, a, "prev", "tagged prev",
		[]FileToCommit{{Name: "f", Content: []byte("p\n")}})
	co := checkoutAt(t, r, a)

	got, err := co.ResolveVersion("prev")
	if err != nil {
		t.Fatalf("Checkout.ResolveVersion(prev): %v", err)
	}
	if got != tagged {
		t.Errorf("prev at a root = %d, want %d, the check-in tagged prev", got, tagged)
	}
}

// Section 8.3 rewrites a branch-type request to a check-in request, so a
// technote tagged with the branch name, newer than its tip, cannot answer it.
func TestResolveBranchStart_TechnoteDoesNotShadow(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "base", []FileToCommit{{Name: "f", Content: []byte("0\n")}})
	b1 := makeBranch(t, r, base, "feat")
	if _, err := r.inner.DB().Exec(`INSERT INTO event(type, mtime, objid, user, comment)
		VALUES ('e', 99999999, 1000000, 'u', 'note');
		INSERT INTO blob(rid, uuid, size)
		VALUES (1000000, 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee', 0);
		INSERT INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
		VALUES ((SELECT tagid FROM tag WHERE tagname='sym-feat'), 1, 1000000, 1000000,
		NULL, 99999999, 1000000)`); err != nil {
		t.Fatal(err)
	}
	got, err := r.ResolveBranchStart("feat")
	if err != nil || got != b1 {
		t.Errorf("ResolveBranchStart(feat) = %d, %v; want %d", got, err, b1)
	}
}

func TestResolveBranchStart(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "base", []FileToCommit{{Name: "f", Content: []byte("0\n")}})
	b1 := makeBranch(t, r, base, "feat")
	b2 := commit(t, r, b1, "b2", []FileToCommit{{Name: "f", Content: []byte("2\n")}})
	b2uuid, err := r.UUIDFromRID(b2)
	if err != nil {
		t.Fatal(err)
	}

	// The plain name and the hash both walk back to the branch's start.
	for _, name := range []string{"feat", b2uuid, "tip"} {
		got, err := r.ResolveBranchStart(name)
		if err != nil {
			t.Errorf("ResolveBranchStart(%q): %v", name, err)
			continue
		}
		if got != b1 {
			t.Errorf("ResolveBranchStart(%q) = %d, want %d, feat's first check-in", name, got, b1)
		}
	}

	files, err := r.ListFiles(b2)
	if err != nil || len(files) != 1 {
		t.Fatalf("ListFiles: %v, %d files", err, len(files))
	}
	if _, err := r.ResolveBranchStart(files[0].UUID); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("ResolveBranchStart(file hash) error = %v, want ErrVersionNotFound", err)
	}
}

// A checkout with no recorded version has no checkout-relative names, but
// every other form still resolves, as in fossil.
func TestCheckoutResolveVersion_NoVersion(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "a", []FileToCommit{{Name: "f", Content: []byte("a\n")}})
	dir := filepath.Join(t.TempDir(), "co")
	co, err := r.CreateCheckout(dir, CheckoutCreateOpts{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	t.Cleanup(func() { _ = co.Close() })
	// Blank the recorded version through a second connection; open refuses
	// a checkout database that starts out without one.
	ck, err := sql.Open(db.RegisteredDriver().Name, filepath.Join(dir, ".fslckout"))
	if err != nil {
		t.Fatal(err)
	}
	defer ck.Close()
	if _, err := ck.Exec("UPDATE vvar SET value = '' WHERE name = 'checkout'"); err != nil {
		t.Fatal(err)
	}

	got, err := co.ResolveVersion("tip")
	if err != nil || got != a {
		t.Errorf("ResolveVersion(tip) = %d, %v; want %d", got, err, a)
	}
	if rid, err := co.ResolveVersion("current"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("ResolveVersion(current) = %d, %v, want ErrVersionNotFound", rid, err)
	}
}

// "ckout" names the working files in the checkout-inclusive request, even
// over a tag of that name, and only as the whole name; every other name
// resolves as Checkout.ResolveVersion resolves it.
func TestCheckoutResolveWorking(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "a", []FileToCommit{{Name: "f", Content: []byte("a\n")}})
	tagged := commitBranch(t, r, a, "ckout", "tagged ckout",
		[]FileToCommit{{Name: "f", Content: []byte("k\n")}})
	co := checkoutAt(t, r, a)

	got, err := co.ResolveWorking("ckout")
	if err != nil || got != (WorkingVersion{RID: a, Working: true}) {
		t.Errorf("ResolveWorking(ckout) = %+v, %v; want the working files on %d", got, err, a)
	}
	for name, want := range map[string]int64{"current": a, "tip": tagged, "ckout.": tagged} {
		got, err := co.ResolveWorking(name)
		if err != nil || got != (WorkingVersion{RID: want}) {
			t.Errorf("ResolveWorking(%q) = %+v, %v; want check-in %d", name, got, err, want)
		}
	}
	// The plain request reads ckout as the tag.
	if rid, err := co.ResolveVersion("ckout"); err != nil || rid != tagged {
		t.Errorf("ResolveVersion(ckout) = %d, %v; want %d", rid, err, tagged)
	}
}

// The checkout-inclusive request is a check-in request: a technote carrying
// the tag cannot answer it, though it answers the untyped ResolveVersion.
func TestCheckoutResolveWorking_CheckinOnly(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "a", []FileToCommit{{Name: "f", Content: []byte("a\n")}})
	co := checkoutAt(t, r, a)
	if _, err := r.inner.DB().Exec(`INSERT INTO event(type, mtime, objid, user, comment)
		VALUES ('e', 99999999, 1000000, 'u', 'note');
		INSERT INTO blob(rid, uuid, size)
		VALUES (1000000, 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee', 0);
		INSERT INTO tag(tagname) VALUES ('sym-note');
		INSERT INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
		VALUES ((SELECT tagid FROM tag WHERE tagname='sym-note'), 1, 1000000, 1000000,
		NULL, 99999999, 1000000)`); err != nil {
		t.Fatal(err)
	}
	if rid, err := co.ResolveVersion("note"); err != nil || rid != 1000000 {
		t.Errorf("ResolveVersion(note) = %d, %v; want the technote", rid, err)
	}
	if got, err := co.ResolveWorking("note"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("ResolveWorking(note) = %+v, %v; want ErrVersionNotFound", got, err)
	}
}

// With no recorded version, "ckout" is no keyword: it falls through to the
// other forms, as in fossil: a tag of that name answers, not the working files.
func TestCheckoutResolveWorking_NoVersion(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "a", []FileToCommit{{Name: "f", Content: []byte("a\n")}})
	tagged := commitBranch(t, r, a, "ckout", "tagged ckout",
		[]FileToCommit{{Name: "f", Content: []byte("k\n")}})
	dir := filepath.Join(t.TempDir(), "co")
	co, err := r.CreateCheckout(dir, CheckoutCreateOpts{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	t.Cleanup(func() { _ = co.Close() })
	ck, err := sql.Open(db.RegisteredDriver().Name, filepath.Join(dir, ".fslckout"))
	if err != nil {
		t.Fatal(err)
	}
	defer ck.Close()
	if _, err := ck.Exec("UPDATE vvar SET value = '' WHERE name = 'checkout'"); err != nil {
		t.Fatal(err)
	}
	got, err := co.ResolveWorking("ckout")
	if err != nil || got != (WorkingVersion{RID: tagged}) {
		t.Errorf("ResolveWorking(ckout) = %+v, %v; want the tag's check-in %d", got, err, tagged)
	}
}
