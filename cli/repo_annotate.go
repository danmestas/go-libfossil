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

	startRID, err := resolveCheckin(r, c.Version)
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

	horizon := ""
	for _, l := range lines {
		uuid := l.UUID
		if len(uuid) > 10 {
			uuid = uuid[:10]
		}
		if l.BelowHorizon {
			// The line's origin is not held: mark the horizon, as git blame
			// marks a boundary commit, rather than credit its check-in.
			horizon = l.UUID
			fmt.Printf("^%s %8s %10s | %s\n", uuid, "horizon", "", l.Text)
			continue
		}
		fmt.Printf("%s %8s %s | %s\n",
			uuid, l.User, l.Date.Format("2006-01-02"), l.Text)
	}
	if horizon != "" {
		fmt.Printf("^ lines originate at or below the structural horizon at %s, whose history is not held here; "+
			"deepen the horizon (repo cone deepen CHECKIN) to attribute them\n", horizon)
	}
	return nil
}

// RepoBlameCmd is an alias for annotate.
type RepoBlameCmd struct {
	RepoAnnotateCmd
}
