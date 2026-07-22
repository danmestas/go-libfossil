package cli

import (
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
)

// RepoAnnotateCmd annotates file lines with version history.
type RepoAnnotateCmd struct {
	File    string `arg:"" required:"" help:"File to annotate"`
	Version string `help:"Starting version (default: tip)"`
}

func (c *RepoAnnotateCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	startRID, err := resolveRID(r, c.Version)
	if err != nil {
		return err
	}

	lines, err := r.Annotate(libfossil.AnnotateOpts{
		FilePath: c.File,
		StartRID: startRID,
	})
	if err != nil {
		return err
	}

	for _, l := range lines {
		uuid := l.UUID
		if len(uuid) > 10 {
			uuid = uuid[:10]
		}
		fmt.Printf("%s %8s %s | %s\n",
			uuid, l.User, l.Date.Format("2006-01-02"), l.Text)
	}
	return nil
}

// RepoBlameCmd is an alias for annotate.
type RepoBlameCmd struct {
	RepoAnnotateCmd
}
