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
// cancellation that ServeHTTP already honors.
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
