package cli

import (
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
)

// RepoNewCmd creates a new Fossil repository.
type RepoNewCmd struct {
	Path string `arg:"" help:"Path for new repository file"`
	User string `help:"Default user name" default:""`
}

func (c *RepoNewCmd) Run(g *Globals) error {
	user := c.User
	if user == "" {
		user = currentUser()
	}
	r, err := libfossil.Create(c.Path, libfossil.CreateOpts{User: user})
	if err != nil {
		return err
	}
	r.Close()
	fmt.Printf("created repository: %s\n", c.Path)
	return nil
}
