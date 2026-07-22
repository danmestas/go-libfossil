package cli

import (
	"fmt"
	"os"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// RepoCatCmd outputs the content of an artifact.
type RepoCatCmd struct {
	Artifact string `arg:"" help:"Artifact UUID or prefix"`
	Raw      bool   `help:"Output raw blob (no delta expansion)"`
}

func (c *RepoCatCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	rid, err := resolveRID(r, c.Artifact)
	if err != nil {
		return err
	}

	db := r.Inner().DB()
	var data []byte
	if c.Raw {
		data, err = blob.Load(db, fsltype.FslID(rid))
	} else {
		data, err = content.Expand(db, fsltype.FslID(rid))
	}
	if err != nil {
		return fmt.Errorf("reading artifact: %w", err)
	}

	os.Stdout.Write(data)
	return nil
}
