package cli

import (
	"fmt"
	"slices"
)

// RepoMergeResolveCmd marks a file conflict as resolved.
//
// A merge conflict needs no marking: as in fossil, a file is in conflict
// while it holds conflict markers, and editing them out resolves it. So this
// command refuses a file that still holds markers, and resolves the
// conflict-fork entries the conflict-fork strategy records.
type RepoMergeResolveCmd struct {
	File string `arg:"" help:"File to mark as resolved"`
	Dir  string `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoMergeResolveCmd) Run(g *Globals) (err error) {
	if _, err := checkoutDBPath(c.Dir); err == nil {
		conflicted, err := checkoutConflicts(g, c.Dir)
		if err != nil {
			return err
		}
		if slices.Contains(conflicted, c.File) {
			return fmt.Errorf("%s still has conflict markers; edit them out to resolve it", c.File)
		}
	}

	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer closeWith(r.Close, &err)
	forks, err := r.ListConflictForks()
	if err != nil {
		return err
	}
	if !slices.Contains(forks, c.File) {
		return fmt.Errorf("%s: no conflict found", c.File)
	}
	if err := r.ResolveConflictFork(c.File); err != nil {
		return err
	}
	fmt.Printf("resolved: %s (conflict-fork)\n", c.File)
	return nil
}

// checkoutConflicts lists the files in the checkout in dir that hold
// conflict markers.
func checkoutConflicts(g *Globals, dir string) (conflicted []string, err error) {
	_, co, done, err := openWorkingCheckout(g, dir)
	if err != nil {
		return nil, err
	}
	defer closeWith(done, &err)
	return co.Conflicts()
}
