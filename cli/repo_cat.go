package cli

import (
	"errors"
	"fmt"
	"os"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// RepoCatCmd outputs the content of an artifact, or with --at the content of
// a file as a version holds it, as fossil cat -r does.
type RepoCatCmd struct {
	Artifact string `arg:"" help:"Artifact UUID or prefix; with --at, the file name"`
	At       string `name:"at" short:"r" help:"Version to read the file from (tip, trunk, a branch, a uuid or prefix)"`
	Raw      bool   `help:"Output raw blob (no delta expansion)"`
}

func (c *RepoCatCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	if c.At != "" {
		return c.runAt(r)
	}

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

// runAt writes the bytes of file c.Artifact as version c.At holds it. A
// version or file that does not exist is ExitNotFound.
func (c *RepoCatCmd) runAt(r *libfossil.Repo) error {
	rid, err := resolveVersionExit(r, c.At)
	if err != nil {
		return err
	}
	data, err := r.ReadFile(rid, c.Artifact)
	if errors.Is(err, libfossil.ErrFileNotFound) {
		return notFound(err)
	}
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}
