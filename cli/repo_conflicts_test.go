package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/merge"
)

func TestExpandForkFileExpandsInheritedFileWithoutChildMlink(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "sparse-mlink.fossil")
	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	want := []byte("inherited bytes\n")
	rootRID, _, err := r.Commit(libfossil.CommitOpts{
		Files:   []libfossil.FileToCommit{{Name: "inherited.txt", Content: want}},
		Comment: "root",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("commit root: %v", err)
	}
	childRID, _, err := r.Commit(libfossil.CommitOpts{
		Comment:  "inherits inherited.txt unchanged",
		User:     "test",
		ParentID: rootRID,
	})
	if err != nil {
		t.Fatalf("commit child: %v", err)
	}

	var childMlinkRows int
	if err := r.Inner().DB().QueryRow(`
		SELECT count(*)
		FROM mlink m
		JOIN filename fn ON fn.fnid = m.fnid
		WHERE m.mid = ? AND fn.name = ?`, childRID, "inherited.txt").Scan(&childMlinkRows); err != nil {
		t.Fatalf("count child mlink rows: %v", err)
	}
	if childMlinkRows != 0 {
		t.Fatalf("child mlink rows for inherited.txt = %d, want 0", childMlinkRows)
	}

	got, present, err := expandForkFile(r.Inner(), childRID, "inherited.txt", 1)
	if err != nil {
		t.Fatalf("expandForkFile inherited child file: %v", err)
	}
	if !present {
		t.Fatal("expandForkFile inherited child file reported absent")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("expandForkFile = %q, want %q", got, want)
	}
}

func TestRepoConflictsPickDoesNotWriteOrResolveWhenExpansionFails(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "conflicts.fossil")
	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	checkinRID, _, err := r.Commit(libfossil.CommitOpts{
		Files:   []libfossil.FileToCommit{{Name: "present.txt", Content: []byte("present\n")}},
		Comment: "root",
		User:    "test",
	})
	if err != nil {
		r.Close()
		t.Fatalf("commit root: %v", err)
	}
	if err := merge.EnsureConflictTable(r.Inner()); err != nil {
		r.Close()
		t.Fatalf("EnsureConflictTable: %v", err)
	}
	if err := merge.RecordConflictFork(r.Inner(), "missing.txt", checkinRID, checkinRID+999999, checkinRID); err != nil {
		r.Close()
		t.Fatalf("RecordConflictFork: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	outDir := filepath.Join(tmp, "checkout")
	outPath := filepath.Join(outDir, "missing.txt")
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	original := []byte("leave this file untouched\n")
	if err := os.WriteFile(outPath, original, 0o644); err != nil {
		t.Fatalf("write existing output: %v", err)
	}

	err = (&RepoConflictsPickCmd{File: "missing.txt", Local: true, Dir: outDir}).Run(&Globals{Repo: repoPath})
	if err == nil {
		t.Fatal("RepoConflictsPickCmd.Run succeeded after expansion failure")
	}
	if !strings.Contains(err.Error(), "missing.txt") {
		t.Fatalf("RepoConflictsPickCmd.Run error = %q, want missing filename", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output after failed pick: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("output after failed pick = %q, want unchanged %q", got, original)
	}

	opened, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer opened.Close()
	var conflicts int
	if err := opened.Inner().DB().QueryRow("SELECT count(*) FROM conflict WHERE filename=?", "missing.txt").Scan(&conflicts); err != nil {
		t.Fatalf("count unresolved conflict: %v", err)
	}
	if conflicts != 1 {
		t.Fatalf("unresolved conflict rows = %d, want 1", conflicts)
	}
}

func TestRepoMergeConflictForkPickRemoteUsesCheckinRIDs(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "conflicts.fossil")
	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	baseRID, _, err := r.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: "conflict.txt", Content: []byte("base\n")},
			{Name: "inherited.txt", Content: []byte("unchanged\n")},
		},
		Comment: "base",
		User:    "test",
	})
	if err != nil {
		r.Close()
		t.Fatalf("commit base: %v", err)
	}
	remoteRID, remoteVersion, err := r.Commit(libfossil.CommitOpts{
		Files:    []libfossil.FileToCommit{{Name: "conflict.txt", Content: []byte("remote\n")}},
		Comment:  "remote",
		User:     "test",
		ParentID: baseRID,
	})
	if err != nil {
		r.Close()
		t.Fatalf("commit remote: %v", err)
	}
	localRID, _, err := r.Commit(libfossil.CommitOpts{
		Files:    []libfossil.FileToCommit{{Name: "conflict.txt", Content: []byte("local\n")}},
		Comment:  "local",
		User:     "test",
		ParentID: baseRID,
	})
	if err != nil {
		r.Close()
		t.Fatalf("commit local: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	checkoutDir := filepath.Join(tmp, "checkout")
	g := &Globals{Repo: repoPath}
	if err := (&RepoOpenCmd{Dir: checkoutDir}).Run(g); err != nil {
		t.Fatalf("RepoOpenCmd.Run: %v", err)
	}
	if err := (&RepoMergeCmd{
		Version:  remoteVersion,
		Strategy: "conflict-fork",
		Dir:      checkoutDir,
	}).Run(g); err != nil {
		t.Fatalf("RepoMergeCmd.Run: %v", err)
	}

	opened, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("Open after merge: %v", err)
	}
	var gotBaseRID, gotLocalRID, gotRemoteRID int64
	if err := opened.Inner().DB().QueryRow(
		"SELECT base_rid, local_rid, remote_rid FROM conflict WHERE filename=?",
		"conflict.txt",
	).Scan(&gotBaseRID, &gotLocalRID, &gotRemoteRID); err != nil {
		opened.Close()
		t.Fatalf("read conflict row: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close after merge: %v", err)
	}
	if gotBaseRID != baseRID || gotLocalRID != localRID || gotRemoteRID != remoteRID {
		t.Fatalf(
			"conflict RIDs = (%d, %d, %d), want check-in RIDs (%d, %d, %d)",
			gotBaseRID, gotLocalRID, gotRemoteRID, baseRID, localRID, remoteRID,
		)
	}

	if err := (&RepoConflictsPickCmd{
		File:   "conflict.txt",
		Remote: true,
		Dir:    checkoutDir,
	}).Run(g); err != nil {
		t.Fatalf("RepoConflictsPickCmd.Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(checkoutDir, "conflict.txt"))
	if err != nil {
		t.Fatalf("read picked file: %v", err)
	}
	if want := []byte("remote\n"); !bytes.Equal(got, want) {
		t.Fatalf("picked remote content = %q, want %q", got, want)
	}

	opened, err = libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("Open after pick: %v", err)
	}
	defer opened.Close()
	var conflicts int
	if err := opened.Inner().DB().QueryRow(
		"SELECT count(*) FROM conflict WHERE filename=?",
		"conflict.txt",
	).Scan(&conflicts); err != nil {
		t.Fatalf("count conflict rows: %v", err)
	}
	if conflicts != 0 {
		t.Fatalf("conflict rows after pick = %d, want 0", conflicts)
	}
}

func TestRepoMergeConflictForkPickAbsentLocalRemovesCheckoutFile(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "conflicts.fossil")
	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	baseRID, _, err := r.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: "target.txt", Content: []byte("base\n")},
			{Name: "keep.txt", Content: []byte("keep\n")},
		},
		Comment: "base",
		User:    "test",
	})
	if err != nil {
		r.Close()
		t.Fatalf("commit base: %v", err)
	}
	remoteRID, remoteVersion, err := r.Commit(libfossil.CommitOpts{
		Files:    []libfossil.FileToCommit{{Name: "target.txt", Content: []byte("remote\n")}},
		Comment:  "remote",
		User:     "test",
		ParentID: baseRID,
	})
	if err != nil {
		r.Close()
		t.Fatalf("commit remote: %v", err)
	}
	localCheckinRID, _, err := r.Commit(libfossil.CommitOpts{
		Files:           []libfossil.FileToCommit{{Name: "keep.txt", Content: []byte("keep\n")}},
		Comment:         "local deletes target",
		User:            "test",
		ParentID:        baseRID,
		PartialManifest: true,
	})
	if err != nil {
		r.Close()
		t.Fatalf("commit local deletion: %v", err)
	}
	if localCheckinRID <= 0 {
		r.Close()
		t.Fatalf("local check-in RID = %d, want positive", localCheckinRID)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	checkoutDir := filepath.Join(tmp, "checkout")
	g := &Globals{Repo: repoPath}
	if err := (&RepoOpenCmd{Dir: checkoutDir}).Run(g); err != nil {
		t.Fatalf("RepoOpenCmd.Run: %v", err)
	}
	if err := (&RepoMergeCmd{
		Version:  remoteVersion,
		Strategy: "conflict-fork",
		Dir:      checkoutDir,
	}).Run(g); err != nil {
		t.Fatalf("RepoMergeCmd.Run: %v", err)
	}

	opened, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("Open after merge: %v", err)
	}
	var gotBaseRID, gotLocalRID, gotRemoteRID int64
	if err := opened.Inner().DB().QueryRow(
		"SELECT base_rid, local_rid, remote_rid FROM conflict WHERE filename=?",
		"target.txt",
	).Scan(&gotBaseRID, &gotLocalRID, &gotRemoteRID); err != nil {
		opened.Close()
		t.Fatalf("read conflict row: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close after merge: %v", err)
	}
	if gotBaseRID != baseRID || gotLocalRID != 0 || gotRemoteRID != remoteRID {
		t.Fatalf(
			"conflict RIDs = (%d, %d, %d), want check-in RIDs for present sides (%d, 0, %d)",
			gotBaseRID, gotLocalRID, gotRemoteRID, baseRID, remoteRID,
		)
	}

	targetPath := filepath.Join(checkoutDir, "target.txt")
	if err := os.WriteFile(targetPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("create existing checkout file: %v", err)
	}
	if err := (&RepoConflictsPickCmd{
		File:  "target.txt",
		Local: true,
		Dir:   checkoutDir,
	}).Run(g); err != nil {
		t.Fatalf("RepoConflictsPickCmd.Run: %v", err)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("checkout target after picking absent local side: stat error = %v, want not exist", err)
	}

	opened, err = libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("Open after pick: %v", err)
	}
	defer opened.Close()
	var conflicts int
	if err := opened.Inner().DB().QueryRow(
		"SELECT count(*) FROM conflict WHERE filename=?",
		"target.txt",
	).Scan(&conflicts); err != nil {
		t.Fatalf("count conflict rows: %v", err)
	}
	if conflicts != 0 {
		t.Fatalf("conflict rows after pick = %d, want 0", conflicts)
	}
}

func TestRepoConflictsPickMigratesLegacyBlobConflict(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "legacy-conflicts.fossil")
	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	checkinRID, _, err := r.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: "legacy-base.bin", Content: []byte("legacy base\n")},
			{Name: "legacy-local.bin", Content: []byte("legacy local\n")},
			{Name: "legacy-remote.bin", Content: []byte("legacy remote\n")},
		},
		Comment: "create legacy conflict blobs",
		User:    "test",
	})
	if err != nil {
		r.Close()
		t.Fatalf("commit file blobs: %v", err)
	}

	blobRID := func(name string) int64 {
		var rid int64
		if err := r.Inner().DB().QueryRow(
			`SELECT m.fid FROM mlink m JOIN filename f USING(fnid) WHERE m.mid=? AND f.name=?`,
			checkinRID, name,
		).Scan(&rid); err != nil {
			r.Close()
			t.Fatalf("lookup %s blob RID: %v", name, err)
		}
		if rid <= 0 {
			r.Close()
			t.Fatalf("%s blob RID = %d, want positive", name, rid)
		}
		return rid
	}
	baseRID := blobRID("legacy-base.bin")
	localRID := blobRID("legacy-local.bin")
	remoteRID := blobRID("legacy-remote.bin")

	if _, err := r.Inner().DB().Exec(`CREATE TABLE conflict(
		cid INTEGER PRIMARY KEY,
		filename TEXT NOT NULL,
		base_rid INTEGER REFERENCES blob,
		local_rid INTEGER REFERENCES blob,
		remote_rid INTEGER REFERENCES blob,
		mtime REAL NOT NULL
	)`); err != nil {
		r.Close()
		t.Fatalf("create legacy conflict table: %v", err)
	}
	if _, err := r.Inner().DB().Exec(
		`INSERT INTO conflict(filename, base_rid, local_rid, remote_rid, mtime)
		 VALUES(?, ?, ?, ?, julianday('now'))`,
		"legacy-conflict.txt", baseRID, localRID, remoteRID,
	); err != nil {
		r.Close()
		t.Fatalf("insert legacy conflict row: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	checkoutDir := filepath.Join(tmp, "checkout")
	if err := (&RepoConflictsPickCmd{
		File:   "legacy-conflict.txt",
		Remote: true,
		Dir:    checkoutDir,
	}).Run(&Globals{Repo: repoPath}); err != nil {
		t.Fatalf("RepoConflictsPickCmd.Run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(checkoutDir, "legacy-conflict.txt"))
	if err != nil {
		t.Fatalf("read picked legacy file: %v", err)
	}
	if want := []byte("legacy remote\n"); !bytes.Equal(got, want) {
		t.Fatalf("picked legacy remote content = %q, want %q", got, want)
	}

	opened, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("Open after pick: %v", err)
	}
	defer opened.Close()

	var ridKindColumns int
	if err := opened.Inner().DB().QueryRow(
		`SELECT count(*) FROM pragma_table_info('conflict') WHERE name='rid_kind'`,
	).Scan(&ridKindColumns); err != nil {
		t.Fatalf("find migrated rid_kind column: %v", err)
	}
	if ridKindColumns != 1 {
		t.Fatalf("migrated rid_kind columns = %d, want 1", ridKindColumns)
	}

	var ridKindDefault string
	if err := opened.Inner().DB().QueryRow(
		`SELECT dflt_value FROM pragma_table_info('conflict') WHERE name='rid_kind'`,
	).Scan(&ridKindDefault); err != nil {
		t.Fatalf("read migrated rid_kind default: %v", err)
	}
	if ridKindDefault != "0" {
		t.Fatalf("migrated rid_kind default = %q, want %q", ridKindDefault, "0")
	}

	var conflicts int
	if err := opened.Inner().DB().QueryRow(
		"SELECT count(*) FROM conflict WHERE filename=?",
		"legacy-conflict.txt",
	).Scan(&conflicts); err != nil {
		t.Fatalf("count legacy conflict rows after pick: %v", err)
	}
	if conflicts != 0 {
		t.Fatalf("legacy conflict rows after pick = %d, want 0", conflicts)
	}
}

func TestExpandForkFileRejectsUnknownKindForAbsentRID(t *testing.T) {
	_, _, err := expandForkFile(nil, 0, "target.txt", 2)
	if err == nil {
		t.Fatal("expandForkFile accepted unknown RID kind for absent side")
	}
}
