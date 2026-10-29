package manifest

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
)

// ErrNoAttachTarget is returned by ResolveAttachTarget when no ticket, wiki
// page or tech note answers to the name.
var ErrNoAttachTarget = errors.New("no ticket, wiki page or tech note by that name")

// AttachmentWrite is one attachment artifact: the file name bound to a
// target, and the file's bytes, or none to delete the binding.
type AttachmentWrite struct {
	// Target is the full ticket id, wiki page name or tech note id.
	Target string
	// Name is the file name as given; only its final path component is kept.
	Name string
	// Content is stored as the attachment's blob. Nil writes a deletion.
	Content []byte
	// Mimetype, when not empty, is written as the N card.
	Mimetype string
	Comment  string
	User     string
	// Time is the D card; zero means now.
	Time time.Time
}

// WriteAttachment stores the artifacts fossil's attach_commit stores for an
// addition (src/attach.c): Content as one ordinary blob, then the control
// artifact whose A card names it, crosslinked in the same transaction and
// queued for sync. A deletion is the control artifact /ainfo writes: an A
// card with no source and no C card. It returns the control artifact's rid
// and hash.
//
// Content that parses as an artifact is compressed and its name gains ".gz",
// as fossil does, so an attached file is never linked as a control artifact.
func WriteAttachment(r *repo.Repo, w AttachmentWrite) (libfossil.FslID, string, error) {
	if r == nil {
		panic("manifest.WriteAttachment: r must not be nil")
	}
	if w.Target == "" || w.User == "" {
		panic("manifest.WriteAttachment: w.Target and w.User must not be empty")
	}
	if w.Content == nil && (w.Comment != "" || w.Mimetype != "") {
		panic("manifest.WriteAttachment: a deletion carries no comment or mimetype")
	}
	when := libfossil.CardTimeNow(time.Now())
	if !w.Time.IsZero() {
		when = libfossil.CardTime(w.Time)
	}
	uid, err := r.LocalUID(w.User)
	if err != nil {
		return 0, "", fmt.Errorf("manifest.WriteAttachment: %w", err)
	}
	var rid libfossil.FslID
	var uuid string
	err = r.WithReceipt(&db.Receipt{UID: uid}).WithTx(func(tx *db.Tx) error {
		d := &deck.Deck{
			Type: deck.Attachment, A: &deck.AttachmentCard{Filename: attachName(w.Name), Target: w.Target},
			C: strings.TrimSpace(w.Comment), D: when, N: w.Mimetype, U: deck.User(w.User),
		}
		stored := []libfossil.FslID{}
		if w.Content != nil {
			srcRID, err := storeAttachedFile(tx, d, w.Content)
			if err != nil {
				return err
			}
			stored = append(stored, srcRID)
		}
		rid, uuid, err = storeAttachmentControl(tx, d)
		if err != nil {
			return err
		}
		_, err = seedcheck.JudgeFilled(tx, append(stored, rid))
		return err
	})
	if err != nil {
		return 0, "", fmt.Errorf("manifest.WriteAttachment: %w", err)
	}
	return rid, uuid, nil
}

// attachName is fossil's: the part after the last '/' or '\', or "unknown"
// when that is empty.
func attachName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "unknown"
	}
	return name
}

// storeAttachedFile stores content as one ordinary blob and sets d's A-card
// source to it, compressing an artifact-shaped file as attach_commit does.
func storeAttachedFile(tx *db.Tx, d *deck.Deck, content []byte) (libfossil.FslID, error) {
	if _, err := deck.Parse(content); err == nil {
		packed, err := blob.Compress(content)
		if err != nil {
			return 0, fmt.Errorf("compress attachment: %w", err)
		}
		content = packed
		d.A.Filename += ".gz"
	}
	rid, uuid, err := blob.Store(tx, content)
	if err != nil {
		return 0, fmt.Errorf("store attachment: %w", err)
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", rid); err != nil {
		return 0, fmt.Errorf("attachment unsent: %w", err)
	}
	d.A.Source = uuid
	return rid, nil
}

// storeAttachmentControl stores d and links it as a sweep would on arrival.
func storeAttachmentControl(tx *db.Tx, d *deck.Deck) (libfossil.FslID, string, error) {
	data, err := d.Marshal()
	if err != nil {
		return 0, "", fmt.Errorf("marshal attachment artifact: %w", err)
	}
	// Link what a reader will parse, not the deck it was built from; a name
	// fossil would refuse fails here, before anything is stored.
	parsed, err := deck.Parse(data)
	if err != nil {
		return 0, "", fmt.Errorf("attachment artifact does not parse: %w", err)
	}
	if parsed.Type != deck.Attachment {
		panic("manifest.storeAttachmentControl: marshalled deck is not an attachment artifact")
	}
	rid, uuid, err := blob.Store(tx, data)
	if err != nil {
		return 0, "", fmt.Errorf("store attachment artifact: %w", err)
	}
	if err := crosslinkAttachment(tx, rid, parsed); err != nil {
		return 0, "", err
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", rid); err != nil {
		return 0, "", fmt.Errorf("attachment control unsent: %w", err)
	}
	return rid, uuid, nil
}

// ResolveAttachTarget returns the full name of what target names: a wiki
// page by its exact name, or a ticket or tech note by its id or a unique
// prefix of it, as fossil's /attachadd resolves its tkt=, page= and
// technote= parameters. Fossil takes the kind from which parameter is set;
// here it is inferred, so a name that two kinds answer to is refused as
// ambiguous rather than guessed at.
func ResolveAttachTarget(q db.Querier, target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("attachment target is empty")
	}
	var found []string
	var one int
	switch err := q.QueryRow("SELECT 1 FROM tag WHERE tagname=?", "wiki-"+target).Scan(&one); {
	case err == nil:
		found = append(found, target)
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("resolve attachment target: %w", err)
	}
	if prefix := strings.ToLower(target); len(prefix) <= 64 && isLowerHex(prefix) {
		for _, kind := range []string{"tkt-", "event-"} {
			ids, err := tagsWithPrefix(q, kind, prefix)
			if err != nil {
				return "", err
			}
			found = append(found, ids...)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("attachment target %q: %w", target, ErrNoAttachTarget)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("attachment target %q is ambiguous", target)
}

// tagsWithPrefix returns up to two ids whose tag is kind followed by an id
// starting with prefix; two is enough to tell unique from ambiguous.
func tagsWithPrefix(q db.Querier, kind, prefix string) ([]string, error) {
	rows, err := q.Query("SELECT substr(tagname, ?) FROM tag WHERE tagname GLOB ? LIMIT 2",
		len(kind)+1, kind+prefix+"*")
	if err != nil {
		return nil, fmt.Errorf("resolve attachment target: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("resolve attachment target: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve attachment target: %w", err)
	}
	return ids, nil
}

// AttachmentRow is one row of the attachment table, with the hash of its
// control artifact and the size of the blob it names (-1 while that blob is
// a phantom, 0 for a deletion).
type AttachmentRow struct {
	Artifact string
	Source   string
	Name     string
	Comment  string
	User     string
	Time     time.Time
	Size     int64
}

const attachmentRowSQL = `SELECT b.uuid, coalesce(a.src, ''), a.filename,
       coalesce(a.comment, ''), coalesce(a.user, ''), CAST(a.mtime AS REAL),
       coalesce((SELECT size FROM blob WHERE uuid=a.src), -1)
  FROM attachment a JOIN blob b ON b.rid=a.attachid`

// CurrentAttachments returns target's current attachments, the newest
// first: the latest artifact for each name, unless that latest artifact
// deletes it. This is the set fossil's attachment_list shows.
func CurrentAttachments(q db.Querier, target string) ([]AttachmentRow, error) {
	return attachmentRows(q, attachmentRowSQL+
		" WHERE a.isLatest AND a.src!='' AND a.target=? ORDER BY a.mtime DESC, a.filename", target)
}

// LatestAttachment returns the latest artifact for name on target, which is
// a deletion when its Source is empty. found is false when there is none.
func LatestAttachment(q db.Querier, target, name string) (row AttachmentRow, found bool, err error) {
	rows, err := attachmentRows(q, attachmentRowSQL+
		" WHERE a.isLatest AND a.target=? AND a.filename=? ORDER BY a.mtime DESC, a.attachid DESC LIMIT 1",
		target, name)
	if err != nil || len(rows) == 0 {
		return AttachmentRow{}, false, err
	}
	return rows[0], true, nil
}

func attachmentRows(q db.Querier, query string, args ...any) ([]AttachmentRow, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("attachments: %w", err)
	}
	defer rows.Close()
	var out []AttachmentRow
	for rows.Next() {
		var a AttachmentRow
		var mtime float64
		if err := rows.Scan(&a.Artifact, &a.Source, &a.Name, &a.Comment, &a.User, &mtime, &a.Size); err != nil {
			return nil, fmt.Errorf("attachments: %w", err)
		}
		if a.Source == "" {
			a.Size = 0
		}
		a.Time = libfossil.JulianToTime(mtime)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attachments: %w", err)
	}
	return out, nil
}
