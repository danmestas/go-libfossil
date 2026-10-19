package cli

import (
	"fmt"
	"os"
	"path/filepath"

	libfossil "github.com/danmestas/go-libfossil"
)

// RepoOpenCmd opens a checkout in a directory, creating the .fslckout database.
type RepoOpenCmd struct {
	Dir     string   `arg:"" optional:"" help:"Checkout directory (default: current dir)" default:"."`
	Cone    []string `name:"cone" help:"Make a sparse checkout: materialize and track only this path's subtree (repeatable)"`
	ConeNot []string `name:"cone-not" help:"Exclude this path's subtree from an enclosing --cone (repeatable)"`
}

func (c *RepoOpenCmd) Run(g *Globals) error {
	if g.Repo == "" {
		return fmt.Errorf("repository required (use -R <path>)")
	}

	absRepo, err := filepath.Abs(g.Repo)
	if err != nil {
		return err
	}

	ckoutPath := filepath.Join(c.Dir, ".fslckout")
	if _, err := os.Stat(ckoutPath); err == nil {
		return fmt.Errorf("checkout already exists: %s", ckoutPath)
	}

	cone, err := coneFlags(c.Cone, c.ConeNot)
	if err != nil {
		return err
	}

	g.Repo = absRepo
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	co, err := r.CreateCheckout(c.Dir, libfossil.CheckoutCreateOpts{Cone: cone})
	if err != nil {
		os.Remove(ckoutPath)
		return err
	}
	defer co.Close()

	tipRid, tipHash, err := co.Version()
	if err != nil {
		os.Remove(ckoutPath)
		return err
	}

	fmt.Printf("opened checkout in %s (repo: %s)\n", c.Dir, absRepo)
	if tipRid > 0 && cone != nil {
		// A sparse open materializes the cone: what it covers is the point.
		if err := co.Extract(tipRid, libfossil.ExtractOpts{}); err != nil {
			return err
		}
		fmt.Printf("sparse cone: %s\n", cone)
	}
	if tipRid > 0 {
		fmt.Printf("checked out version %s\n", tipHash[:10])
	} else {
		fmt.Println("empty repository -- no checkins yet")
	}
	return nil
}
