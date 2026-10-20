package cli

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/manifest"
)

// RepoLogCmd lists the check-ins that changed one path, newest first, as
// fossil finfo does.
type RepoLogCmd struct {
	Path  string `required:"" help:"File name in the repository"`
	JSON  bool   `help:"Print a JSON array instead of text"`
	Limit int    `short:"n" help:"Number of entries (default: all)"`
}

// logEntry is one element of log's --json array.
type logEntry struct {
	UUID     string `json:"uuid"`
	Time     string `json:"time"`
	User     string `json:"user"`
	Comment  string `json:"comment"`
	Action   string `json:"action"`
	FileUUID string `json:"file_uuid,omitempty"`
	PrevName string `json:"prev_name,omitempty"`
}

// jsonTime is the time format of every JSON field: RFC 3339, UTC, with
// milliseconds, the precision a check-in records.
const jsonTime = "2006-01-02T15:04:05.000Z"

func (c *RepoLogCmd) Run(g *Globals) (err error) {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := r.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	paths := newRepoPaths(r)
	versions, err := manifest.FileHistory(r.Inner(), manifest.FileHistoryOpts{
		Path: paths.clean(c.Path), Limit: c.Limit, FoldCase: paths.foldCase,
	})
	if errors.Is(err, sql.ErrNoRows) || err == nil && len(versions) == 0 {
		return notFound(fmt.Errorf("no history for %q", c.Path))
	}
	if err != nil {
		return err
	}

	entries := make([]logEntry, len(versions))
	for i, v := range versions {
		entries[i] = logEntry{
			UUID:     v.CheckinUUID,
			Time:     v.Date.UTC().Format(jsonTime),
			User:     v.User,
			Comment:  v.Comment,
			Action:   v.Action.String(),
			FileUUID: v.FileUUID,
			PrevName: v.PrevName,
		}
	}
	if c.JSON {
		return writeJSON(entries)
	}
	for _, e := range entries {
		fmt.Printf("%s  %s  %s  %s  %s\n", e.UUID[:10], e.Time, e.User, e.Action, e.Comment)
	}
	return nil
}
