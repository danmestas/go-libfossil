package cli

import (
	"context"
	"errors"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
)

// RepoMergeCmd merges a divergent version into the current checkout. In a
// store with a structural horizon, a merge whose base lies below the horizon
// asks --remote, else the saved last-sync-url, for the base and deepens the
// horizon to reach it before merging (draft-fossil-horizon-00 §5.5).
type RepoMergeCmd struct {
	RecoveryFlags
	Version  string `arg:"" help:"Version to merge into current checkout"`
	Strategy string `help:"Override merge strategy for all files"`
	DryRun   bool   `help:"Show what would be merged without writing"`
	Dir      string `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoMergeCmd) Run(g *Globals) (err error) {
	r, co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	rid, err := co.ResolveVersion(c.Version)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", c.Version, err)
	}
	opts := libfossil.CheckoutMergeOpts{Version: rid, Strategy: c.Strategy, DryRun: c.DryRun}
	res, err := co.Merge(opts)
	if errors.Is(err, libfossil.ErrBelowHorizon) {
		// Only a crossing contacts a peer: an ordinary merge stays local.
		required, rerr := c.requiredContent(r)
		if rerr != nil {
			return errors.Join(err, rerr)
		}
		if required.Peer != nil {
			ctx, cancel := cloneContext(context.Background(), c.Timeout)
			defer cancel()
			res, err = co.MergeContext(ctx, opts, required)
		}
	}
	if err != nil {
		return err
	}
	printMergeResult(res, c.DryRun)
	return nil
}

// mergeLabels are the labels fossil's merge prints for each action.
var mergeLabels = map[string]string{
	"updated":  "UPDATE",
	"merged":   "MERGE",
	"conflict": "MERGE",
	"kept":     "KEPT",
	"forked":   "FORK",
	"added":    "ADDED",
	"deleted":  "DELETE",
	"mode":     "MODE",
}

// printMergeResult reports a merge the way fossil's merge command does.
func printMergeResult(res libfossil.CheckoutMergeResult, dryRun bool) {
	if len(res.Files) == 0 {
		fmt.Println("merge skipped: nothing to merge")
		return
	}
	conflicts := 0
	for _, f := range res.Files {
		label, ok := mergeLabels[f.Action]
		if !ok {
			panic("cli.printMergeResult: unknown merge action " + f.Action)
		}
		switch f.Action {
		case "conflict":
			fmt.Printf("%s %s\n***** merge conflict in %s\n", label, f.Name, f.Name)
			conflicts++
		case "kept":
			fmt.Printf("%s %s (%s)\n", label, f.Name, f.Reason)
			conflicts++
		case "forked":
			fmt.Printf("%s %s (conflict-fork: all versions preserved)\n", label, f.Name)
			conflicts++
		default:
			fmt.Printf("%s %s\n", label, f.Name)
		}
	}
	if conflicts > 0 {
		fmt.Printf("WARNING: %d merge conflicts\n", conflicts)
	}
	if dryRun {
		fmt.Println("REMINDER: this was a dry run - no files were actually changed.")
	}
}
