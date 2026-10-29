package libfossil

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/internal/attachstub"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// ErrAttachmentNotFound is returned when a target has no current attachment
// by the name asked for: none was ever added, or the latest was deleted.
// Callers can match with errors.Is.
var ErrAttachmentNotFound = errors.New("libfossil: attachment not found")

// AttachmentOpts is who makes an attachment change, when, and why.
type AttachmentOpts struct {
	// User is the change's author, its U card.
	User string
	// Comment is the attachment's description, its C card. Fossil writes
	// none on a deletion, so AttachmentDelete refuses one.
	Comment string
	// Time is the change's D card; zero means now.
	Time time.Time
}

// ExternalRef names an attachment's bytes held outside the repository, in a
// bucket the host binds and keys attachments/<SHA256> (Quarry RFC 0016). The
// repository holds only a stub naming them; this library never reads or
// writes the bytes themselves.
type ExternalRef struct {
	// SHA256 is the bytes' SHA-256, 64 lowercase hex digits.
	SHA256 string `json:"sha256"`
	// Size is the bytes' length.
	Size int64 `json:"size"`
	// Type is the declared mimetype.
	Type string `json:"type"`
	// Name is the file's name, a bare name with no directory.
	Name string `json:"name"`
}

// Attachment is one current attachment: the latest artifact for its name on
// its target, when that artifact adds a file rather than deleting it.
type Attachment struct {
	// Artifact is the hash of the control artifact that attached the file.
	Artifact string `json:"artifact"`
	// Target is the full ticket id, wiki page name or tech note id.
	Target string `json:"target"`
	Name   string `json:"name"`
	// Source is the hash of the blob the A card names: the file itself, or
	// the stub when External is set.
	Source  string    `json:"source"`
	Comment string    `json:"comment"`
	User    string    `json:"user"`
	Time    time.Time `json:"time"`
	// Size is the file's length in bytes: the blob's, or External.Size. It
	// is -1 when the blob has not arrived in this repository.
	Size int64 `json:"size"`
	// External, when not nil, says the bytes live outside the repository:
	// Source is a stub, and AttachmentRead returns no content.
	External *ExternalRef `json:"external,omitempty"`
}

// AttachmentAdd attaches content to target under name, as `fossil attachment
// add` does, and returns the hash of the control artifact. content is stored
// as one ordinary blob; only name's final path component is kept. The
// attachment table and timeline reflect it when AttachmentAdd returns, and
// both artifacts are queued to sync.
//
// target is a wiki page's exact name, or a ticket's or tech note's id or a
// unique prefix of it. Fossil is told the kind; here it is inferred, and a
// name more than one kind answers to is refused as ambiguous.
//
// Content that is a well-formed external-attachment stub is refused, since
// it would read back as an external attachment rather than as the bytes
// given: use AttachmentAddExternal. Content that parses as a fossil artifact
// is compressed and its name gains ".gz", as fossil does.
func (r *Repo) AttachmentAdd(target, name string, content []byte, opts AttachmentOpts) (artifact string, err error) {
	if content == nil {
		content = []byte{}
	}
	if _, isStub := attachstub.Parse(content); isStub {
		return "", fmt.Errorf("libfossil: attachment add: content is an external-attachment stub; use AttachmentAddExternal")
	}
	return r.attachmentAdd(target, manifest.AttachmentWrite{Name: name, Content: content}, opts)
}

// AttachmentAddExternal attaches the file ref names without its bytes, and
// returns the hash of the control artifact. The blob stored is the stub
// Quarry writes, and the control artifact's N card carries ref.Type, so
// stock fossil shows a small text file under ref.Name. The host stores the
// bytes; this records only where they are found. target resolves as in
// AttachmentAdd.
func (r *Repo) AttachmentAddExternal(target string, ref ExternalRef, opts AttachmentOpts) (artifact string, err error) {
	if strings.ContainsAny(ref.Name, `/\`) {
		return "", fmt.Errorf("libfossil: attachment add: external name %q holds a directory", ref.Name)
	}
	stub, err := attachstub.Format(attachstub.Ref(ref))
	if err != nil {
		return "", fmt.Errorf("libfossil: attachment add: %w", err)
	}
	return r.attachmentAdd(target, manifest.AttachmentWrite{
		Name: ref.Name, Content: stub, Mimetype: ref.Type,
	}, opts)
}

func (r *Repo) attachmentAdd(target string, w manifest.AttachmentWrite, opts AttachmentOpts) (string, error) {
	if strings.TrimSpace(opts.User) == "" {
		return "", fmt.Errorf("libfossil: attachment add: an attachment needs a user")
	}
	full, err := manifest.ResolveAttachTarget(r.inner.DB(), target)
	if err != nil {
		return "", fmt.Errorf("libfossil: attachment add: %w", err)
	}
	w.Target, w.User, w.Comment, w.Time = full, opts.User, opts.Comment, opts.Time
	_, artifact, err := manifest.WriteAttachment(r.inner, w)
	if err != nil {
		return "", fmt.Errorf("libfossil: attachment add: %w", err)
	}
	return artifact, nil
}

// AttachmentDelete removes the current attachment name from target, as
// fossil's attachment page does: a new control artifact whose A card names
// no file. The bytes stay in history. It returns the artifact's hash, and
// ErrAttachmentNotFound when target has no current attachment by that name.
// opts.Time, when set, must be later than the attachment's.
func (r *Repo) AttachmentDelete(target, name string, opts AttachmentOpts) (artifact string, err error) {
	if strings.TrimSpace(opts.User) == "" {
		return "", fmt.Errorf("libfossil: attachment delete: a deletion needs a user")
	}
	if opts.Comment != "" {
		return "", fmt.Errorf("libfossil: attachment delete: fossil writes no comment on a deletion")
	}
	cur, err := r.currentAttachment(target, name)
	if err != nil {
		return "", fmt.Errorf("libfossil: attachment delete: %w", err)
	}
	// The latest artifact per name wins by time; one no later than the
	// attachment would leave it current, or tie and leave both current.
	if !opts.Time.IsZero() && !opts.Time.After(cur.Time) {
		return "", fmt.Errorf("libfossil: attachment delete: time %s is not after the attachment's %s",
			opts.Time.UTC().Format(time.RFC3339Nano), cur.Time.UTC().Format(time.RFC3339Nano))
	}
	_, artifact, err = manifest.WriteAttachment(r.inner, manifest.AttachmentWrite{
		Target: cur.Target, Name: cur.Name, User: opts.User, Time: opts.Time,
	})
	if err != nil {
		return "", fmt.Errorf("libfossil: attachment delete: %w", err)
	}
	return artifact, nil
}

// Attachments returns target's current attachments, the most recently
// attached first: one per name, leaving out deleted ones, as fossil lists
// them on the target's page. target resolves as in AttachmentAdd.
func (r *Repo) Attachments(target string) ([]Attachment, error) {
	full, err := r.attachTarget(target)
	if err != nil {
		return nil, fmt.Errorf("libfossil: attachments: %w", err)
	}
	rows, err := manifest.CurrentAttachments(r.inner.DB(), full)
	if err != nil {
		return nil, fmt.Errorf("libfossil: attachments: %w", err)
	}
	out := make([]Attachment, len(rows))
	for i, row := range rows {
		out[i] = r.attachment(full, row)
	}
	return out, nil
}

// AttachmentRead returns target's current attachment name and its bytes.
// For an external attachment the bytes are not here: content is nil and
// a.External says where the host keeps them. It returns
// ErrAttachmentNotFound when there is no such current attachment.
func (r *Repo) AttachmentRead(target, name string) (a Attachment, content []byte, err error) {
	a, err = r.currentAttachment(target, name)
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("libfossil: attachment read: %w", err)
	}
	if a.External != nil {
		return a, nil, nil
	}
	content, err = r.ReadBlob(a.Source)
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("libfossil: attachment read %s: %w", a.Name, err)
	}
	return a, content, nil
}

func (r *Repo) currentAttachment(target, name string) (Attachment, error) {
	full, err := r.attachTarget(target)
	if err != nil {
		return Attachment{}, err
	}
	row, found, err := manifest.LatestAttachment(r.inner.DB(), full, name)
	if err != nil {
		return Attachment{}, err
	}
	if !found || row.Source == "" {
		return Attachment{}, fmt.Errorf("%q on %s: %w", name, full, ErrAttachmentNotFound)
	}
	return r.attachment(full, row), nil
}

// attachTarget resolves target for a read or a deletion. An attachment can
// arrive before what it is attached to, or outlive it here, so a name the
// attachment table holds verbatim stands even when nothing else answers.
func (r *Repo) attachTarget(target string) (string, error) {
	full, err := manifest.ResolveAttachTarget(r.inner.DB(), target)
	if !errors.Is(err, manifest.ErrNoAttachTarget) {
		return full, err
	}
	var one int
	if r.inner.DB().QueryRow("SELECT 1 FROM attachment WHERE target=? LIMIT 1", target).Scan(&one) == nil {
		return target, nil
	}
	return "", err
}

// attachment converts row, reading its blob to tell a stub from a file. A
// stub is at most attachstub.MaxBytes, so a larger blob is never read here.
func (r *Repo) attachment(target string, row manifest.AttachmentRow) Attachment {
	a := Attachment{
		Artifact: row.Artifact, Target: target, Name: row.Name, Source: row.Source,
		Comment: row.Comment, User: row.User, Time: row.Time, Size: row.Size,
	}
	if row.Size < 0 || row.Size > attachstub.MaxBytes {
		return a
	}
	// A blob held but unreadable here (below the horizon, say) cannot be
	// told from a file; it lists as one, and AttachmentRead reports the
	// read's own error.
	raw, err := r.ReadBlob(row.Source)
	if err != nil {
		return a
	}
	// A blob that opens like a stub and does not parse is a file: Quarry's
	// index folds it so, and its bytes are then all there is to give.
	if ref, ok := attachstub.Parse(raw); ok {
		ext := ExternalRef(ref)
		a.External, a.Size = &ext, ref.Size
	}
	return a
}
