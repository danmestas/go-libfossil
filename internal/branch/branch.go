package branch

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
)

type CreateOpts struct {
	Name   string
	Parent libfossil.FslID
	User   string
	Time   time.Time
	Color  string
}

type Branch struct {
	Name         string
	LastMTime    float64
	IsClosed     bool
	CheckinCount int
	LatestUUID   string
}

func Create(r *repo.Repo, opts CreateOpts) (libfossil.FslID, string, error) {
	if r == nil {
		panic("branch.Create: r must not be nil")
	}
	if opts.Name == "" {
		panic("branch.Create: opts.Name must not be empty")
	}
	if opts.Parent <= 0 {
		panic("branch.Create: opts.Parent must be positive")
	}
	if opts.User == "" {
		panic("branch.Create: opts.User must not be empty")
	}

	// 1. Get parent's file list.
	entries, err := manifest.ListFiles(r, opts.Parent)
	if err != nil {
		return 0, "", fmt.Errorf("branch.Create list files: %w", err)
	}
	var files []manifest.File
	for _, e := range entries {
		frid, ok := content.AvailableByUUID(r.DB(), e.UUID)
		if !ok {
			return 0, "", fmt.Errorf("branch.Create: file blob %s not found", e.UUID)
		}
		// Bare Expand: each file of the single parent checkin is expanded
		// exactly once to seed the new branch commit. There is no repeated or
		// overlapping-chain access here for a cache to amortize.
		data, err := content.Expand(r.DB(), frid)
		if err != nil {
			return 0, "", fmt.Errorf("branch.Create: expand %s: %w", e.Name, err)
		}
		files = append(files, manifest.File{Name: e.Name, Content: data, Perm: e.Perm})
	}

	// 2. Query existing sym-* tags on parent for cancel cards.
	rows, err := r.DB().Query(
		"SELECT tag.tagname FROM tagxref JOIN tag USING(tagid) WHERE tagxref.rid=? AND tagxref.tagtype>0 AND tag.tagname GLOB 'sym-*'",
		opts.Parent,
	)
	if err != nil {
		return 0, "", fmt.Errorf("branch.Create query sym tags: %w", err)
	}
	defer rows.Close()
	var oldSyms []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, "", err
		}
		oldSyms = append(oldSyms, name)
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}

	// 3. Build T-cards.
	var tags []deck.TagCard
	tags = append(tags, deck.TagCard{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: opts.Name})
	tags = append(tags, deck.TagCard{Type: deck.TagPropagating, Name: "sym-" + opts.Name, UUID: "*"})
	for _, old := range oldSyms {
		tags = append(tags, deck.TagCard{Type: deck.TagCancel, Name: old, UUID: "*"})
	}
	if opts.Color != "" {
		tags = append(tags, deck.TagCard{Type: deck.TagPropagating, Name: "bgcolor", UUID: "*", Value: opts.Color})
	}

	// 4. Create the checkin with its branch tags. Checkin applies them in its
	// own transaction, and refuses there a name an open branch already
	// carries, as `fossil branch new` does (src/branch.c branch_is_open): the
	// check and the write are one transaction, so of two concurrent Creates of
	// one name only the first commits, and a refused Create stores nothing.
	rid, uuid, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   files,
		Comment: fmt.Sprintf("Create branch %s", opts.Name),
		User:    opts.User,
		Parent:  opts.Parent,
		Time:    opts.Time,
		Tags:    tags,
		Precheck: func(q db.Querier) error {
			open, err := branchIsOpen(q, opts.Name)
			if err != nil {
				return err
			}
			if open {
				return fmt.Errorf("an open branch named %q already exists", opts.Name)
			}
			return nil
		},
	})
	if err != nil {
		return 0, "", fmt.Errorf("branch.Create: %w", err)
	}
	return rid, uuid, nil
}

// branchIsOpen reports whether some open leaf's active branch tag is name:
// a leaf (in the leaf table) whose propagating branch tag (tagtype=2) has the
// value name and that carries no singleton closed tag (tagtype=1). This is
// fossil's branch_is_open (src/branch.c), which uses those exact tag types.
//
// Create runs it on the check-in's own transaction (CheckinOpts.Precheck), so
// the check and the new branch's write see one state.
func branchIsOpen(q db.Querier, name string) (bool, error) {
	var one int
	err := q.QueryRow(`
		SELECT 1 FROM tagxref ox
		JOIN tag t ON t.tagid = ox.tagid
		WHERE t.tagname = 'branch'
		  AND ox.value = ?
		  AND ox.tagtype = 2
		  AND ox.rid IN (SELECT rid FROM leaf)
		  AND NOT EXISTS (
		    SELECT 1 FROM tagxref ix
		    JOIN tag ct ON ct.tagid = ix.tagid
		    WHERE ct.tagname = 'closed'
		      AND ix.rid = ox.rid
		      AND ix.tagtype = 1
		  )
		LIMIT 1`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("branch.branchIsOpen %q: %w", name, err)
	}
	return true, nil
}

func List(r *repo.Repo) ([]Branch, error) {
	if r == nil {
		panic("branch.List: r must not be nil")
	}

	rows, err := r.DB().Query(`
		SELECT
			tagxref.value AS branch_name,
			max(event.mtime) AS last_mtime,
			count(*) AS checkin_count,
			(SELECT EXISTS(SELECT 1 FROM tagxref cx JOIN tag ct ON cx.tagid=ct.tagid
				 WHERE ct.tagname='closed' AND cx.rid=ev.objid AND cx.tagtype>0)
			 FROM tagxref bx JOIN tag bt ON bx.tagid=bt.tagid
			 JOIN event ev ON ev.objid=bx.rid
			 WHERE bt.tagname='branch' AND bx.value=tagxref.value AND bx.tagtype>0
			 ORDER BY ev.mtime DESC LIMIT 1) AS isclosed,
			(SELECT b.uuid FROM blob b
			 JOIN tagxref tx4 ON b.rid=tx4.rid
			 JOIN tag t4 ON tx4.tagid=t4.tagid
			 JOIN event e ON e.objid=tx4.rid
			 WHERE t4.tagname='branch' AND tx4.value=tagxref.value AND tx4.tagtype>0
			 ORDER BY e.mtime DESC LIMIT 1) AS latest
		FROM tagxref JOIN tag ON tagxref.tagid=tag.tagid
		JOIN event ON event.objid=tagxref.rid
		WHERE tag.tagname='branch' AND tagxref.tagtype>0
		GROUP BY tagxref.value
	`)
	if err != nil {
		return nil, fmt.Errorf("branch.List: %w", err)
	}
	defer rows.Close()

	var branches []Branch
	for rows.Next() {
		var b Branch
		var isClosed *int
		if err := rows.Scan(&b.Name, &b.LastMTime, &b.CheckinCount, &isClosed, &b.LatestUUID); err != nil {
			return nil, fmt.Errorf("branch.List scan: %w", err)
		}
		b.IsClosed = isClosed != nil && *isClosed == 1
		branches = append(branches, b)
	}
	return branches, rows.Err()
}

func Close(r *repo.Repo, name string, user string) error {
	if r == nil {
		panic("branch.Close: r must not be nil")
	}
	if name == "" {
		panic("branch.Close: name must not be empty")
	}

	var tipRID int64
	err := r.DB().QueryRow(`
		SELECT tagxref.rid FROM tagxref
		JOIN tag ON tagxref.tagid=tag.tagid
		JOIN event ON event.objid=tagxref.rid
		WHERE tag.tagname='branch' AND tagxref.value=? AND tagxref.tagtype>0
		ORDER BY event.mtime DESC LIMIT 1`,
		name,
	).Scan(&tipRID)
	if err != nil {
		return fmt.Errorf("branch.Close: find tip for %q: %w", name, err)
	}

	_, err = tag.AddTag(r, tag.TagOpts{
		TargetRID: libfossil.FslID(tipRID),
		TagName:   "closed",
		TagType:   tag.TagSingleton,
		User:      user,
	})
	if err != nil {
		return fmt.Errorf("branch.Close: add closed tag: %w", err)
	}
	return nil
}
