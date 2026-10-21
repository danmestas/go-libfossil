package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// RepoServeCmd starts an HTTP xfer server for the repository so a remote
// fossil client -- the canonical binary or another instance of this tool --
// can clone or sync against it. This is a thin wrapper: all serving logic
// lives in Repo.ServeHTTP; the command's job is to open the repo, resolve
// the bind address, and translate process interrupt into the context
// cancellation that ServeHTTP already honors. Access follows the user table,
// as with fossil serve: clients without credentials act as nobody, which may
// clone and pull a new repository but not push to it. A repository with a
// store cone refuses clones its cone does not cover unless
// `repo config set partial-mirror on` marks it a partial mirror.
type RepoServeCmd struct {
	Addr string `short:"P" default:"127.0.0.1:8080" help:"Bind address as host:port"`
}

func (c *RepoServeCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("serving %s on http://%s (Ctrl-C to stop)\n", g.Repo, c.Addr)
	if err := r.ServeHTTP(ctx, c.Addr); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
