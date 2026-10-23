package cli

import (
	"context"
	"fmt"
	"path/filepath"

	libfossil "github.com/danmestas/go-libfossil"
)

// RepoCiCmd commits the checkout's changes, as fossil's commit command does:
// every change, or only the named files. A pending merge is committed with
// the merged-in checkin as a merge parent.
type RepoCiCmd struct {
	RecoveryFlags
	NoSync        bool     `help:"Suppress optional autosync, not required-content recovery"`
	Message       string   `short:"m" required:"" help:"Checkin comment"`
	Files         []string `arg:"" optional:"" help:"Files to commit (default: all changes)"`
	User          string   `help:"Checkin user (default: OS username)"`
	Branch        string   `help:"Branch name for this checkin"`
	AllowConflict bool     `help:"Commit files that still hold merge conflict markers"`
	AllowEmpty    bool     `help:"Commit even when nothing has changed"`
	NoSign        bool     `name:"nosign" help:"Do not sign this check-in, even with the clearsign setting on"`
	Dir           string   `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoCiCmd) Run(g *Globals) (err error) {
	r, co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	defer closeWith(done, &err)

	absDir, err := filepath.Abs(c.Dir)
	if err != nil {
		return err
	}
	files := make([]string, 0, len(c.Files))
	for _, path := range c.Files {
		relPath, err := checkoutRelative(absDir, path)
		if err != nil {
			return err
		}
		files = append(files, relPath)
	}

	user := c.User
	if user == "" {
		user = currentUser()
	}
	required, err := c.RecoveryFlags.requiredContent(r)
	if err != nil {
		return err
	}
	ctx, cancel := cloneContext(context.Background(), c.Timeout)
	defer cancel()
	rid, uuid, err := co.CheckinContext(ctx, libfossil.CheckoutCommitOpts{
		Message:       c.Message,
		User:          user,
		Branch:        c.Branch,
		Files:         files,
		AllowConflict: c.AllowConflict,
		AllowEmpty:    c.AllowEmpty,
		NoSign:        c.NoSign,
	}, required)
	if err != nil {
		return err
	}
	fmt.Printf("checkin %s (rid=%d)\n", uuid[:10], rid)
	return nil
}
