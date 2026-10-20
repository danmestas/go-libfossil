package cli

import (
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
)

// RepoNewCmd creates a new Fossil repository, at Path or else at the -R path.
type RepoNewCmd struct {
	Path string `arg:"" optional:"" help:"Path for new repository file (default: -R)"`
	User string `help:"Default user name" default:""`
}

func (c *RepoNewCmd) Run(g *Globals) error {
	user := c.User
	if user == "" {
		user = currentUser()
	}
	path := c.Path
	if path == "" {
		path = g.Repo
	}
	if path == "" {
		return fmt.Errorf("repo new: no path: give PATH or -R")
	}
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: user})
	if err != nil {
		return err
	}
	if err := r.Close(); err != nil {
		return err
	}
	fmt.Printf("created repository: %s\n", path)
	return nil
}
