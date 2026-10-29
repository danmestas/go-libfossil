package content_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

// lose takes a tree the memo read away from the store, one way per name:
// "phantom" makes its manifest a phantom; "node" deletes the sub-manifest
// nodes it cites, which it and the trees sharing a node go on citing;
// "source" makes a phantom of the source its stored delta expands from;
// "sourceless" deletes the row of the blob it is a delta of
// (storeTreesOnBlobs). The last two leave the manifest held but
// unexpandable. They return false for a manifest stored whole. (Deleting a
// manifest is shunning it, which fossil does:
// TestMemoTreesFollowLostTreesFossilWritten.)
func lose(t *testing.T, r *repo.Repo, how string, rid libfossil.FslID) bool {
	t.Helper()
	q := r.DB()
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := q.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	switch how {
	case "node":
		for name := range lostNodes(t, r, rid) {
			run("DELETE FROM delta WHERE rid=(SELECT rid FROM blob WHERE uuid=?)", name)
			run("DELETE FROM blob WHERE uuid=?", name)
		}
	case "sourceless":
		var src libfossil.FslID
		err := q.QueryRow(`SELECT coalesce((SELECT srcid FROM delta WHERE rid=?1
			AND srcid NOT IN (SELECT objid FROM event)),0)`, rid).Scan(&src)
		if err != nil {
			t.Fatal(err)
		}
		if src == 0 {
			return false
		}
		run("DELETE FROM blob WHERE rid=?", src)
	case "source":
		var src libfossil.FslID
		if err := q.QueryRow("SELECT coalesce((SELECT srcid FROM delta WHERE rid=?),0)", rid).Scan(&src); err != nil {
			t.Fatal(err)
		}
		if src == 0 {
			return false
		}
		rid = src
		fallthrough
	case "phantom":
		run("DELETE FROM delta WHERE rid=?", rid)
		run("UPDATE blob SET size=-1, content=NULL WHERE rid=?", rid)
		run("INSERT OR IGNORE INTO phantom(rid) VALUES(?)", rid)
	default:
		t.Fatalf("lose: %q", how)
	}
	return true
}

// lostNodes names the sub-manifest nodes the tree rid holds cites.
func lostNodes(t *testing.T, r *repo.Repo, rid libfossil.FslID) map[string]bool {
	t.Helper()
	data, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	d, err := deck.Parse(data)
	if err != nil || len(d.S) == 0 {
		t.Fatalf("tree %d: no sub-manifest node (%v)", rid, err)
	}
	names := map[string]bool{}
	for _, sc := range d.S {
		names[sc.UUID] = true
	}
	return names
}

// lostTrees has a memo classify c's horizons over m, then loses each tree
// in turn (lose) and has the memo classify again: every cone must classify
// as a fresh read of the changed store does, warm or cold.
func lostTrees(t *testing.T, m *memoRepo, c libfossil.FslID, lost []libfossil.FslID, how string) {
	t.Helper()
	ctx := context.Background()
	memo := content.NewMemo()
	classify := func() int {
		t.Helper()
		v, err := memo.View(ctx, m.r.DB())
		if err != nil {
			t.Fatal(err)
		}
		for _, sc := range m.memoCones(c) {
			sameClassification(t, m.r, v, sc)
		}
		v.Publish()
		_, reads := v.TreesRead()
		return reads
	}
	if classify() == 0 {
		t.Fatal("the first horizon read no tree")
	}
	changed := false
	for _, rid := range lost {
		if lose(t, m.r, how, rid) {
			changed = true
			classify()
		}
	}
	if !changed {
		t.Fatalf("no tree of %v is stored as a delta", lost)
	}
}

// draft-fossil-horizon-00 §6.2: the memo serves a tree it read only where a
// fresh read would give the same tree. A tree whose manifest the store
// later makes a phantom of, deletes, or can no longer expand (a delta whose
// source it lost) classifies as a fresh read of that store does.
// Sub-manifest trees are too small for the deltify policy, so they lose
// their nodes instead of their sources.
func TestMemoTreesFollowLostTrees(t *testing.T) {
	for _, mode := range []hzMode{hzFull, hzDelta, hzTree} {
		for _, how := range []string{"phantom", "source", "sourceless", "node"} {
			if (mode == hzTree) != (how == "node") && how != "phantom" {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", mode, how), func(t *testing.T) {
				m := newMemoRepo(t, mode)
				tip := m.commit(0)
				for i := 0; i < 5; i++ {
					tip = m.commit(tip)
				}
				switch how {
				case "source":
					storeTreesAsDeltas(t, m)
				case "sourceless":
					storeTreesOnBlobs(t, m)
				}
				lost := []libfossil.FslID{m.tips[4], m.tips[2]}
				if how == "node" {
					// Until no tree names the files below the horizon.
					lost = m.tips
				}
				lostTrees(t, m, m.tips[1], lost, how)
			})
		}
	}
}

// storeTreesAsDeltas stores each check-in manifest but the last as a delta
// of the one after it where the deltify policy takes it, as fossil stores a
// parent against its child.
func storeTreesAsDeltas(t *testing.T, m *memoRepo) {
	t.Helper()
	err := m.r.DB().WithTx(func(tx *db.Tx) error {
		for i := len(m.tips) - 2; i >= 0; i-- {
			if _, err := content.Deltify(tx, m.tips[i], m.tips[i+1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// storeTreesOnBlobs stores each check-in manifest the deltify policy takes
// as a delta of a blob no artifact names, its bytes and a newline, in place
// of any delta it was stored as.
func storeTreesOnBlobs(t *testing.T, m *memoRepo) {
	t.Helper()
	err := m.r.DB().WithTx(func(tx *db.Tx) error {
		for _, rid := range m.tips {
			data, err := content.Expand(tx, rid)
			if err != nil {
				return err
			}
			src, _, err := blob.Store(tx, append(data, '\n'))
			if err != nil {
				return err
			}
			if err := content.Undelta(tx, rid); err != nil {
				return err
			}
			if _, err := content.Deltify(tx, rid, src); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The same over a store stock fossil wrote, which stores each parent
// manifest as a delta of its child, and from which fossil itself shuns a
// check-in: a shun row and a rebuild delete its manifest.
func TestMemoTreesFollowLostTreesFossilWritten(t *testing.T) {
	for _, how := range []string{"shun", "phantom", "source"} {
		t.Run(how, func(t *testing.T) {
			m, path := newFossilMemoRepo(t, 6)
			if how != "shun" {
				lostTrees(t, m, m.tips[1], []libfossil.FslID{m.tips[4], m.tips[2]}, how)
				return
			}
			ctx := context.Background()
			memo := content.NewMemo()
			v, err := memo.View(ctx, m.r.DB())
			if err != nil {
				t.Fatal(err)
			}
			for _, sc := range m.memoCones(m.tips[1]) {
				sameClassification(t, m.r, v, sc)
			}
			v.Publish()
			shunned := m.uuid(m.tips[3])
			m.r.Close()
			bin := testutil.RequireFossilBin(t)
			// As fossil's /shun page does: a shun row, then a rebuild.
			fossil(t, bin, filepath.Dir(path), "sql", "-R", path,
				fmt.Sprintf("INSERT INTO shun(uuid,mtime,scom) VALUES('%s',now(),'test');", shunned))
			fossil(t, bin, filepath.Dir(path), "rebuild", path)
			if m.r, err = repo.Open(path); err != nil {
				t.Fatal(err)
			}
			var held bool
			if err := m.r.DB().QueryRow("SELECT EXISTS(SELECT 1 FROM blob WHERE uuid=? AND size>=0)",
				shunned).Scan(&held); err != nil || held {
				t.Fatalf("the shunned manifest is still held (%v)", err)
			}
			if v, err = memo.View(ctx, m.r.DB()); err != nil {
				t.Fatal(err)
			}
			for _, sc := range m.memoCones(m.tips[1]) {
				sameClassification(t, m.r, v, sc)
			}
		})
	}
}

// newFossilMemoRepo has stock fossil write a store of n check-ins in a
// line, each changing one file, and opens it.
func newFossilMemoRepo(t *testing.T, n int) (*memoRepo, string) {
	t.Helper()
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "memo.fossil")
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	fossil(t, bin, dir, "init", path)
	fossil(t, bin, work, "open", path)
	files := []string{"lib/x.c", "lib/y.c", "src/a.c", "src/b.c"}
	// After the check-in fossil init writes now, which a commit may not
	// precede.
	when := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < n; i++ {
		for j, name := range files {
			body := fmt.Sprintf("%s\n", name)
			if j == i%len(files) {
				body = fmt.Sprintf("v%d\n", i)
			}
			if err := os.MkdirAll(filepath.Join(work, filepath.Dir(name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		fossil(t, bin, work, "addremove")
		when = when.Add(time.Hour)
		fossil(t, bin, work, "commit", "-m", fmt.Sprintf("c%d", i), "--no-warnings",
			"--date-override", when.Format("2006-01-02T15:04:05"))
	}
	fossil(t, bin, work, "close", "--force")
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := &memoRepo{t: t, r: r, mode: hzFull, when: when}
	t.Cleanup(func() { m.r.Close() })
	rows, err := r.DB().Query("SELECT objid FROM event WHERE type='ci' ORDER BY mtime")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			t.Fatal(err)
		}
		m.tips = append(m.tips, rid)
	}
	if err := rows.Err(); err != nil || len(m.tips) != n+1 {
		t.Fatalf("%d check-ins (%v), want %d", len(m.tips), err, n+1)
	}
	return m, path
}

func fossil(t *testing.T, bin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fossil %v: %v\n%s", args, err, out)
	}
}
