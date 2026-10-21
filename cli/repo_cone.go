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

	cone := co.Cone()
	if cone == nil {
		fmt.Println("dense")
		return nil
	}
	for _, m := range cone.Include() {
		fmt.Println(m)
	}
	for _, m := range cone.Exclude() {
		fmt.Println("!" + m)
	}
	return nil
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
