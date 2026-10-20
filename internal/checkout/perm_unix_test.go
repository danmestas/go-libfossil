//go:build unix

package checkout

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// permRepo is a repository whose one check-in holds a plain file, an
// executable and a symlink to the plain file.
func permRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.CreateWithEnv(filepath.Join(t.TempDir(), "p.fossil"), "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "plain.txt", Content: []byte("plain\n")},
			{Name: "run.sh", Content: []byte("#!/bin/sh\n"), Perm: "x"},
			{Name: "link", Content: []byte("plain.txt"), Perm: "l"},
		},
		Comment: "initial", User: "test", Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// permCheckout extracts r's tip to disk, with link a real symlink as fossil
// writes it while allow-symlinks is on, and scans once.
func permCheckout(t *testing.T, r *repo.Repo) *Checkout {
	t.Helper()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	replaceWithSymlink(t, co, "link", "plain.txt")
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}
	return co
}

func replaceWithSymlink(t *testing.T, co *Checkout, name, target string) {
	t.Helper()
	p := filepath.Join(co.dir, name)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, co *Checkout, name string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(filepath.Join(co.dir, name), mode); err != nil {
		t.Fatal(err)
	}
}

func chngedOf(t *testing.T, co *Checkout, name string) int64 {
	t.Helper()
	var chnged int64
	if err := co.db.QueryRow("SELECT chnged FROM vfile WHERE pathname=?", name).
		Scan(&chnged); err != nil {
		t.Fatalf("chnged of %s: %v", name, err)
	}
	return chnged
}

// Checkout records a symlink's l permission in vfile.islink, as fossil's
// load_vfile_from_rid does.
func TestLoadVFileRecordsSymlink(t *testing.T) {
	co := permCheckout(t, permRepo(t))
	var isexe, islink int64
	if err := co.db.QueryRow("SELECT isexe, islink FROM vfile WHERE pathname='link'").
		Scan(&isexe, &islink); err != nil {
		t.Fatal(err)
	}
	if isexe != 0 || islink != 1 {
		t.Fatalf("link: isexe %d islink %d, want 0 and 1", isexe, islink)
	}
}

// A file missing from disk is marked changed and its mtime recorded as -1,
// as fossil's scan does.
func TestScanMarksMissingFileChanged(t *testing.T) {
	co := permCheckout(t, permRepo(t))
	if err := os.Remove(filepath.Join(co.dir, "plain.txt")); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(0); err != nil {
		t.Fatal(err)
	}
	if got := chngedOf(t, co, "plain.txt"); got != 1 {
		t.Fatalf("missing file: chnged %d, want 1", got)
	}
	if got := vfileMTime(t, co, "plain.txt"); got != -1 {
		t.Fatalf("missing file: mtime %d, want -1", got)
	}
}

// With allow-symlinks off (fossil's default) a symlink is seen as what it
// points to, so a tracked symlink reads as a plain file: fossil reports it
// UNLINK on a fresh checkout.
func TestScanSymlinksOff(t *testing.T) {
	co := permCheckout(t, permRepo(t))
	got := statusOf(t, co)
	if len(got) != 1 || got["link"] != ChangeUnlink {
		t.Fatalf("status = %v, want link unlink", got)
	}
}

// The scan sets chnged 6 through 9 for executable and symlink changes, and
// back to 0 once they are undone.
func TestScanSeesModeChanges(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	co := permCheckout(t, r)
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("fresh checkout: status %v, want nothing", got)
	}
	chmod(t, co, "plain.txt", 0o755)
	chmod(t, co, "run.sh", 0o644)
	if err := os.Remove(filepath.Join(co.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(co.dir, "link"), []byte("plain.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := statusOf(t, co)
	want := map[string]FileChange{
		"plain.txt": ChangeExecutable, "run.sh": ChangeUnexec, "link": ChangeUnlink,
	}
	assertStatus(t, got, want)

	chmod(t, co, "plain.txt", 0o644)
	chmod(t, co, "run.sh", 0o755)
	replaceWithSymlink(t, co, "link", "plain.txt")
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("modes put back: status %v, want nothing", got)
	}
	replaceWithSymlink(t, co, "plain.txt", "run.sh")
	assertStatus(t, statusOf(t, co), map[string]FileChange{"plain.txt": ChangeSymlink})
}

// A storage without file modes, such as MemStorage, keeps the recorded
// permissions, as fossil on Windows does: an executable is not seen to lose
// its bit.
func TestScanWithoutModesKeepsPermissions(t *testing.T) {
	co, _ := memCheckout(t, permRepo(t), simio.RealClock{})
	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status = %v, want nothing", got)
	}
}

// Commit records the permission it finds on disk: x for an executable and,
// with allow-symlinks on, l for a symlink whose content is its target.
func TestCommitRecordsModeChanges(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	co := permCheckout(t, r)
	chmod(t, co, "plain.txt", 0o755)
	chmod(t, co, "run.sh", 0o644)
	replaceWithSymlink(t, co, "link", "run.sh")
	rid, _, err := co.Commit(CommitOpts{Message: "modes", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	perms := map[string]string{}
	for _, f := range files {
		perms[f.Name] = f.Perm
	}
	want := map[string]string{"plain.txt": "x", "run.sh": "", "link": "l"}
	for name, perm := range want {
		if perms[name] != perm {
			t.Errorf("%s: perm %q, want %q", name, perms[name], perm)
		}
	}
	if got := readCommitted(t, r, rid, "link"); got != "run.sh" {
		t.Errorf("link content %q, want its target run.sh", got)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("after commit: status %v, want nothing", got)
	}
}

func readCommitted(t *testing.T, r *repo.Repo, rid libfossil.FslID, name string) string {
	t.Helper()
	data, err := content.Expand(r.DB(), fileRIDByName(t, r, rid, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertStatus(t *testing.T, got, want map[string]FileChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("status = %v, want %v", got, want)
	}
	for name, change := range want {
		if got[name] != change {
			t.Errorf("%s: %v, want %v", name, got[name], change)
		}
	}
}

// createdCheckout creates a checkout of r's tip in a fresh directory,
// leaving every file unwritten, as Create does.
func createdCheckout(t *testing.T, r *repo.Repo) (*Checkout, libfossil.FslID) {
	t.Helper()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	return co, rid
}

// With allow-symlinks off, fossil writes a symlink entry as a plain file
// holding its target and keeps islink, so its scan reports UNLINK and its
// checkout refuses without --force, even right after a forced one. The
// first Extract after Create plays fossil's open and is not refused: every
// file is missing, so nothing on disk is lost.
func TestExtractSymlinkEntryWithSymlinksOff(t *testing.T) {
	co, rid := createdCheckout(t, permRepo(t))
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatalf("first Extract without Force: %v", err)
	}
	assertStatus(t, statusOf(t, co), map[string]FileChange{"link": ChangeUnlink})
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{}); err == nil {
		t.Fatal("Extract without Force over UNLINK succeeded; fossil's checkout refuses")
	}
	assertStatus(t, statusOf(t, co), map[string]FileChange{"link": ChangeUnlink})
}

// With allow-symlinks on, Extract writes a symlink entry as a symlink, as
// fossil's vfile_to_disk does, so the checkout is clean and extracts again
// without Force, after a forced extract too.
func TestExtractSymlinkEntryWithSymlinksOn(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	co, rid := createdCheckout(t, r)
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatalf("first Extract without Force: %v", err)
	}
	target, err := os.Readlink(filepath.Join(co.dir, "link"))
	if err != nil || target != "plain.txt" {
		t.Fatalf("link: Readlink = %q, %v; want a symlink to plain.txt", target, err)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status = %v, want nothing", got)
	}
	for i, force := range []bool{false, true, false} {
		if err := co.Extract(rid, ExtractOpts{Force: force}); err != nil {
			t.Fatalf("Extract %d (Force %v): %v", i, force, err)
		}
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status after extracts = %v, want nothing", got)
	}
}

// Extract over a symlink where a plain file belongs replaces the link, not
// what it points to, as fossil deletes the link first.
func TestExtractReplacesSymlinkWithFile(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	co, rid := createdCheckout(t, r)
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	replaceWithSymlink(t, co, "plain.txt", "run.sh")
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(co.dir, "plain.txt"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("plain.txt: %v, %v; want a regular file", info, err)
	}
	data, err := os.ReadFile(filepath.Join(co.dir, "run.sh"))
	if err != nil || string(data) != "#!/bin/sh\n" {
		t.Fatalf("run.sh = %q, %v; the write went through the link", data, err)
	}
}

// ScanENotFile does not refuse a directory at a pending add's path: fossil
// promotes the add to changed first and skips the not-a-file check.
func TestScanENotFileSkipsPendingAdd(t *testing.T) {
	co, rid := createdCheckout(t, permRepo(t))
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec(`INSERT INTO vfile(vid, pathname, rid, mrid, chnged)
		VALUES(?, 'added', 0, 0, 0)`, int64(rid)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(co.dir, "added"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(ScanENotFile); err != nil {
		t.Fatalf("ScanChanges(ScanENotFile) = %v, fossil does not refuse a pending add", err)
	}
	if err := os.Remove(filepath.Join(co.dir, "added")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(co.dir, "plain.txt.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(co.dir, "plain.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(co.dir, "plain.txt.d"), filepath.Join(co.dir, "plain.txt")); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(ScanENotFile); err == nil {
		t.Fatal("ScanChanges(ScanENotFile) accepted a directory at a tracked file's path")
	}
}

// Update writes a symlink entry the target adds as a symlink, with
// allow-symlinks on, as fossil's update does through vfile_to_disk, and
// leaves the executable it points to alone, as fossil's file_setexe skips
// a symlink.
func TestUpdateWritesSymlinkEntry(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	v1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "plain.txt", Content: []byte("plain\n")},
			{Name: "run.sh", Content: []byte("#!/bin/sh\n"), Perm: "x"},
		},
		Comment: "no link", User: "test", Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	v2, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "plain.txt", Content: []byte("plain\n")},
			{Name: "run.sh", Content: []byte("#!/bin/sh\n"), Perm: "x"},
			{Name: "tool", Content: []byte("run.sh"), Perm: "l"},
		},
		Comment: "link", User: "test", Parent: v1,
		Time: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	co, _ := createdCheckout(t, r)
	if err := co.Extract(v1, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(UpdateOpts{TargetRID: v2}); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(co.dir, "tool"))
	if err != nil || target != "run.sh" {
		t.Fatalf("tool: Readlink = %q, %v; want a symlink to run.sh", target, err)
	}
	info, err := os.Stat(filepath.Join(co.dir, "run.sh"))
	if err != nil || !modeIsExecutable(info.Mode()) {
		t.Fatalf("run.sh lost its executable bit: %v, %v", info, err)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status = %v, want nothing", got)
	}
}

// A symlink entry entering the cone is written as Extract writes it: with
// allow-symlinks on, a symlink to its target, leaving the checkout clean.
func TestSetConeMaterializesSymlink(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	narrow, err := cone.New([]string{"plain.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	co, err := Create(r, t.TempDir(), CreateOpts{Cone: narrow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	rid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(co.dir, "link")); !os.IsNotExist(err) {
		t.Fatalf("link outside the cone is on disk: %v", err)
	}
	if _, err := co.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(co.dir, "link"))
	if err != nil || target != "plain.txt" {
		t.Fatalf("link: Readlink = %q, %v; want a symlink to plain.txt", target, err)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status = %v, want nothing", got)
	}
}

// A symlink repointed locally that the target renames and repoints is not
// merged: as fossil's update ("Cannot merge symlink"), it is listed in
// Unmerged, nothing is written through it, and the rename still deletes the
// old name. A dry run touches nothing.
func TestUpdateDoesNotMergeRenamedSymlink(t *testing.T) {
	r := permRepo(t)
	if err := r.SetConfig("allow-symlinks", "on"); err != nil {
		t.Fatal(err)
	}
	files := func(link manifest.File) []manifest.File {
		return []manifest.File{
			{Name: "p.txt", Content: []byte("p\n")},
			{Name: "other.txt", Content: []byte("other\n")},
			link,
		}
	}
	v1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   files(manifest.File{Name: "link", Content: []byte("p.txt"), Perm: "l"}),
		Comment: "A", User: "test", Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	v2, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: files(manifest.File{
			Name: "link2", OldName: "link", Content: []byte("other.txt"), Perm: "l",
		}),
		Comment: "B", User: "test", Parent: v1,
		Time: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	co, _ := createdCheckout(t, r)
	if err := co.Extract(v1, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(co.dir, "link")
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.txt", linkPath); err != nil {
		t.Fatal(err)
	}

	for _, dryRun := range []bool{true, false} {
		res, err := co.Update(UpdateOpts{TargetRID: v2, DryRun: dryRun})
		if err != nil {
			t.Fatalf("dry run %v: %v", dryRun, err)
		}
		if !slices.Equal(res.Unmerged, []string{"link2"}) || len(res.Conflicted) != 0 ||
			slices.Contains(res.FilesWritten, "link2") {
			t.Fatalf("dry run %v: Unmerged = %v, Conflicted = %v, FilesWritten = %v; want link2 unmerged only",
				dryRun, res.Unmerged, res.Conflicted, res.FilesWritten)
		}
		_, lerr := os.Lstat(linkPath)
		if dryRun != (lerr == nil) {
			t.Fatalf("dry run %v: old name link: Lstat = %v", dryRun, lerr)
		}
	}
	if body, err := os.ReadFile(filepath.Join(co.dir, "other.txt")); err != nil || string(body) != "other\n" {
		t.Fatalf("other.txt = %q, %v; written through the link", body, err)
	}
	if _, err := os.Lstat(filepath.Join(co.dir, "link2")); !os.IsNotExist(err) {
		t.Fatalf("link2 written: Lstat = %v", err)
	}
}
