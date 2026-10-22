package cli

import (
	"context"
	"errors"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
)

// ErrConeTestElided is what `repo cone test` returns when any path it was
// given is outside the cone, so the command works as a predicate.
var ErrConeTestElided = errors.New("a path is elided by the sparse cone")

// RepoConeCmd shows or edits a checkout's sparse cone
// (draft-fossil-sparse-cone-00): the paths, directory subtrees or single
// files, that the checkout materializes and tracks. An exclusion carves a
// part out of an inclusion; the longest member covering a path decides.
type RepoConeCmd struct {
	Show    RepoConeShowCmd    `cmd:"" default:"withargs" help:"List the cone's members (\"dense\" when there is none)"`
	Add     RepoConeAddCmd     `cmd:"" help:"Widen the cone, or with --not carve paths out of it"`
	Remove  RepoConeRemoveCmd  `cmd:"" help:"Remove members of either polarity"`
	Set     RepoConeSetCmd     `cmd:"" help:"Replace the cone"`
	Dense   RepoConeDenseCmd   `cmd:"" help:"Drop the cone and materialize every file"`
	Test    RepoConeTestCmd    `cmd:"" help:"Classify paths against the cone and name the deciding member"`
	Store   RepoConeStoreCmd   `cmd:"" help:"Show or replace the repository's store cone, fetching what widening covers"`
	Deepen  RepoConeDeepenCmd  `cmd:"" help:"Move the store's horizon down to reach a check-in, fetching the history it keeps"`
	Compact RepoConeCompactCmd `cmd:"" help:"Drop held blobs the store cone elides (irreversible locally)"`
}

// RepoConeShowCmd lists the inclusions, then the exclusions marked '!'.
type RepoConeShowCmd struct {
	Dir string `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoConeShowCmd) Run(g *Globals) (err error) {
	_, co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	printCone(co.Cone())
	return nil
}

func printCone(cone *libfossil.Cone) {
	if cone == nil {
		fmt.Println("dense")
		return
	}
	if h, ok := cone.Horizon(); ok {
		fmt.Printf("horizon %s %s%s\n", h.Checkin, h.Rule, structuralSuffix(h))
	}
	for _, m := range cone.Include() {
		fmt.Println(m)
	}
	for _, m := range cone.Exclude() {
		fmt.Println("!" + m)
	}
}

// RepoConeStoreCmd shows or replaces the repository's store cone, the policy
// form of demand fetch (draft-fossil-sparse-cone-00 §6.4): artifacts the new
// cone covers that the old one elided are pulled from --remote, else the saved
// last-sync-url. With such artifacts and no peer it refuses and changes nothing.
//
// A store cone may carry a horizon (draft-fossil-horizon-00): --horizon sets
// it, or deepens it when the check-in is older than the current one, with
// --structural taking the structural cut, and --no-horizon drops it. Either
// keeps the store's path members unless paths are given, which replace them.
type RepoConeStoreCmd struct {
	RecoveryFlags
	Paths      []string `arg:"" optional:"" help:"Inclusion members; none shows the store cone"`
	Not        []string `help:"Exclusion members (repeatable)"`
	Dense      bool     `help:"Drop the store cone, fetching everything it elided"`
	Horizon    string   `help:"Elide blobs below this check-in (name, branch or hash prefix), fetching what a deeper horizon keeps"`
	Leaves     bool     `help:"With --horizon, also hold every open leaf's ancestry back to the check-in"`
	Structural bool     `help:"With --horizon, also elide the manifests below the check-in; the next pull fetches the origin's seed"`
	NoHorizon  bool     `help:"Drop the horizon, keeping the path members, and fetch what it elided"`
}

func (c *RepoConeStoreCmd) Run(g *Globals) (err error) {
	members := len(c.Paths) > 0 || len(c.Not) > 0
	horizon := c.Horizon != "" || c.NoHorizon
	switch {
	case c.Dense && (members || horizon):
		return fmt.Errorf("--dense takes no cone members or horizon")
	case c.Horizon != "" && c.NoHorizon:
		return fmt.Errorf("--horizon and --no-horizon are exclusive")
	case c.Leaves && c.Horizon == "":
		return fmt.Errorf("--leaves needs --horizon")
	case c.Structural && c.Horizon == "":
		return fmt.Errorf("--structural needs --horizon")
	}
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer closeWith(r.Close, &err)
	if !c.Dense && !members && !horizon {
		cone, err := r.StoreCone()
		if err != nil {
			return err
		}
		printCone(cone)
		return nil
	}
	var next *libfossil.Cone
	if !c.Dense {
		if next, err = c.storeCone(r, members); err != nil {
			return err
		}
	}
	required, err := c.requiredContent(r)
	if err != nil {
		return err
	}
	ctx, cancel := cloneContext(context.Background(), c.Timeout)
	defer cancel()
	result, err := r.SetStoreConeContext(ctx, next, required)
	if err != nil {
		return err
	}
	fetched := 0
	if result != nil {
		fetched = result.FilesRecvd
	}
	fmt.Printf("fetched %d artifact(s)\n", fetched)
	return nil
}

// storeCone is the store cone the flags ask for. Path members given replace
// the current ones; with none, the current ones stay and only the horizon
// changes.
func (c *RepoConeStoreCmd) storeCone(r *libfossil.Repo, members bool) (*libfossil.Cone, error) {
	if !members && c.Horizon == "" && c.NoHorizon {
		current, err := r.StoreCone()
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, fmt.Errorf("the store cone is dense; there is no horizon to drop")
		}
		if _, ok := current.Horizon(); !ok {
			return nil, fmt.Errorf("the store cone has no horizon to drop")
		}
		if len(current.Include()) == 0 {
			return nil, fmt.Errorf("the store cone is only a horizon; --dense drops it")
		}
		return current.WithHorizon(nil)
	}
	if c.Horizon == "" {
		return coneFlags(c.Paths, c.Not)
	}
	rid, err := resolveRID(r, c.Horizon)
	if err != nil {
		return nil, fmt.Errorf("--horizon %q: %w", c.Horizon, err)
	}
	checkin, err := r.UUIDFromRID(rid)
	if err != nil {
		return nil, err
	}
	h := libfossil.Horizon{Checkin: checkin, Rule: horizonRule(c.Leaves), Structural: c.Structural}
	if members {
		if len(c.Paths) == 0 {
			return nil, fmt.Errorf("--not needs a path to carve from")
		}
		return libfossil.NewHorizonCone(c.Paths, c.Not, h)
	}
	current, err := r.StoreCone()
	if err != nil {
		return nil, err
	}
	if current == nil {
		return libfossil.NewHorizonCone(nil, nil, h)
	}
	return current.WithHorizon(&h)
}

// RepoConeDeepenCmd deepens the store's horizon (draft-fossil-horizon-00
// §5.8) so that it reaches a check-in: C moves to that check-in when it is
// an ancestor of C, else to a common ancestor of the two, which the remote
// is asked for when the store cannot see one. The rule, the depth and the
// path members stay. History comes from --remote, else the saved
// last-sync-url.
type RepoConeDeepenCmd struct {
	RecoveryFlags
	Checkin string `arg:"" help:"Check-in to reach: a full hash, which may lie below the horizon, or a name"`
}

func (c *RepoConeDeepenCmd) Run(g *Globals) (err error) {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer closeWith(r.Close, &err)
	required, err := c.requiredContent(r)
	if err != nil {
		return err
	}
	ctx, cancel := cloneContext(context.Background(), c.Timeout)
	defer cancel()
	result, err := r.DeepenHorizon(ctx, c.Checkin, required)
	if err != nil {
		return err
	}
	cone, err := r.StoreCone()
	if err != nil {
		return err
	}
	printCone(cone)
	fmt.Printf("fetched %d artifact(s)\n", result.FilesRecvd)
	return nil
}

// structuralSuffix marks a structural horizon where one is printed; the
// content horizon, the default, is printed bare.
func structuralSuffix(h libfossil.Horizon) string {
	if h.Structural {
		return " structural"
	}
	return ""
}

func horizonRule(leaves bool) libfossil.HorizonRule {
	if leaves {
		return libfossil.HorizonLeaves
	}
	return libfossil.HorizonDescendants
}

// RepoConeAddCmd adds members to the cone.
type RepoConeAddCmd struct {
	RecoveryFlags
	Paths []string `arg:"" help:"Paths to add"`
	Not   bool     `help:"Add the paths as exclusions"`
	Dir   string   `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoConeAddCmd) Run(g *Globals) error {
	return amendCone(g, c.Dir, c.RecoveryFlags, func(cone *libfossil.Cone) (*libfossil.Cone, error) {
		if cone == nil {
			if c.Not {
				return nil, fmt.Errorf("this checkout is dense; there is nothing to exclude from")
			}
			return libfossil.NewCone(c.Paths, nil)
		}
		if c.Not {
			return cone.With(nil, c.Paths)
		}
		return cone.With(c.Paths, nil)
	})
}

// RepoConeRemoveCmd removes members from the cone.
type RepoConeRemoveCmd struct {
	RecoveryFlags
	Paths []string `arg:"" help:"Members to remove"`
	Dir   string   `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoConeRemoveCmd) Run(g *Globals) error {
	return amendCone(g, c.Dir, c.RecoveryFlags, func(cone *libfossil.Cone) (*libfossil.Cone, error) {
		if cone == nil {
			return nil, fmt.Errorf("this checkout is dense; there is no cone to remove from")
		}
		return cone.Without(c.Paths)
	})
}

// RepoConeSetCmd replaces the cone.
type RepoConeSetCmd struct {
	RecoveryFlags
	Paths []string `arg:"" help:"Inclusion members"`
	Not   []string `help:"Exclusion members (repeatable)"`
	Dir   string   `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoConeSetCmd) Run(g *Globals) error {
	return amendCone(g, c.Dir, c.RecoveryFlags, func(*libfossil.Cone) (*libfossil.Cone, error) {
		return libfossil.NewCone(c.Paths, c.Not)
	})
}

// RepoConeDenseCmd drops the cone.
type RepoConeDenseCmd struct {
	RecoveryFlags
	Dir string `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoConeDenseCmd) Run(g *Globals) error {
	return amendCone(g, c.Dir, c.RecoveryFlags, func(*libfossil.Cone) (*libfossil.Cone, error) {
		return nil, nil
	})
}

// amendCone applies edit to the checkout's cone and reports what moved.
func amendCone(
	g *Globals, dir string, flags RecoveryFlags,
	edit func(*libfossil.Cone) (*libfossil.Cone, error),
) (err error) {
	r, co, done, err := openWorkingCheckout(g, dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	next, err := edit(co.Cone())
	if err != nil {
		return err
	}
	required, err := flags.requiredContent(r)
	if err != nil {
		return err
	}
	ctx, cancel := cloneContext(context.Background(), flags.Timeout)
	defer cancel()
	change, err := co.SetConeContext(ctx, next, required)
	if err != nil {
		return err
	}
	for _, name := range change.Materialized {
		fmt.Printf("ADDED    %s\n", name)
	}
	for _, name := range change.Dematerialized {
		fmt.Printf("REMOVED  %s\n", name)
	}
	return nil
}

// RepoConeCompactCmd is the narrow-then-compact deletion, zit's
// `pack --drop-elided`: it acts on the store cone, not a checkout's, and a
// later widening fetches the dropped blobs from a peer.
type RepoConeCompactCmd struct{}

func (c *RepoConeCompactCmd) Run(g *Globals) (err error) {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer closeWith(r.Close, &err)

	n, err := r.CompactStore(context.Background())
	if err != nil {
		return err
	}
	noun := "artifacts"
	if n == 1 {
		noun = "artifact"
	}
	fmt.Printf("compacted %d %s\n", n, noun)
	return nil
}

// RepoConeTestCmd classifies paths as zit's `cone test` does.
type RepoConeTestCmd struct {
	Paths []string `arg:"" help:"Paths to classify"`
	Quiet bool     `short:"q" help:"Print nothing; report through the exit status only"`
	Dir   string   `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoConeTestCmd) Run(g *Globals) (err error) {
	_, co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	cone := co.Cone()
	width := 0
	for _, p := range c.Paths {
		width = max(width, len(p))
	}
	elided := false
	for _, p := range c.Paths {
		// A dense checkout elides nothing, and no member decided that.
		covered, reason := true, "(dense)"
		if cone != nil {
			covered, reason = cone.Decide(p)
			if reason == "" {
				reason = "(no member covers)"
			}
		}
		elided = elided || !covered
		if c.Quiet {
			continue
		}
		verdict := "covered"
		if !covered {
			verdict = "elided"
		}
		fmt.Printf("%-8s%-*s%s\n", verdict, width+2, p, reason)
	}
	if elided {
		return ErrConeTestElided
	}
	return nil
}

// cloneConeFlags is coneFlags for a clone, which can also start a content
// horizon store. Nothing is local yet, so the check-in must be a full hash.
func cloneConeFlags(include, exclude []string, checkin string, leaves, structural bool) (*libfossil.Cone, error) {
	if checkin == "" {
		if leaves {
			return nil, fmt.Errorf("--leaves needs --horizon")
		}
		if structural {
			return nil, fmt.Errorf("--structural needs --horizon")
		}
		return coneFlags(include, exclude)
	}
	if len(include) == 0 && len(exclude) > 0 {
		return nil, fmt.Errorf("--cone-not needs a --cone to carve from")
	}
	c, err := libfossil.NewHorizonCone(include, exclude,
		libfossil.Horizon{Checkin: checkin, Rule: horizonRule(leaves), Structural: structural})
	if err != nil {
		return nil, fmt.Errorf("--horizon needs the full check-in hash on clone: %w", err)
	}
	return c, nil
}

// coneFlags is the --cone and --cone-not pair of a command that can start a
// sparse checkout.
func coneFlags(include, exclude []string) (*libfossil.Cone, error) {
	if len(include) == 0 && len(exclude) == 0 {
		return nil, nil
	}
	if len(include) == 0 {
		return nil, fmt.Errorf("--cone-not needs a --cone to carve from")
	}
	return libfossil.NewCone(include, exclude)
}
