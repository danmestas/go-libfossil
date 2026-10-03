package cli

import "fmt"

// RepoRenameCmd renames a tracked file in the checkout and on disk.
type RepoRenameCmd struct {
	From string `arg:"" help:"Current file name"`
	To   string `arg:"" help:"New file name"`
	Dir  string `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoRenameCmd) Run(g *Globals) (err error) {
	co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	if err := co.Rename(c.From, c.To); err != nil {
		return err
	}
	fmt.Printf("RENAMED  %s -> %s\n", c.From, c.To)
	return nil
}
