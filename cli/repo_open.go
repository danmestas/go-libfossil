package cli

import (
	"fmt"
	"os"
	"path/filepath"

	libfossil "github.com/danmestas/libfossil"
)

// RepoOpenCmd opens a checkout in a directory, creating the .fslckout database.
type RepoOpenCmd struct {
	Dir string `arg:"" optional:"" help:"Checkout directory (default: current dir)" default:"."`
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

	g.Repo = absRepo
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	co, err := r.CreateCheckout(c.Dir, libfossil.CheckoutCreateOpts{})
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
	if tipRid > 0 {
		fmt.Printf("checked out version %s\n", tipHash[:10])
	} else {
		fmt.Println("empty repository -- no checkins yet")
	}
	return nil
}
