package cli

import (
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
)

// RepoRevertCmd restores files to their checked-out version.
type RepoRevertCmd struct {
	Files []string `arg:"" optional:"" help:"Files to revert (default: all)"`
	Dir   string   `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoRevertCmd) Run(g *Globals) (err error) {
	co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	if err := co.Revert(libfossil.RevertOpts{Files: c.Files}); err != nil {
		return err
	}
	if len(c.Files) == 0 {
		fmt.Println("reverted all changes")
		return nil
	}
	for _, name := range c.Files {
		fmt.Printf("REVERTED %s\n", name)
	}
	return nil
}
