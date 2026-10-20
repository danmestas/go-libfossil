package conformance

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/checkout"
)

func scWorktreeCompleteCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.4.4.record-only-merge", Req: "SC-4.4-1"}, scRecordOnlyMerge},
		{Case{ID: "sparsecone.4.4.record-only-conflict", Req: "SC-4.4-1"}, scRecordOnlyConflict},
	}
}

func TestSparseWorktreeComplete(t *testing.T) {
	for _, c := range scWorktreeCompleteCases() {
		t.Run(c.ID, func(t *testing.T) {
			if err := c.run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func scRecordOnlyMerge(t *testing.T) error {
	for _, tree := range []bool{false, true} {
		f := newSCRepo(t, tree)
		remote := make(map[string]string, len(scFiles)+1)
		for name, text := range scFiles {
			remote[name] = text
		}
		remote["docs/d.txt"] = "remote docs\n"
		remote["tools/new.txt"] = "remote addition\n"
		delete(remote, "tools/t.c")
		remote["src/a.c"] = "remote source\n"
		mid := f.commit(t, remote, f.c1)
		co := f.open(t, scDefault(t), f.c1)
		if _, err := co.Merge(checkout.MergeOpts{Version: mid}); err != nil {
			return err
		}
		if got := scOnDisk(t, co.Dir()); !slices.Equal(got, scDefaultCovered) {
			return fmt.Errorf("merge materialized outside cone: %q", got)
		}
		rid, _, err := co.Commit(checkout.CommitOpts{Message: "merge", User: "u", Time: f.when.Add(time.Hour)})
		if err != nil {
			return err
		}
		got, err := scContent(f.r, rid)
		if err != nil {
			return err
		}
		if err := scSameContent("record-only merge", got, remote); err != nil {
			return err
		}
	}
	return nil
}

func scRecordOnlyConflict(t *testing.T) error {
	f := newSCRepo(t, false)
	files := func(text string) map[string]string {
		out := make(map[string]string, len(scFiles))
		for name, text := range scFiles {
			out[name] = text
		}
		out["docs/d.txt"] = text
		return out
	}
	ours := f.commit(t, files("ours\n"), f.c1)
	theirs := f.commit(t, files("theirs\n"), f.c1)
	co := f.open(t, scDefault(t), ours)
	if _, err := co.Merge(checkout.MergeOpts{Version: theirs}); err != nil {
		return err
	}
	if got := scOnDisk(t, co.Dir()); !slices.Equal(got, scDefaultCovered) {
		return fmt.Errorf("outside conflict materialized a file: %q", got)
	}
	conflicts, err := co.Conflicts()
	if err != nil || !slices.Equal(conflicts, []string{"docs/d.txt"}) {
		return fmt.Errorf("record-only conflicts = %q: %v", conflicts, err)
	}
	_, _, err = co.Commit(checkout.CommitOpts{Message: "unresolved", User: "u", Time: f.when.Add(time.Hour)})
	if !errors.Is(err, checkout.ErrUnresolvedConflicts) {
		return fmt.Errorf("record-only conflict commit: %v, want unresolved-conflict refusal", err)
	}
	wider, err := co.Cone().With([]string{"docs"}, nil)
	if err != nil {
		return err
	}
	if _, err := co.SetCone(wider); err != nil {
		return fmt.Errorf("widen to resolve record-only conflict: %w", err)
	}
	scWrite(t, co.Dir(), "docs/d.txt", "resolved\n")
	rid, _, err := co.Commit(checkout.CommitOpts{Message: "resolved", User: "u", Time: f.when.Add(2 * time.Hour)})
	if err != nil {
		return err
	}
	got, err := scContent(f.r, rid)
	if err != nil {
		return err
	}
	return scSameContent("resolved record-only conflict", got, files("resolved\n"))
}
