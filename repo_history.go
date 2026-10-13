package libfossil

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/annotate"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/diff"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// ErrFileNotFound is returned by ReadFile when the requested filePath is
// not tracked in the given checkin. Callers can match with errors.Is.
var ErrFileNotFound = errors.New("libfossil: file not found in checkin")

// LogOpts configures an Ancestry query: a first-parent walk starting from
// a specific check-in.
type LogOpts struct {
	Start int64
	Limit int
}

// TimelineOpts configures a Timeline query: a repository-wide enumeration
// of the event table.
type TimelineOpts struct {
	// Type restricts the enumeration to a single event kind. The zero
	// value means "all kinds" — the canonical `fossil timeline` default.
	Type EventKind
	// After, when valid, resumes enumeration immediately following this
	// cursor — the next page after whatever TimelineEntry produced it. The
	// zero Cursor means "start from the newest event". See Repo.Timeline's
	// doc comment for the pagination contract this forms, and Cursor's doc
	// comment for why it must come from a TimelineEntry rather than be
	// built by hand.
	After Cursor
	// Limit caps the number of entries returned. Zero or negative means
	// unbounded.
	Limit int
}

// LogEntry represents a single event, as returned by Ancestry. Parents is
// populated for Kind == EventKindCheckin and empty for every other kind —
// plink, which Parents is derived from, only relates check-in artifacts,
// so that is correct, not a gap.
//
// LogEntry carries no pagination cursor: Ancestry paginates by
// LogOpts.Start/Limit, not by cursor, so there would be nothing valid to
// put in one. See TimelineEntry, Timeline's result type, for the
// cursor-carrying counterpart — this split means an Ancestry entry's
// (necessarily invalid) cursor can no longer be obtained at all, let alone
// passed to Timeline by mistake.
type LogEntry struct {
	RID     int64
	UUID    string
	Comment string
	User    string
	Time    time.Time
	Kind    EventKind
	Parents []string
}

// TimelineEntry represents a single event as returned by Timeline. It
// extends LogEntry with Cursor, the pagination token for this row: pass it
// back as the next call's TimelineOpts.After to resume enumeration
// immediately after this entry. Cursor is always valid
// (Cursor.Valid() == true) on every TimelineEntry Timeline produces.
//
// TimelineEntry values should come from Timeline, not be hand-assembled.
// A TimelineEntry built by embedding a LogEntry from Ancestry carries a
// zero-value Cursor, which Timeline treats as "start from newest" rather
// than as an error.
type TimelineEntry struct {
	LogEntry
	Cursor Cursor
}

// DiffEntry describes a unified diff for a single file.
type DiffEntry struct {
	Name    string
	Unified string
}

// AnnotatedLine is a single line of blame/annotate output.
type AnnotatedLine struct {
	Text string
	UUID string
	User string
	Date time.Time
}

// AnnotateOpts configures an annotate operation.
type AnnotateOpts struct {
	FilePath string
	StartRID int64
}

// BisectSession holds state for a binary-search bisect operation.
type BisectSession struct {
	inner interface{}
}

// Annotate attributes each line of a file to the commit that last changed it.
func (r *Repo) Annotate(opts AnnotateOpts) ([]AnnotatedLine, error) {
	lines, err := annotate.Annotate(r.inner, annotate.Options{
		FilePath: opts.FilePath,
		StartRID: fsltype.FslID(opts.StartRID),
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: annotate: %w", err)
	}
	result := make([]AnnotatedLine, len(lines))
	for i, l := range lines {
		result[i] = AnnotatedLine{
			Text: l.Text,
			UUID: l.Version.UUID,
			User: l.Version.User,
			Date: l.Version.Date,
		}
	}
	return result, nil
}

// Diff returns the unified diff(s) between ridA and ridB.
//
// When filePath is non-empty, returns 0 or 1 entries for that single file:
// the file is treated as empty bytes on any side where it is absent, so
// additions and deletions render as pure insert/delete hunks. An empty
// slice is returned when both sides are byte-identical.
//
// When filePath is empty, returns one entry per file that changed between
// the two checkins (the union of files across both sides where the content
// UUID differs or the file exists on only one side). Entries are sorted by
// Name for deterministic ordering. An empty slice is returned when the two
// checkins have identical file sets and content.
//
// Whole-checkin enumeration is currently name-keyed: a rename with no
// content change surfaces as a delete of the old name plus an add of the
// new name, and permission-only changes are not reflected. Proper rename
// and perm-change detection (via the underlying mlink table) is tracked as
// a follow-up; per-file diff behaviour for an explicitly named file is
// unchanged.
func (r *Repo) Diff(ridA, ridB int64, filePath string) ([]DiffEntry, error) {
	if filePath == "" {
		return r.diffWholeCheckin(ridA, ridB)
	}
	return r.diffSingleFile(ridA, ridB, filePath)
}

// diffSingleFile produces the diff for a single named path between two
// checkins. Extracted from Diff so the whole-checkin path can reuse the
// same per-file rendering for each changed file in the union set.
func (r *Repo) diffSingleFile(ridA, ridB int64, filePath string) ([]DiffEntry, error) {
	a, err := blobAt(r, ridA, filePath)
	if err != nil {
		return nil, fmt.Errorf("libfossil: diff: checkin %d: %w", ridA, err)
	}
	b, err := blobAt(r, ridB, filePath)
	if err != nil {
		return nil, fmt.Errorf("libfossil: diff: checkin %d: %w", ridB, err)
	}
	unified := diff.Unified(a, b, diff.Options{
		ContextLines: 3,
		SrcName:      "a/" + filePath,
		DstName:      "b/" + filePath,
	})
	if unified == "" {
		return []DiffEntry{}, nil
	}
	return []DiffEntry{{Name: filePath, Unified: unified}}, nil
}

// diffWholeCheckin enumerates the union of files across the two checkins
// and emits one DiffEntry per file whose content UUID differs or that
// exists on only one side. Results are sorted by Name.
func (r *Repo) diffWholeCheckin(ridA, ridB int64) ([]DiffEntry, error) {
	filesA, err := manifest.ListFiles(r.inner, fsltype.FslID(ridA))
	if err != nil {
		return nil, fmt.Errorf("libfossil: diff: checkins %d..%d: list files at %d: %w", ridA, ridB, ridA, err)
	}
	filesB, err := manifest.ListFiles(r.inner, fsltype.FslID(ridB))
	if err != nil {
		return nil, fmt.Errorf("libfossil: diff: checkins %d..%d: list files at %d: %w", ridA, ridB, ridB, err)
	}

	uuidA := make(map[string]string, len(filesA))
	for _, f := range filesA {
		uuidA[f.Name] = f.UUID
	}
	uuidB := make(map[string]string, len(filesB))
	for _, f := range filesB {
		uuidB[f.Name] = f.UUID
	}

	changed := make(map[string]struct{}, len(uuidA)+len(uuidB))
	for name, ua := range uuidA {
		if uuidB[name] != ua {
			changed[name] = struct{}{}
		}
	}
	for name, ub := range uuidB {
		if uuidA[name] != ub {
			changed[name] = struct{}{}
		}
	}

	if len(changed) == 0 {
		return []DiffEntry{}, nil
	}

	names := make([]string, 0, len(changed))
	for name := range changed {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]DiffEntry, 0, len(names))
	for _, name := range names {
		entries, err := r.diffSingleFile(ridA, ridB, name)
		if err != nil {
			return nil, err
		}
		// diffSingleFile returns an empty slice when both sides are byte-
		// identical. Identical bytes under a changed UUID are not expected
		// (UUID is content-addressed), but skip the entry rather than emit
		// an empty diff if it does happen.
		out = append(out, entries...)
	}
	return out, nil
}

// ReadFile returns the bytes of filePath as they existed in checkin rid.
// Returns ErrFileNotFound (wrapped) if the file is not tracked in that
// checkin. A file that exists but is empty returns ([]byte{}, nil).
func (r *Repo) ReadFile(rid int64, filePath string) ([]byte, error) {
	if filePath == "" {
		return nil, fmt.Errorf("libfossil: read file: filePath is required")
	}
	data, err := blobAt(r, rid, filePath)
	if err != nil {
		return nil, fmt.Errorf("libfossil: read file: checkin %d: %w", rid, err)
	}
	if data == nil {
		return nil, fmt.Errorf("libfossil: read file: %q in checkin %d: %w", filePath, rid, ErrFileNotFound)
	}
	return data, nil
}

// ResolveVersion resolves a symbolic version name to a repository artifact RID.
//
// It follows fossil's symbolic_name_to_rid (src/name.c) for the forms
// go-libfossil supports, in fossil's order:
//
//  1. "tip" — newest check-in by event.mtime. "" is treated as "tip".
//  2. "rid:N" — the literal RID N, when that artifact exists.
//  3. "tag:NAME" — the check-in carrying sym-NAME with the greatest
//     event.mtime (forces a tag lookup even for a hex NAME).
//  4. "root:BR" / "start:BR" / "merge-in:BR" — the origin, first check-in, or
//     most recent merge-in of branch BR (BR itself resolved recursively).
//  5. "NAME:TIMESTAMP" — the check-in carrying sym-NAME with the greatest
//     event.mtime at or before TIMESTAMP (UTC).
//  6. Artifact hash or 4–64 char hex prefix — unique match against blob.uuid,
//     tried before a plain tag; more than one match is ambiguous.
//  7. Plain symbolic name — the check-in carrying sym-NAME with the greatest
//     event.mtime (resolves "trunk" and branch names).
//  8. Trailing-punctuation retry — a name pasted from prose that ends in one
//     or two punctuation characters is retried without them.
//
// No match returns ErrVersionNotFound (wrapped); a hex prefix matching more
// than one artifact returns ErrAmbiguousVersion (wrapped).
//
// The names relative to an open checkout ("current", "prev", "next") are
// ordinary names here, as in fossil outside a checkout; Checkout's
// ResolveVersion resolves them.
func (r *Repo) ResolveVersion(name string) (int64, error) {
	return resolveVersion(r.inner.DB(), name, 0, false)
}

// ResolveBranchStart resolves name as ResolveVersion does, then returns the
// first check-in of the branch the result is on: fossil's branch-type ("br")
// request (repo-state-01 Section 8.3), the start of the branch rather than
// the check-in its name most recently points at. As Section 8.3 requires, the
// request is rewritten to a check-in request first: only check-ins can match,
// so a technote tagged with the branch name does not shadow the branch.
//
// Section 8.3 walks back from the result of every form. fossil(1) walks only
// from a "tag:NAME" or plain-name result (symbolic_name_to_rid, src/name.c),
// so there a hash, "tip" or a date names the check-in itself; this follows
// the draft.
func (r *Repo) ResolveBranchStart(name string) (int64, error) {
	h := r.inner.DB()
	rid, err := resolveVersion(h, name, 0, true)
	if err != nil {
		return 0, err
	}
	first, err := startOfBranch(h, rid, branchFirst)
	if err != nil {
		return 0, fmt.Errorf("libfossil: resolve branch start %q: %w", name, err)
	}
	return first, nil
}

// resolveVersion is ResolveVersion with the open checkout's version, ckout,
// or 0 when no checkout is open. ciOnly restricts every lookup to check-ins,
// as fossil's "ci" request type does (src/name.c).
func resolveVersion(h *db.DB, name string, ckout int64, ciOnly bool) (int64, error) {
	if h == nil {
		panic("libfossil: resolveVersion: nil db")
	}
	if ckout < 0 {
		panic("libfossil: resolveVersion: negative checkout rid")
	}
	// go-libfossil's contract treats "" as "tip"; fossil treats an empty
	// name as unresolvable.
	if name == "" {
		name = "tip"
	}
	rid, err := resolver{h: h, ckout: ckout, ciOnly: ciOnly}.resolve(name)
	if err != nil {
		return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, err)
	}
	if rid <= 0 {
		return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, ErrVersionNotFound)
	}
	return rid, nil
}

// start_of_branch modes, fossil's eType argument (src/name.c).
const (
	branchOrigin  = 0 // root:   the check-in the branch sprouted from
	branchFirst   = 1 // start:  the first check-in on the branch
	branchMergeIn = 2 // merge-in: the most recent merge-in for the branch
)

// resolver is fossil's symbolic_name_to_rid for one request. ckout is the
// open checkout's version, 0 when none is open; it is carried into the
// recursive forms, as fossil's global checkout is.
type resolver struct {
	h     *db.DB
	ckout int64
	// ciOnly is fossil's "ci" request type: only check-ins can match, so a
	// technote or wiki page carrying the same sym- tag is never the answer.
	ciOnly bool
}

// resolve is ResolveVersion's engine, as an ordered sequence of attempts.
// It returns 0 when nothing matches and ErrAmbiguousVersion when a hex
// prefix matches more than one artifact.
func (rv resolver) resolve(name string) (int64, error) {
	h := rv.h

	if name == "tip" {
		// fossil's "tip" is the newest check-in, but when there is none it
		// falls through to the ordinary forms below rather than stopping, so
		// a literal tag named "tip" could still match. (No sym-tip tag can
		// exist without a check-in, so this only ever resolves to not-found.)
		if rid, err := newestCheckin(h); err != nil || rid != 0 {
			return rid, err
		}
	}
	if rid, err := rv.checkoutRelative(name); err != nil || rid != 0 {
		return rid, err
	}
	if rest, ok := strings.CutPrefix(name, "rid:"); ok {
		return ridLiteral(h, rest, rv.ciOnly)
	}
	if rest, ok := strings.CutPrefix(name, "tag:"); ok {
		return newestWithSymTag(h, rest, rv.ciOnly)
	}
	if rest, ok := strings.CutPrefix(name, "root:"); ok {
		return rv.startOfBranchBy(rest, branchOrigin)
	}
	if rest, ok := strings.CutPrefix(name, "start:"); ok {
		return rv.startOfBranchBy(rest, branchFirst)
	}
	if rest, ok := strings.CutPrefix(name, "merge-in:"); ok {
		return rv.startOfBranchBy(rest, branchMergeIn)
	}
	if base, julian, ok := splitNameTimestamp(name); ok {
		return newestSymTagBefore(h, base, julian, rv.ciOnly)
	}
	// A hash prefix is tried before a plain tag, as fossil does; a hex string
	// that matches no artifact falls through to the tag lookup.
	if rid, matched, err := hashPrefix(h, name, rv.ciOnly); err != nil || matched {
		return rid, err
	}
	if rid, err := newestWithSymTag(h, name, rv.ciOnly); err != nil || rid != 0 {
		return rid, err
	}
	if trimmed, ok := trimTrailingPunct(name); ok {
		return rv.resolve(trimmed)
	}
	return 0, nil
}

// checkoutRelative resolves fossil's checkout keywords (src/name.c):
// "current", "prev"/"previous" and "next". It returns 0 when no checkout is
// open, when name is no keyword, or when the keyword names nothing (prev at
// a root, next at a leaf), so the name falls through to the other forms.
func (rv resolver) checkoutRelative(name string) (int64, error) {
	if rv.ckout == 0 {
		return 0, nil
	}
	var query string
	switch name {
	case "current":
		return rv.ckout, nil
	case "prev", "previous":
		query = "SELECT pid FROM plink WHERE cid = ? AND isprim"
	case "next":
		// fossil's order: a primary child first, then the newest link.
		query = "SELECT cid FROM plink WHERE pid = ? ORDER BY isprim DESC, mtime DESC"
	default:
		return 0, nil
	}
	var rid int64
	err := rv.h.QueryRow(query+" LIMIT 1", rv.ckout).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rid, nil
}

// eventIsCheckin and blobIsCheckin are the SQL conditions of a check-in-only
// request, as appended clauses; they are empty for an untyped one.
func eventIsCheckin(ciOnly bool) string {
	if !ciOnly {
		return ""
	}
	return " AND event.type = 'ci'"
}

func blobIsCheckin(ciOnly bool) string {
	if !ciOnly {
		return ""
	}
	return " AND EXISTS(SELECT 1 FROM event WHERE event.objid = blob.rid AND event.type = 'ci')"
}

// newestCheckin is fossil's "tip": the newest check-in by event.mtime.
func newestCheckin(h *db.DB) (int64, error) {
	var rid int64
	err := h.QueryRow(
		"SELECT objid FROM event WHERE type='ci' ORDER BY mtime DESC LIMIT 1",
	).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rid, nil
}

// ridLiteral resolves "rid:N" to RID N when that artifact exists.
func ridLiteral(h *db.DB, digits string, ciOnly bool) (int64, error) {
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 {
		return 0, nil
	}
	var rid int64
	err = h.QueryRow("SELECT rid FROM blob WHERE rid = ?"+blobIsCheckin(ciOnly), n).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rid, nil
}

// newestWithSymTag is fossil's most_recent_event_with_tag: the check-in
// carrying sym-<tag> with the greatest event.mtime, 0 when none does.
func newestWithSymTag(h *db.DB, tag string, ciOnly bool) (int64, error) {
	var rid int64
	err := h.QueryRow(`
		SELECT event.objid FROM tag
		JOIN tagxref ON tagxref.tagid = tag.tagid
		JOIN event ON event.objid = tagxref.rid
		WHERE tag.tagname = ?
		  AND tagxref.tagtype > 0`+eventIsCheckin(ciOnly)+`
		ORDER BY event.mtime DESC LIMIT 1`,
		"sym-"+tag).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rid, nil
}

// newestSymTagBefore is the "NAME:TIMESTAMP" form: the check-in carrying
// sym-<base> with the greatest event.mtime at or before julian.
func newestSymTagBefore(h *db.DB, base string, julian float64, ciOnly bool) (int64, error) {
	var rid int64
	err := h.QueryRow(`
		SELECT event.objid FROM tag
		JOIN tagxref ON tagxref.tagid = tag.tagid
		JOIN event ON event.objid = tagxref.rid
		WHERE tag.tagname = ?
		  AND tagxref.tagtype > 0
		  AND event.mtime <= ?`+eventIsCheckin(ciOnly)+`
		ORDER BY event.mtime DESC LIMIT 1`,
		"sym-"+base, julian).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rid, nil
}

// startOfBranchBy resolves branch name BR recursively, then returns the point
// in BR that mode selects (fossil's root:/start:/merge-in:).
func (rv resolver) startOfBranchBy(name string, mode int) (int64, error) {
	rid, err := rv.resolve(name)
	if err != nil || rid <= 0 {
		return rid, err
	}
	return startOfBranch(rv.h, rid, mode)
}

// startOfBranch is fossil's start_of_branch (src/name.c): walk rid's primary-
// parent chain while it stays on rid's branch. mode branchOrigin returns the
// off-branch parent the branch sprang from; branchFirst the deepest on-branch
// ancestor (the branch's first check-in); branchMergeIn the youngest ancestor,
// by any parent, that is on the branch the origin's parent is on.
func startOfBranch(h *db.DB, rid int64, mode int) (int64, error) {
	br, err := branchOfRID(h, rid)
	if err != nil {
		return 0, err
	}
	ans := rid
	var pid int64
	err = h.QueryRow(`
		WITH RECURSIVE par(pid, ex, cnt) AS (
		  SELECT pid,
		    EXISTS(SELECT 1 FROM tagxref WHERE tagid=(SELECT tagid FROM tag WHERE tagname='branch')
		            AND tagtype>0 AND value=? AND rid=plink.pid),
		    1
		  FROM plink WHERE cid=? AND isprim=1
		  UNION ALL
		  SELECT plink.pid,
		    EXISTS(SELECT 1 FROM tagxref WHERE tagid=(SELECT tagid FROM tag WHERE tagname='branch')
		            AND tagtype>0 AND value=? AND rid=plink.pid),
		    1+par.cnt
		  FROM plink, par
		  WHERE cid=par.pid AND isprim=1 AND par.ex
		  LIMIT 100000
		)
		SELECT pid FROM par WHERE ex>=? ORDER BY cnt DESC LIMIT 1`,
		br, rid, br, mode%2).Scan(&pid)
	switch {
	case err == nil:
		ans = pid
	case errors.Is(err, sql.ErrNoRows):
		// No primary parent on rid's branch: ans stays rid.
	default:
		return 0, err
	}
	if mode == branchMergeIn && ans > 0 {
		br2, err := branchOfRID(h, ans)
		if err != nil {
			return 0, err
		}
		return youngestAncestorInBranch(h, rid, br2)
	}
	return ans, nil
}

// branchOfRID is fossil's branch_of_rid: rid's active branch, the default
// branch (the main-branch setting) when it carries no active branch tag.
func branchOfRID(h *db.DB, rid int64) (string, error) {
	var br string
	err := h.QueryRow(`
		SELECT value FROM tagxref
		WHERE rid = ?
		  AND tagid = (SELECT tagid FROM tag WHERE tagname='branch')
		  AND tagtype > 0`, rid).Scan(&br)
	if errors.Is(err, sql.ErrNoRows) {
		return repo.MainBranch(h)
	}
	if err != nil {
		return "", err
	}
	return br, nil
}

// youngestAncestorInBranch is fossil's compute_youngest_ancestor_in_branch
// (src/descendants.c): the youngest ancestor of rid, by any parent edge, that
// is on branch.
func youngestAncestorInBranch(h *db.DB, rid int64, branch string) (int64, error) {
	var ans int64
	err := h.QueryRow(`
		WITH RECURSIVE ancestor(rid, mtime) AS (
		  SELECT ?, mtime FROM event WHERE objid=?
		  UNION
		  SELECT plink.pid, event.mtime
		    FROM ancestor, plink, event
		   WHERE plink.cid=ancestor.rid AND event.objid=plink.pid
		   ORDER BY mtime DESC
		)
		SELECT ancestor.rid FROM ancestor
		 WHERE EXISTS(SELECT 1 FROM tagxref
		              WHERE tagid=(SELECT tagid FROM tag WHERE tagname='branch')
		                AND tagxref.rid=ancestor.rid AND value=? AND tagtype>0)
		 ORDER BY mtime DESC LIMIT 1`, rid, rid, branch).Scan(&ans)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return ans, nil
}

// hashPrefix matches name as an artifact hash or 4–64 character hex prefix
// against blob.uuid, after stripping optional surrounding brackets. matched
// reports whether name was a hex form that found one or more artifacts: one
// match returns its rid; more than one returns ErrAmbiguousVersion. A non-hex
// name, or a hex prefix that matches nothing, returns matched=false so the
// caller falls through to a tag lookup.
func hashPrefix(h *db.DB, name string, ciOnly bool) (rid int64, matched bool, err error) {
	s := name
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if len(s) < 4 || len(s) > 64 || !isHex(s) {
		return 0, false, nil
	}
	rows, err := h.Query("SELECT rid FROM blob WHERE uuid GLOB ?"+blobIsCheckin(ciOnly),
		strings.ToLower(s)+"*")
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, false, err
		}
		ids = append(ids, id)
		if len(ids) > 1 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	switch len(ids) {
	case 0:
		return 0, false, nil
	case 1:
		return ids[0], true, nil
	default:
		return 0, true, ErrAmbiguousVersion
	}
}

// splitNameTimestamp detects fossil's "NAME:TIMESTAMP" form: the first colon
// whose suffix is at least seven characters (fossil's i<nTag-8 gate) and
// parses as a date-time. It returns the base name and the inclusive upper
// bound of the timestamp as a text Julian day matching event.mtime.
func splitNameTimestamp(name string) (base string, julian float64, ok bool) {
	i := strings.IndexByte(name, ':')
	if i < 0 || len(name)-i-1 < 7 {
		return "", 0, false
	}
	t, ok := parseFossilTimestamp(name[i+1:])
	if !ok {
		return "", 0, false
	}
	return name[:i], fsltype.TimeToTextJulian(t), true
}

// parseFossilTimestamp parses the date-time forms fossil accepts and rounds
// the result UP to the last millisecond of the given precision, as fossil's
// fossil_roundup_date and fossil_expand_datetime do (src/name.c): a bare date
// becomes 23:59:59.999, HH:MM becomes :59.999, HH:MM:SS becomes .999. The time
// is read as UTC, matching fossil's default timeline-utc setting.
//
// Supported: the compact YYYYMMDD / YYYYMMDDHHMM / YYYYMMDDHHMMSS forms, the
// punctuated date / minute / second forms with a 'T' or space separator, a
// fractional-second suffix of one to three digits, and an optional trailing
// 'Z'/'z' or 'UTC'. The bare YYYY-MM and hour-only forms are not supported
// (they do not resolve to a check-in time in fossil either).
func parseFossilTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if n := len(s); n >= 3 && strings.EqualFold(s[n-3:], "utc") {
		s = strings.TrimSpace(s[:n-3])
	} else if n >= 1 && (s[n-1] == 'z' || s[n-1] == 'Z') {
		s = s[:n-1]
	}
	if exp, ok := expandCompactDate(s); ok {
		s = exp
	}
	s = strings.Replace(s, "T", " ", 1)

	// Fractional seconds: round up to the last millisecond of the stated
	// precision, as fossil pads the fraction with 9s and then rounds up.
	if head, frac, found := strings.Cut(s, "."); found {
		t, err := time.ParseInLocation("2006-01-02 15:04:05", head, time.UTC)
		if err != nil || len(frac) == 0 || len(frac) > 3 || !isDigits(frac) {
			return time.Time{}, false
		}
		v, _ := strconv.Atoi(frac + strings.Repeat("9", 3-len(frac)))
		if v < 999 {
			v++
		}
		return t.Add(time.Duration(v) * time.Millisecond), true
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC); err == nil {
		return t.Add(999 * time.Millisecond), true
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC); err == nil {
		return t.Add(59*time.Second + 999*time.Millisecond), true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.UTC); err == nil {
		return t.Add(24*time.Hour - time.Millisecond), true
	}
	return time.Time{}, false
}

// expandCompactDate expands fossil's digit-only date forms (src/name.c
// fossil_expand_datetime): YYYYMMDD, YYYYMMDDHHMM and YYYYMMDDHHMMSS to their
// punctuated equivalents, leaving parseFossilTimestamp to round them up.
func expandCompactDate(s string) (string, bool) {
	if !isDigits(s) {
		return "", false
	}
	switch len(s) {
	case 8:
		return s[:4] + "-" + s[4:6] + "-" + s[6:8], true
	case 12:
		return s[:4] + "-" + s[4:6] + "-" + s[6:8] + " " + s[8:10] + ":" + s[10:12], true
	case 14:
		return s[:4] + "-" + s[4:6] + "-" + s[6:8] + " " +
			s[8:10] + ":" + s[10:12] + ":" + s[12:14], true
	}
	return "", false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// trimTrailingPunct drops one or two trailing punctuation characters from a
// name, as fossil does for a name pasted from prose. It returns ok=false when
// there is nothing fossil would trim.
func trimTrailingPunct(name string) (string, bool) {
	n := len(name)
	if n > 4 && isTrailingPunct(name[n-1]) && !isTrailingPunct(name[n-2]) {
		return name[:n-1], true
	}
	if n > 5 && isTrailingPunct(name[n-1]) && isTrailingPunct(name[n-2]) &&
		!isTrailingPunct(name[n-3]) {
		return name[:n-2], true
	}
	return "", false
}

func isTrailingPunct(c byte) bool {
	switch c {
	case '.', '_', ')', '>', '!', '?', ',':
		return true
	}
	return false
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// ReadFileAt reads filePath from the checkin identified by a symbolic version
// name (e.g. "tip", "trunk", a branch name, a UUID, or a UUID prefix).
// It calls ResolveVersion to obtain the RID, then delegates to ReadFile.
// Use ReadFile directly when you already have a numeric RID.
func (r *Repo) ReadFileAt(version string, filePath string) ([]byte, error) {
	rid, err := r.ResolveVersion(version)
	if err != nil {
		return nil, fmt.Errorf("libfossil: read file at %q: %w", version, err)
	}
	return r.ReadFile(rid, filePath)
}

// blobAt returns the bytes of filePath as they exist in the given checkin.
// A checkin that does not contain filePath returns (nil, nil); callers treat
// that as "empty side" for diff purposes. Errors surface only for real I/O
// or consistency problems (unknown RID, missing blob for a listed UUID).
func blobAt(r *Repo, checkinRID int64, filePath string) ([]byte, error) {
	if filePath == "" {
		return nil, nil
	}
	f, found, err := manifest.LookupFile(r.inner, fsltype.FslID(checkinRID), filePath)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	rid, ok := content.AvailableByUUID(r.DB(), f.UUID)
	if !ok {
		return nil, fmt.Errorf("blob not found for uuid %s", f.UUID)
	}
	// Bare Expand: Diff is pairwise (two named checkins), not a walk over a
	// file's whole history, so each side's blob is expanded once. There is
	// no deep overlapping delta chain here for content.Cache to amortize --
	// that pattern lives in annotate, which is cached.
	return content.Expand(r.DB(), rid)
}
