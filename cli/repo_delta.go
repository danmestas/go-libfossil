package cli

import (
	"fmt"
	"os"

	"github.com/danmestas/go-libfossil/internal/delta"
)

// RepoDeltaCmd groups delta create/apply operations.
type RepoDeltaCmd struct {
	Create RepoDeltaCreateCmd `cmd:"" help:"Create a delta between two files"`
	Apply  RepoDeltaApplyCmd  `cmd:"" help:"Apply a delta to a source file"`
}

// RepoDeltaCreateCmd creates a delta between two files.
type RepoDeltaCreateCmd struct {
	Source string `arg:"" help:"Source (original) file"`
	Target string `arg:"" help:"Target (new) file"`
	Output string `short:"o" help:"Output file (default: stdout)"`
}

func (c *RepoDeltaCreateCmd) Run(g *Globals) error {
	src, err := os.ReadFile(c.Source)
	if err != nil {
		return fmt.Errorf("reading source: %w", err)
	}
	tgt, err := os.ReadFile(c.Target)
	if err != nil {
		return fmt.Errorf("reading target: %w", err)
	}

	d := delta.Create(src, tgt)

	if c.Output != "" {
		return os.WriteFile(c.Output, d, 0o644)
	}
	os.Stdout.Write(d)
	return nil
}

// RepoDeltaApplyCmd applies a delta to a source file.
type RepoDeltaApplyCmd struct {
	Source string `arg:"" help:"Source (original) file"`
	Delta  string `arg:"" help:"Delta file"`
	Output string `short:"o" help:"Output file (default: stdout)"`
}

func (c *RepoDeltaApplyCmd) Run(g *Globals) error {
	src, err := os.ReadFile(c.Source)
	if err != nil {
		return fmt.Errorf("reading source: %w", err)
	}
	d, err := os.ReadFile(c.Delta)
	if err != nil {
		return fmt.Errorf("reading delta: %w", err)
	}

	result, err := delta.Apply(src, d)
	if err != nil {
		return fmt.Errorf("applying delta: %w", err)
	}

	if c.Output != "" {
		return os.WriteFile(c.Output, result, 0o644)
	}
	os.Stdout.Write(result)
	return nil
}
