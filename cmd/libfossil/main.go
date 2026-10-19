package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/danmestas/go-libfossil/cli"
)

// CLI is the top-level command structure.
//
// Version is deliberately a subcommand only, not also a global --version
// flag: `repo extract` already has its own --version flag (the source
// version to extract from), and kong's global flags are visible in every
// subcommand's context, so a root-level --version would collide with it.
// Renaming that existing, unrelated flag to make room isn't part of this
// change.
type CLI struct {
	cli.Globals

	Repo    cli.RepoCmd    `cmd:"" help:"Repository operations"`
	Version cli.VersionCmd `cmd:"" help:"Print version information"`
}

func main() {
	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("libfossil"),
		kong.Description("Fossil-compatible repository tool (pure Go)"),
		kong.UsageOnError(),
	)
	err := ctx.Run(&c.Globals)
	var exit *cli.ExitError
	if errors.As(err, &exit) {
		fmt.Fprintf(os.Stderr, "libfossil: %v\n", exit)
		os.Exit(exit.Code)
	}
	ctx.FatalIfErrorf(err)
}
