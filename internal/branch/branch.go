package branch

import (
	"fmt"
	"time"

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
	if opts.Time.IsZero() {
		opts.Time = time.Now().UTC()
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

	// 4. Create checkin with branch tags.
	rid, uuid, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   files,
		Comment: fmt.Sprintf("Create branch %s", opts.Name),
		User:    opts.User,
		Parent:  opts.Parent,
		Time:    opts.Time,
		Tags:    tags,
	})
	if err != nil {
		return 0, "", fmt.Errorf("branch.Create checkin: %w", err)
	}

	// 5. Process inline T-cards to apply tags.
	// Checkin already inserted event/plink/mlink, so we just need to process tags.
	if err := processInlineTags(r, rid, tags, opts.Time); err != nil {
		return 0, "", fmt.Errorf("branch.Create process tags: %w", err)
	}

	return rid, uuid, nil
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
			(SELECT 1 FROM tagxref tx2 JOIN tag t2 ON tx2.tagid=t2.tagid
			 WHERE t2.tagname='closed' AND tx2.rid IN
				(SELECT rid FROM tagxref tx3 JOIN tag t3 ON tx3.tagid=t3.tagid
				 WHERE t3.tagname='branch' AND tx3.value=tagxref.value AND tx3.tagtype>0)
			 AND tx2.tagtype>0
			 LIMIT 1) AS isclosed,
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

func processInlineTags(r *repo.Repo, rid libfossil.FslID, tags []deck.TagCard, mtime time.Time) error {
	julianMtime := libfossil.TimeToJulian(mtime)
	for _, tc := range tags {
		if tc.UUID != "*" {
			continue // only process inline tags (UUID="*" means this checkin)
		}
		var tagType int
		switch tc.Type {
		case deck.TagPropagating:
			tagType = tag.TagPropagating
		case deck.TagSingleton:
			tagType = tag.TagSingleton
		case deck.TagCancel:
			tagType = tag.TagCancel
		default:
			continue
		}

		if err := tag.ApplyTag(r, tag.ApplyOpts{
			TargetRID: rid,
			SrcRID:    rid, // inline: checkin is its own source
			TagName:   tc.Name,
			TagType:   tagType,
			Value:     tc.Value,
			MTime:     julianMtime,
		}); err != nil {
			return fmt.Errorf("apply tag %q: %w", tc.Name, err)
		}
	}
	return nil
}
