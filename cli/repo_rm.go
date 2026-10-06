package cli

import "fmt"

// RepoRmCmd stages files for removal from the checkout.
type RepoRmCmd struct {
	Files []string `arg:"" required:"" help:"Files to stage for removal"`
	Dir   string   `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoRmCmd) Run(g *Globals) (err error) {
	_, co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	for _, name := range c.Files {
		if err := co.Remove([]string{name}); err != nil {
			return err
		}
		fmt.Printf("REMOVED  %s\n", name)
	}
	return nil
}
