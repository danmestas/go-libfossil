package cli

import (
	"fmt"

	libfossil "github.com/danmestas/libfossil"
)

// RepoTimelineCmd shows repository timeline/history.
type RepoTimelineCmd struct {
	Limit int `short:"n" default:"20" help:"Number of entries"`
}

func (c *RepoTimelineCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	tipRid, err := resolveRID(r, "")
	if err != nil {
		return err
	}

	entries, err := r.Timeline(libfossil.LogOpts{Start: tipRid, Limit: c.Limit})
	if err != nil {
		return err
	}

	for _, e := range entries {
		uuid := e.UUID
		if len(uuid) > 10 {
			uuid = uuid[:10]
		}
		// A check-in with no recorded user (event.user IS NULL) comes back
		// from the library as "" — LogEntry.User never leaks a nullable type.
		// Canonical fossil renders that case as "?" on the TTY
		// (coalesce(euser,user,'?') in timeline_query_for_tty()); match it
		// here at the presentation boundary rather than in the library.
		user := e.User
		if user == "" {
			user = "?"
		}
		fmt.Printf("%s  %s  %s  %s\n", uuid, e.Time.Format("2006-01-02 15:04"), user, e.Comment)
	}
	return nil
}
