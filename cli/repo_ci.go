package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	libfossil "github.com/danmestas/libfossil"
)

// RepoCiCmd creates a new checkin (commit).
type RepoCiCmd struct {
	Message string   `short:"m" required:"" help:"Checkin comment"`
	Files   []string `arg:"" required:"" help:"Files to checkin"`
	User    string   `help:"Checkin user (default: OS username)"`
	Parent  string   `help:"Parent version UUID (default: tip)"`
	Branch  string   `help:"Branch name for this checkin"`
}

func (c *RepoCiCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	var parentRid int64
	if c.Parent != "" {
		parentRid, err = resolveRID(r, c.Parent)
		if err != nil {
			return fmt.Errorf("resolving parent: %w", err)
		}
	} else {
		parentRid, _ = resolveRID(r, "") // ignore error for initial checkin
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	files := make([]libfossil.FileToCommit, len(c.Files))
	for i, path := range c.Files {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", path, err)
		}
		name, err := filepath.Rel(cwd, absPath)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", path, err)
		}
		name = filepath.Clean(name)
		if name == "." || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s is outside current directory", path)
		}
		files[i] = libfossil.FileToCommit{
			Name:    filepath.ToSlash(name),
			Content: data,
		}
	}

	user := c.User
	if user == "" {
		user = currentUser()
	}

	var tags []libfossil.TagSpec
	if c.Branch != "" {
		tags = append(tags,
			libfossil.TagSpec{Name: "branch", Value: c.Branch},
			libfossil.TagSpec{Name: "sym-" + c.Branch},
		)
	}

	rid, uuid, err := r.Commit(libfossil.CommitOpts{
		Files:    files,
		Comment:  c.Message,
		User:     user,
		ParentID: parentRid,
		Time:     time.Now().UTC(),
		Tags:     tags,
	})
	if err != nil {
		return err
	}

	fmt.Printf("checkin %s (rid=%d)\n", uuid[:10], rid)
	return nil
}
