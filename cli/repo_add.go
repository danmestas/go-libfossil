package cli

import (
	"fmt"
	"path/filepath"
)

// RepoAddCmd stages files for addition in the checkout database.
type RepoAddCmd struct {
	Files []string `arg:"" required:"" help:"Files to stage for addition"`
	Dir   string   `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoAddCmd) Run(g *Globals) (err error) {
	_, co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	absDir, err := filepath.Abs(c.Dir)
	if err != nil {
		return err
	}
	for _, path := range c.Files {
		relPath, err := checkoutRelative(absDir, path)
		if err != nil {
			return err
		}
		added, err := co.Add([]string{relPath})
		if err != nil {
			return err
		}
		if added == 0 {
			fmt.Printf("TRACKED  %s\n", relPath)
			continue
		}
		fmt.Printf("ADDED    %s\n", relPath)
	}
	return nil
}

// checkoutRelative turns a file argument, absolute or relative to the
// current directory, into a path relative to the checkout at absDir.
func checkoutRelative(absDir, path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relPath, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return "", fmt.Errorf("file %s is not within checkout directory: %w", path, err)
	}
	if !filepath.IsLocal(relPath) {
		return "", fmt.Errorf("file %s is not within checkout directory", path)
	}
	return filepath.ToSlash(relPath), nil
}
