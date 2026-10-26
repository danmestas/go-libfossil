package libfossil

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func commit(t *testing.T, r *Repo, parent int64, comment string, files []FileToCommit) int64 {
	t.Helper()
	rid, _, err := r.Commit(CommitOpts{
		ParentID: parent,
		Files:    files,
		Comment:  comment,
		User:     "test",
	})
	if err != nil {
		t.Fatalf("Commit %q: %v", comment, err)
	}
	return rid
}

func TestDiff_Modification(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nworld\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nbrave new world\n")},
	})

	entries, err := r.Diff(a, b, "hello.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Name != "hello.txt" {
		t.Errorf("Name = %q, want %q", e.Name, "hello.txt")
	}
	if !strings.Contains(e.Unified, "a/hello.txt") || !strings.Contains(e.Unified, "b/hello.txt") {
		t.Errorf("missing headers in unified output:\n%s", e.Unified)
	}
	if !strings.Contains(e.Unified, "-world") || !strings.Contains(e.Unified, "+brave new world") {
		t.Errorf("missing hunk lines in unified output:\n%s", e.Unified)
	}
}

func TestDiff_Identical(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\n")},
		{Name: "other.txt", Content: []byte("first\n")},
	})
	b := commit(t, r, a, "touch other", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\n")},
		{Name: "other.txt", Content: []byte("second\n")},
	})

	entries, err := r.Diff(a, b, "hello.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("want empty slice for identical content, got %d entries:\n%+v", len(entries), entries)
	}
}

func TestDiff_Addition(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "keep.txt", Content: []byte("anchor\n")},
	})
	b := commit(t, r, a, "add new", []FileToCommit{
		{Name: "new.txt", Content: []byte("line one\nline two\n")},
	})

	entries, err := r.Diff(a, b, "new.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	out := entries[0].Unified
	if !strings.Contains(out, "+line one") || !strings.Contains(out, "+line two") {
		t.Errorf("expected insertion lines, got:\n%s", out)
	}
	if strings.Contains(out, "-line one") {
		t.Errorf("unexpected deletion marker for pure addition:\n%s", out)
	}
}

func TestDiff_Deletion(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "doomed.txt", Content: []byte("goodbye\ncruel world\n")},
		{Name: "keep.txt", Content: []byte("anchor\n")},
	})
	// PartialManifest: true models deletion by omission — only callers
	// intentionally erasing tracked files should opt in.
	b, _, err := r.Commit(CommitOpts{
		ParentID:        a,
		Files:           []FileToCommit{{Name: "keep.txt", Content: []byte("anchor\n")}},
		Comment:         "drop doomed",
		User:            "test",
		PartialManifest: true,
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entries, err := r.Diff(a, b, "doomed.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	out := entries[0].Unified
	if !strings.Contains(out, "-goodbye") || !strings.Contains(out, "-cruel world") {
		t.Errorf("expected deletion lines, got:\n%s", out)
	}
	if strings.Contains(out, "+goodbye") {
		t.Errorf("unexpected insertion marker for pure deletion:\n%s", out)
	}
}

func TestDiff_AbsentBothSides(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "only.txt", Content: []byte("x\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "only.txt", Content: []byte("y\n")},
	})

	entries, err := r.Diff(a, b, "never-existed.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("want empty slice for file absent in both checkins, got %d entries:\n%+v", len(entries), entries)
	}
}

func TestReadFile_Present(t *testing.T) {
	r := newTestRepo(t)
	rid := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nworld\n")},
		{Name: "other.txt", Content: []byte("other\n")},
	})

	data, err := r.ReadFile(rid, "hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello\nworld\n" {
		t.Errorf("content = %q, want %q", data, "hello\nworld\n")
	}
}

func TestReadFile_AcrossRevs(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "f.txt", Content: []byte("first\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "f.txt", Content: []byte("second\n")},
	})

	got, err := r.ReadFile(a, "f.txt")
	if err != nil {
		t.Fatalf("ReadFile(a): %v", err)
	}
	if string(got) != "first\n" {
		t.Errorf("rev a = %q, want %q", got, "first\n")
	}
	got, err = r.ReadFile(b, "f.txt")
	if err != nil {
		t.Fatalf("ReadFile(b): %v", err)
	}
	if string(got) != "second\n" {
		t.Errorf("rev b = %q, want %q", got, "second\n")
	}
}

func TestReadFile_NotInCheckin(t *testing.T) {
	r := newTestRepo(t)
	rid := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "only.txt", Content: []byte("x\n")},
	})

	_, err := r.ReadFile(rid, "missing.txt")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("err = %v, want errors.Is ErrFileNotFound", err)
	}
}

func TestReadFile_EmptyFilePath(t *testing.T) {
	r := newTestRepo(t)
	rid := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "x.txt", Content: []byte("x\n")},
	})

	_, err := r.ReadFile(rid, "")
	if err == nil {
		t.Fatal("expected error for empty filePath, got nil")
	}
	if !strings.Contains(err.Error(), "filePath is required") {
		t.Errorf("err = %q, want message mentioning filePath", err.Error())
	}
}

func TestDiff_WholeCheckin_Identical(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\n")},
		{Name: "other.txt", Content: []byte("other\n")},
	})

	entries, err := r.Diff(a, a, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if entries == nil {
		t.Fatal("entries = nil, want empty non-nil slice for identical checkins")
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0:\n%+v", len(entries), entries)
	}
}

func TestDiff_WholeCheckin_SingleFileChange(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nworld\n")},
		{Name: "stable.txt", Content: []byte("unchanged\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hello\nbrave new world\n")},
		{Name: "stable.txt", Content: []byte("unchanged\n")},
	})

	entries, err := r.Diff(a, b, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1:\n%+v", len(entries), entries)
	}
	if entries[0].Name != "hello.txt" {
		t.Errorf("Name = %q, want %q", entries[0].Name, "hello.txt")
	}
	if !strings.Contains(entries[0].Unified, "+brave new world") {
		t.Errorf("missing expected hunk in unified diff:\n%s", entries[0].Unified)
	}
}

func TestDiff_WholeCheckin_MultiFileChange(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "a.txt", Content: []byte("a-old\n")},
		{Name: "b.txt", Content: []byte("b-old\n")},
		{Name: "c.txt", Content: []byte("c-stable\n")},
	})
	b := commit(t, r, a, "v2", []FileToCommit{
		{Name: "a.txt", Content: []byte("a-new\n")},
		{Name: "b.txt", Content: []byte("b-new\n")},
		{Name: "c.txt", Content: []byte("c-stable\n")},
	})

	entries, err := r.Diff(a, b, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2:\n%+v", len(entries), entries)
	}
	// Deterministic order: sorted by Name.
	if entries[0].Name != "a.txt" || entries[1].Name != "b.txt" {
		t.Errorf("entry order = [%q, %q], want [a.txt, b.txt]", entries[0].Name, entries[1].Name)
	}
	if !strings.Contains(entries[0].Unified, "-a-old") || !strings.Contains(entries[0].Unified, "+a-new") {
		t.Errorf("a.txt hunk missing expected lines:\n%s", entries[0].Unified)
	}
	if !strings.Contains(entries[1].Unified, "-b-old") || !strings.Contains(entries[1].Unified, "+b-new") {
		t.Errorf("b.txt hunk missing expected lines:\n%s", entries[1].Unified)
	}
}

func TestDiff_WholeCheckin_AdditionOnly(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "anchor.txt", Content: []byte("anchor\n")},
	})
	b := commit(t, r, a, "add new", []FileToCommit{
		{Name: "anchor.txt", Content: []byte("anchor\n")},
		{Name: "new.txt", Content: []byte("brand new\n")},
	})

	entries, err := r.Diff(a, b, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1:\n%+v", len(entries), entries)
	}
	if entries[0].Name != "new.txt" {
		t.Errorf("Name = %q, want %q", entries[0].Name, "new.txt")
	}
	if !strings.Contains(entries[0].Unified, "+brand new") {
		t.Errorf("expected addition hunk, got:\n%s", entries[0].Unified)
	}
	if strings.Contains(entries[0].Unified, "-brand new") {
		t.Errorf("unexpected deletion marker for pure addition:\n%s", entries[0].Unified)
	}
}

func TestDiff_WholeCheckin_DeletionOnly(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "v1", []FileToCommit{
		{Name: "anchor.txt", Content: []byte("anchor\n")},
		{Name: "doomed.txt", Content: []byte("goodbye\n")},
	})
	// PartialManifest: true models deletion by omission.
	b, _, err := r.Commit(CommitOpts{
		ParentID:        a,
		Files:           []FileToCommit{{Name: "anchor.txt", Content: []byte("anchor\n")}},
		Comment:         "drop doomed",
		User:            "test",
		PartialManifest: true,
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entries, err := r.Diff(a, b, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1:\n%+v", len(entries), entries)
	}
	if entries[0].Name != "doomed.txt" {
		t.Errorf("Name = %q, want %q", entries[0].Name, "doomed.txt")
	}
	if !strings.Contains(entries[0].Unified, "-goodbye") {
		t.Errorf("expected deletion hunk, got:\n%s", entries[0].Unified)
	}
	if strings.Contains(entries[0].Unified, "+goodbye") {
		t.Errorf("unexpected insertion marker for pure deletion:\n%s", entries[0].Unified)
	}
}

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
	// Two artifacts whose hashes share a four-character hex prefix. Inserted
	// directly, since a natural collision cannot be forced.
	for _, uuid := range []string{
		"abcd" + strings.Repeat("0", 36),
		"abcd" + strings.Repeat("1", 36),
	} {
		if _, err := r.inner.DB().Exec(
			"INSERT INTO blob(uuid, size) VALUES(?, 0)", uuid); err != nil {
			t.Fatalf("insert blob %s: %v", uuid, err)
		}
	}

	_, err := r.ResolveVersion("abcd")
	if !errors.Is(err, ErrAmbiguousVersion) {
		t.Fatalf("ResolveVersion(\"abcd\") err = %v, want ErrAmbiguousVersion", err)
	}
}

func TestResolveVersion_AmbiguousWraps(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", ErrAmbiguousVersion)
	if !errors.Is(wrapped, ErrAmbiguousVersion) {
		t.Error("ErrAmbiguousVersion does not unwrap through fmt.Errorf %w")
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

// commitRel commits a check-in carrying sym-rel at time tm, for the
// NAME:TIMESTAMP tests.
func commitRel(t *testing.T, r *Repo, parent int64, name string, tm time.Time) int64 {
	t.Helper()
	rid, _, err := r.Commit(CommitOpts{
		ParentID: parent,
		Files:    []FileToCommit{{Name: name + ".txt", Content: []byte(name)}},
		Comment:  name,
		User:     "test",
		Time:     tm,
		Tags:     []TagSpec{{Name: "sym-rel", Value: "rel"}},
	})
	if err != nil {
		t.Fatalf("commitRel %q: %v", name, err)
	}
	return rid
}

// TestResolveVersion_NameTimestampRoundsUp checks that "NAME:TIMESTAMP" rounds
// the boundary UP to the last millisecond of the stated precision (fossil's
// fossil_roundup_date), for the date-only, minute and second forms, each with
// a check-in just inside and just outside the window.
func TestResolveVersion_NameTimestampRoundsUp(t *testing.T) {
	cases := []struct {
		query           string
		inside, outside time.Time
	}{
		{ // date-only -> 2026-03-10 23:59:59.999
			"rel:2026-03-10",
			time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 3, 11, 0, 0, 1, 0, time.UTC),
		},
		{ // minute -> 2026-03-10 12:30:59.999
			"rel:2026-03-10T12:30",
			time.Date(2026, 3, 10, 12, 30, 30, 0, time.UTC),
			time.Date(2026, 3, 10, 12, 31, 0, 0, time.UTC),
		},
		{ // second -> 2026-03-10 12:30:15.999
			"rel:2026-03-10T12:30:15",
			time.Date(2026, 3, 10, 12, 30, 15, 500*int(time.Millisecond), time.UTC),
			time.Date(2026, 3, 10, 12, 30, 16, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			r := newTestRepo(t)
			base := commit(t, r, 0, "base", []FileToCommit{{Name: "b.txt", Content: []byte("b")}})
			inside := commitRel(t, r, base, "inside", c.inside)
			commitRel(t, r, inside, "outside", c.outside)

			got, err := r.ResolveVersion(c.query)
			if err != nil {
				t.Fatalf("ResolveVersion(%q): %v", c.query, err)
			}
			if got != inside {
				t.Errorf("ResolveVersion(%q) = %d, want %d (the check-in inside the window)",
					c.query, got, inside)
			}
		})
	}
}

// TestResolveVersion_NameTimestampZulu checks that a trailing Z/UTC designator
// is accepted and the time is read as UTC.
func TestResolveVersion_NameTimestampZulu(t *testing.T) {
	for _, q := range []string{"rel:2026-03-10T12:30:15Z", "rel:2026-03-10T12:30:15 UTC"} {
		r := newTestRepo(t)
		base := commit(t, r, 0, "base", []FileToCommit{{Name: "b.txt", Content: []byte("b")}})
		inside := commitRel(t, r, base, "inside", time.Date(2026, 3, 10, 12, 30, 15, 0, time.UTC))
		commitRel(t, r, inside, "outside", time.Date(2026, 3, 10, 12, 30, 16, 0, time.UTC))

		got, err := r.ResolveVersion(q)
		if err != nil {
			t.Fatalf("ResolveVersion(%q): %v", q, err)
		}
		if got != inside {
			t.Errorf("ResolveVersion(%q) = %d, want %d", q, got, inside)
		}
	}
}
