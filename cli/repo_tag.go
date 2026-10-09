package cli

import (
	"database/sql"
	"fmt"
)

// RepoTagCmd groups tag operations.
type RepoTagCmd struct {
	Ls  RepoTagLsCmd  `cmd:"" help:"List tags on an artifact"`
	Add RepoTagAddCmd `cmd:"" help:"Add a tag to an artifact"`
}

// RepoTagLsCmd lists tags on an artifact.
type RepoTagLsCmd struct {
	Version string `arg:"" optional:"" help:"Version to list tags for (default: tip)"`
}

func (c *RepoTagLsCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	rid, err := resolveRID(r, c.Version)
	if err != nil {
		return err
	}

	db := r.Inner().DB()
	rows, err := db.Query(`
		SELECT t.tagname, tx.value,
		       CASE tx.tagtype WHEN 1 THEN '+' WHEN 2 THEN '*' ELSE '-' END as type
		FROM tagxref tx
		JOIN tag t ON t.tagid=tx.tagid
		WHERE tx.rid=? AND tx.tagtype>0
		ORDER BY t.tagname`, rid)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var name, tagtype string
		var value sql.NullString // NULL for a tag without a value, as fossil stores it
		if err := rows.Scan(&name, &value, &tagtype); err != nil {
			return err
		}
		if value.String != "" {
			fmt.Printf("%s%s=%s\n", tagtype, name, value.String)
		} else {
			fmt.Printf("%s%s\n", tagtype, name)
		}
	}
	return rows.Err()
}

// RepoTagAddCmd adds a tag to an artifact.
type RepoTagAddCmd struct {
	Tag     string `arg:"" help:"Tag name"`
	Value   string `arg:"" optional:"" help:"Tag value (optional)"`
	Version string `help:"Version to tag (default: tip)"`
}

func (c *RepoTagAddCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	rid, err := resolveRID(r, c.Version)
	if err != nil {
		return err
	}

	db := r.Inner().DB()

	// Get or create the tag ID.
	tagName := "sym-" + c.Tag
	var tagID int64
	err = db.QueryRow("SELECT tagid FROM tag WHERE tagname=?", tagName).Scan(&tagID)
	if err != nil {
		result, err := db.Exec("INSERT INTO tag(tagname) VALUES(?)", tagName)
		if err != nil {
			return fmt.Errorf("creating tag: %w", err)
		}
		tagID, _ = result.LastInsertId()
	}

	// Insert tagxref: tagtype 1 = singleton (+), 2 = propagating (*).
	_, err = db.Exec(`
		INSERT OR REPLACE INTO tagxref(tagid, tagtype, srcid, rid, mtime, value)
		VALUES(?, 1, 0, ?, julianday('now'), ?)`,
		tagID, rid, c.Value)
	if err != nil {
		return fmt.Errorf("applying tag: %w", err)
	}

	var uuid string
	db.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid)
	if len(uuid) > 10 {
		uuid = uuid[:10]
	}
	fmt.Printf("tagged %s with %s\n", uuid, c.Tag)
	return nil
}
