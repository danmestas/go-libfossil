//go:build !js

package checkout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

// checkoutState is everything Undo must put back: each working file's
// content, each vfile row, and the checked-out version and its hash.
func checkoutState(t *testing.T, co *Checkout) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(co.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(co.dir, path)
		if err != nil || strings.HasPrefix(rel, ".fslckout") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("file %s %q", rel, data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := co.db.Query(`SELECT vid, pathname, coalesce(rid,0), coalesce(mrid,0),
		chnged, deleted, coalesce(origname,'') FROM vfile`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var vid, rid, mrid, chnged, deleted int64
		var name, orig string
		if err := rows.Scan(&vid, &name, &rid, &mrid, &chnged, &deleted, &orig); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("vfile %d %s %d %d %d %d %s", vid, name, rid, mrid, chnged, deleted, orig))
	}
	vid, hash, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines, fmt.Sprintf("version %d %s", vid, hash))
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// Undo after an update restores the working files, the file list and the
// version as they were before it, as fossil's undo does; Redo puts the
// update back.
func TestUndoReversesUpdate(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "merge.txt", "a\nb\nC\n")
	writeWorking(t, co, "clash.txt", "z\n")
	writeWorking(t, co, "gone.txt", "gone, edited\n")
	writeWorking(t, co, "new.txt", "new\n")
	if _, err := co.Manage(ManageOpts{Paths: []string{"new.txt"}}); err != nil {
		t.Fatal(err)
	}
	statusOf(t, co) // the scan the update starts with, recorded beforehand
	before := checkoutState(t, co)

	if _, err := co.Update(UpdateOpts{TargetRID: b}); err != nil {
		t.Fatal(err)
	}
	after := checkoutState(t, co)
	if _, err := co.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := checkoutState(t, co); got != before {
		t.Fatalf("after undo:\n%s\nwant, as before the update:\n%s", got, before)
	}
	if _, err := co.Redo(); err != nil {
		t.Fatal(err)
	}
	if got := checkoutState(t, co); got != after {
		t.Fatalf("after redo:\n%s\nwant, as after the update:\n%s", got, after)
	}
}

// A dry run saves no undo state: the last real operation stays undoable.
func TestUpdateDryRunKeepsUndoState(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	if _, err := co.Undo(); err == nil {
		t.Fatal("undo with nothing saved succeeded")
	}
	if _, err := co.Update(UpdateOpts{TargetRID: b, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Undo(); err == nil {
		t.Fatal("a dry run left something to undo")
	}
}

// failingStorage fails every write to one path.
type failingStorage struct {
	*simio.MemStorage
	path string
}

func (f failingStorage) WriteFile(path string, data []byte, perm os.FileMode) error {
	if path == f.path {
		return errors.New("injected write failure")
	}
	return f.MemStorage.WriteFile(path, data, perm)
}

// An update that fails partway puts back the files it already changed, as
// fossil's undo_rollback does, and leaves the checkout on its version.
func TestUpdateRollsBackFilesOnFailure(t *testing.T) {
	r, a, b := editsRepo(t)
	co, mem := memCheckout(t, r, simio.RealClock{})
	if err := co.Extract(a, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/checkout/merge.txt", []byte("a\nb\nC\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	co.env.Storage = failingStorage{MemStorage: mem, path: "/checkout/plain.txt"}

	if _, err := co.Update(UpdateOpts{TargetRID: b}); err == nil {
		t.Fatal("update succeeded despite the injected failure")
	}
	for name, want := range map[string]string{
		"merge.txt": "a\nb\nC\n", "clash.txt": "x\n", "gone.txt": "gone\n", "plain.txt": "plain\n",
	} {
		got, err := mem.ReadFile("/checkout/" + name)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q put back", name, got, err, want)
		}
	}
	if vid, _, _ := co.Version(); vid != a {
		t.Fatalf("version = %d, want %d", vid, a)
	}
}
