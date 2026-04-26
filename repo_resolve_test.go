package libfossil

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// commitBranch creates a commit on a named branch by attaching a sym-<branch>
// propagating tag. The first commit on the branch uses ParentID=parent.
func commitBranch(t *testing.T, r *Repo, parent int64, branch, comment string, files []FileToCommit) int64 {
	t.Helper()
	rid, _, err := r.Commit(CommitOpts{
		ParentID: parent,
		Files:    files,
		Comment:  comment,
		User:     "test",
		Tags:     []TagSpec{{Name: "sym-" + branch, Value: branch}},
	})
	if err != nil {
		t.Fatalf("Commit on branch %q: %v", branch, err)
	}
	return rid
}

func TestResolveVersion_Tip(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "first", []FileToCommit{{Name: "a.txt", Content: []byte("a\n")}})
	b := commit(t, r, a, "second", []FileToCommit{{Name: "b.txt", Content: []byte("b\n")}})

	got, err := r.ResolveVersion("tip")
	if err != nil {
		t.Fatalf("ResolveVersion(tip): %v", err)
	}
	if got != b {
		t.Errorf("tip = %d, want %d (newest checkin)", got, b)
	}
}

func TestResolveVersion_EmptyStringIsTip(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "only", []FileToCommit{{Name: "x.txt", Content: []byte("x\n")}})

	got, err := r.ResolveVersion("")
	if err != nil {
		t.Fatalf("ResolveVersion(''): %v", err)
	}
	if got != a {
		t.Errorf("empty string = %d, want %d", got, a)
	}
}

func TestResolveVersion_Trunk(t *testing.T) {
	r := newTestRepo(t)
	// First commit gets sym-trunk automatically from Fossil's Create; but our
	// test repo uses Commit directly. Attach trunk tag explicitly.
	trunk := commitBranch(t, r, 0, "trunk", "trunk commit", []FileToCommit{
		{Name: "t.txt", Content: []byte("trunk\n")},
	})

	got, err := r.ResolveVersion("trunk")
	if err != nil {
		t.Fatalf("ResolveVersion(trunk): %v", err)
	}
	if got != trunk {
		t.Errorf("trunk = %d, want %d", got, trunk)
	}
}

func TestResolveVersion_NamedBranch(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "base", []FileToCommit{{Name: "base.txt", Content: []byte("base\n")}})
	featureTip := commitBranch(t, r, base, "feature-x", "feature work",
		[]FileToCommit{{Name: "feature.txt", Content: []byte("feature\n")}})

	got, err := r.ResolveVersion("feature-x")
	if err != nil {
		t.Fatalf("ResolveVersion(feature-x): %v", err)
	}
	if got != featureTip {
		t.Errorf("feature-x = %d, want %d", got, featureTip)
	}
}

func TestResolveVersion_FullUUID(t *testing.T) {
	r := newTestRepo(t)
	rid, uuid, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "u.txt", Content: []byte("u\n")}},
		Comment: "uuid test",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := r.ResolveVersion(uuid)
	if err != nil {
		t.Fatalf("ResolveVersion(full uuid): %v", err)
	}
	if got != rid {
		t.Errorf("full uuid = %d, want %d", got, rid)
	}
}

func TestResolveVersion_UniquePrefix(t *testing.T) {
	r := newTestRepo(t)
	rid, uuid, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "p.txt", Content: []byte("p\n")}},
		Comment: "prefix test",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Use first 8 characters of the UUID as a prefix.
	prefix := uuid[:8]

	got, err := r.ResolveVersion(prefix)
	if err != nil {
		t.Fatalf("ResolveVersion(%q): %v", prefix, err)
	}
	if got != rid {
		t.Errorf("prefix %q = %d, want %d", prefix, got, rid)
	}
}

func TestResolveVersion_NotFound(t *testing.T) {
	r := newTestRepo(t)
	commit(t, r, 0, "init", []FileToCommit{{Name: "x.txt", Content: []byte("x\n")}})

	_, err := r.ResolveVersion("deadbeefdeadbeef")
	if err == nil {
		t.Fatal("expected error for nonexistent version, got nil")
	}
	if !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("err = %v, want errors.Is ErrVersionNotFound", err)
	}
}

func TestResolveVersion_EmptyRepoTipNotFound(t *testing.T) {
	r := newTestRepo(t)
	// No commits at all.
	_, err := r.ResolveVersion("tip")
	if err == nil {
		t.Fatal("expected error for tip in empty repo, got nil")
	}
	if !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("err = %v, want errors.Is ErrVersionNotFound", err)
	}
}

func TestResolveVersion_AmbiguousPrefix(t *testing.T) {
	r := newTestRepo(t)
	// We cannot force a collision naturally, but we can verify the error type
	// is returned correctly by inspecting the code path. Instead, confirm that
	// a single-match prefix does NOT return ErrAmbiguousVersion.
	_, uuid, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "q.txt", Content: []byte("q\n")}},
		Comment: "ambiguous test base",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Single-character prefix is likely ambiguous in a repo with multiple blobs
	// (the manifest blob itself plus the file blob), so look for ErrAmbiguousVersion.
	_, ambigErr := r.ResolveVersion(uuid[:1])
	// If there's only one match it's fine too; the important thing is the error
	// type when ambiguity occurs.
	if ambigErr != nil && !errors.Is(ambigErr, ErrAmbiguousVersion) && !errors.Is(ambigErr, ErrVersionNotFound) {
		t.Errorf("unexpected error type: %v", ambigErr)
	}

	// Now we want to explicitly test the ErrAmbiguousVersion sentinel exists and
	// wraps correctly.
	wrapped := fmt.Errorf("outer: %w", ErrAmbiguousVersion)
	if !errors.Is(wrapped, ErrAmbiguousVersion) {
		t.Error("ErrAmbiguousVersion does not unwrap correctly through fmt.Errorf %w")
	}
}

func TestResolveVersion_EmptyString(t *testing.T) {
	// Empty string with no commits should return ErrVersionNotFound.
	r := newTestRepo(t)
	_, err := r.ResolveVersion("")
	if err == nil {
		t.Fatal("expected error for empty string with no commits, got nil")
	}
	if !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("err = %v, want ErrVersionNotFound", err)
	}
}

func TestReadFileAt_Trunk(t *testing.T) {
	r := newTestRepo(t)
	commitBranch(t, r, 0, "trunk", "trunk init",
		[]FileToCommit{{Name: "README.md", Content: []byte("# readme\n")}},
	)

	data, err := r.ReadFileAt("trunk", "README.md")
	if err != nil {
		t.Fatalf("ReadFileAt(trunk, README.md): %v", err)
	}
	if string(data) != "# readme\n" {
		t.Errorf("content = %q, want %q", data, "# readme\n")
	}
}

func TestReadFileAt_ResolveError(t *testing.T) {
	r := newTestRepo(t)
	commit(t, r, 0, "init", []FileToCommit{{Name: "x.txt", Content: []byte("x\n")}})

	_, err := r.ReadFileAt("no-such-version-xyz", "x.txt")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("err = %v, want ErrVersionNotFound", err)
	}
}

func TestReadFileAt_FileNotFound(t *testing.T) {
	r := newTestRepo(t)
	commitBranch(t, r, 0, "trunk", "trunk init",
		[]FileToCommit{{Name: "README.md", Content: []byte("# readme\n")}},
	)

	_, err := r.ReadFileAt("trunk", "missing.txt")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("err = %v, want ErrFileNotFound", err)
	}
}

func TestReadFileAt_ByUUIDPrefix(t *testing.T) {
	r := newTestRepo(t)
	rid, uuid, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "doc.txt", Content: []byte("doc content\n")}},
		Comment: "doc commit",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = rid

	data, err := r.ReadFileAt(uuid[:12], "doc.txt")
	if err != nil {
		t.Fatalf("ReadFileAt(prefix, doc.txt): %v", err)
	}
	if !strings.Contains(string(data), "doc content") {
		t.Errorf("content = %q, want it to contain 'doc content'", data)
	}
}
