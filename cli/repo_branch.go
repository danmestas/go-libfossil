package cli

import (
	"fmt"
	"time"

	libfossil "github.com/danmestas/libfossil"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/tag"
)

// RepoBranchCmd groups branch operations.
type RepoBranchCmd struct {
	Ls    RepoBranchLsCmd    `cmd:"" help:"List branches"`
	New   RepoBranchNewCmd   `cmd:"" help:"Create new branch"`
	Close RepoBranchCloseCmd `cmd:"" help:"Close a branch"`
}

// RepoBranchLsCmd lists branches.
type RepoBranchLsCmd struct {
	Closed bool `help:"Show only closed branches"`
	All    bool `help:"Show all branches (open and closed)"`
}

func (c *RepoBranchLsCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	db := r.Inner().DB()

	var filter string
	switch {
	case c.All:
		// no filter
	case c.Closed:
		filter = " AND tx.tagtype = 0"
	default:
		filter = " AND tx.tagtype > 0"
	}

	query := `
		SELECT DISTINCT substr(t.tagname, 5) AS branch, b.uuid,
		       datetime(e.mtime), e.user
		FROM tag t
		JOIN tagxref tx ON tx.tagid = t.tagid
		JOIN blob b ON b.rid = tx.rid
		LEFT JOIN event e ON e.objid = tx.rid
		WHERE t.tagname LIKE 'sym-%'` + filter + `
		ORDER BY e.mtime DESC`

	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var branch, uuid string
		var mtime, user *string
		if err := rows.Scan(&branch, &uuid, &mtime, &user); err != nil {
			return err
		}
		short := uuid
		if len(short) > 10 {
			short = short[:10]
		}
		mt := ""
		if mtime != nil {
			mt = *mtime
		}
		u := ""
		if user != nil {
			u = *user
		}
		fmt.Printf("%-20s %s  %s  %s\n", branch, short, mt, u)
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		fmt.Println("no branches found")
	}
	return nil
}

// RepoBranchNewCmd creates a new branch.
type RepoBranchNewCmd struct {
	Name    string `arg:"" help:"Branch name"`
	From    string `help:"Parent version (default: tip)"`
	Message string `short:"m" help:"Checkin comment"`
	User    string `help:"Checkin user (default: OS username)"`
}

func (c *RepoBranchNewCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	parentRid, err := resolveRID(r, c.From)
	if err != nil {
		return fmt.Errorf("resolving parent: %w", err)
	}

	entries, err := r.ListFiles(parentRid)
	if err != nil {
		return fmt.Errorf("listing parent files: %w", err)
	}

	db := r.Inner().DB()
	files := make([]libfossil.FileToCommit, 0, len(entries))
	for _, e := range entries {
		fileRid, ok := content.AvailableByUUID(db, e.UUID)
		if !ok {
			return fmt.Errorf("blob %s not found for %s", e.UUID, e.Name)
		}
		data, err := content.Expand(db, fileRid)
		if err != nil {
			return fmt.Errorf("expanding %s: %w", e.Name, err)
		}
		files = append(files, libfossil.FileToCommit{
			Name:    e.Name,
			Content: data,
			Perm:    e.Perm,
		})
	}

	user := c.User
	if user == "" {
		user = currentUser()
	}

	comment := c.Message
	if comment == "" {
		comment = "Create branch " + c.Name
	}

	rid, uuid, err := r.Commit(libfossil.CommitOpts{
		Files:    files,
		Comment:  comment,
		User:     user,
		ParentID: parentRid,
		Time:     time.Now().UTC(),
		Tags: []libfossil.TagSpec{
			{Name: "branch", Value: c.Name},
			{Name: "sym-" + c.Name},
		},
	})
	if err != nil {
		return err
	}

	short := uuid
	if len(short) > 10 {
		short = short[:10]
	}
	fmt.Printf("branch %s created at %s (rid=%d)\n", c.Name, short, rid)
	return nil
}

// RepoBranchCloseCmd closes a branch.
type RepoBranchCloseCmd struct {
	Name string `arg:"" help:"Branch name to close"`
	User string `help:"User for control artifacts (default: OS username)"`
}

func (c *RepoBranchCloseCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	db := r.Inner().DB()
	tagName := "sym-" + c.Name
	var tipRID int64
	err = db.QueryRow(`
		SELECT tx.rid FROM tagxref tx
		JOIN tag t ON t.tagid = tx.tagid
		WHERE t.tagname = ? AND tx.tagtype > 0
		ORDER BY tx.mtime DESC LIMIT 1`, tagName).Scan(&tipRID)
	if err != nil {
		return fmt.Errorf("branch %q not found or already closed", c.Name)
	}

	user := c.User
	if user == "" {
		user = currentUser()
	}

	inner := r.Inner()
	fslID := fsltype.FslID(tipRID)

	if _, err := tag.AddTag(inner, tag.TagOpts{
		TargetRID: fslID,
		TagName:   tagName,
		TagType:   tag.TagCancel,
		User:      user,
	}); err != nil {
		return fmt.Errorf("cancelling branch tag: %w", err)
	}

	if _, err := tag.AddTag(inner, tag.TagOpts{
		TargetRID: fslID,
		TagName:   "closed",
		TagType:   tag.TagSingleton,
		User:      user,
	}); err != nil {
		return fmt.Errorf("adding closed tag: %w", err)
	}

	fmt.Printf("branch %s closed\n", c.Name)
	return nil
}
