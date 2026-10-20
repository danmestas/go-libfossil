package tag

import (
	"fmt"
	"sort"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// InsertControlEvent writes the event row fossil's manifest_crosslink
// writes for control artifact d stored at rid: type 'g', at the D-card
// time, with a comment describing every tag change d makes.
func InsertControlEvent(tx *db.Tx, rid fsltype.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("tag.InsertControlEvent: tx must not be nil")
	}
	if rid <= 0 {
		panic("tag.InsertControlEvent: rid must be positive")
	}
	human, url := fsltype.HashDigits(tx)
	if _, err := tx.Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('g', ?, ?, ?, ?)",
		fsltype.TimeToTextJulian(d.D), rid, d.U, controlComment(d, human, url),
	); err != nil {
		return fmt.Errorf("tag.InsertControlEvent: %w", err)
	}
	return nil
}

// controlComment builds the comment of a control artifact's event row
// as manifest_crosslink does: its tag changes ordered by target hash, then by
// full tag name sign included, each target introduced by an "Edit" link.
func controlComment(d *deck.Deck, human, url int) string {
	tags := make([]deck.TagCard, 0, len(d.T))
	for _, tc := range d.T {
		if tc.UUID != "*" {
			tags = append(tags, tc)
		}
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].UUID != tags[j].UUID {
			return tags[i].UUID < tags[j].UUID
		}
		return string(tags[i].Type)+tags[i].Name < string(tags[j].Type)+tags[j].Name
	})
	var b strings.Builder
	branchMove := false
	for i, tc := range tags {
		if i == 0 || tc.UUID != tags[i-1].UUID {
			fmt.Fprintf(&b, " Edit [%s|%s]:", fsltype.HashPrefix(tc.UUID, url), fsltype.HashPrefix(tc.UUID, human))
			branchMove = false
		}
		branchMove = appendTagChange(&b, tc, url, branchMove)
	}
	// fossil drops the first byte, the space before the first phrase.
	return strings.TrimPrefix(b.String(), " ")
}

// tagChangeVerb names a change to a tag a control comment has no phrase for.
var tagChangeVerb = map[deck.TagType]string{
	deck.TagCancel: "Cancel", deck.TagSingleton: "Add", deck.TagPropagating: "Add propagating",
}

// appendTagChange appends one tag change's phrase to a control comment and
// reports whether the target has moved to a new branch, which silences the
// sym- tag changes that accompany the move.
func appendTagChange(b *strings.Builder, tc deck.TagCard, url int, branchMove bool) bool {
	name := string(tc.Type) + tc.Name
	value := fsltype.HTMLize(tc.Value)
	switch {
	case name == "*branch":
		fmt.Fprintf(b, " Move to branch [/timeline?r=%s&nd&dp=%s&unhide | %s].",
			fsltype.HTTPize(tc.Value), fsltype.HashPrefix(tc.UUID, url), value)
		return true
	case name == "*bgcolor":
		fmt.Fprintf(b, " Change branch background color to \"%s\".", value)
		return branchMove
	case name == "+bgcolor":
		fmt.Fprintf(b, " Change background color to \"%s\".", value)
		return branchMove
	case name == "+comment":
		b.WriteString(" Edit check-in comment.")
		return branchMove
	case name == "+user":
		fmt.Fprintf(b, " Change user to \"%s\".", value)
		return branchMove
	case name == "+date":
		fmt.Fprintf(b, " Timestamp %s.", value)
		return branchMove
	case name == "-bgcolor":
		b.WriteString(" Cancel background color")
	case strings.HasPrefix(name, "-sym-") && branchMove,
		strings.HasPrefix(name, "*sym-") && branchMove:
		return branchMove
	case strings.HasPrefix(name, "-sym-"):
		fmt.Fprintf(b, " Cancel tag \"%s\"", fsltype.HTMLize(name[5:]))
	case strings.HasPrefix(name, "*sym-"):
		fmt.Fprintf(b, " Add propagating tag \"%s\"", fsltype.HTMLize(name[5:]))
	case strings.HasPrefix(name, "+sym-"):
		fmt.Fprintf(b, " Add tag \"%s\"", fsltype.HTMLize(name[5:]))
	case name == "+closed":
		b.WriteString(` Mark "Closed"`)
	case name == "-closed":
		b.WriteString(` Remove the "Closed" mark`)
	default:
		fmt.Fprintf(b, " %s \"%s\"", tagChangeVerb[tc.Type], fsltype.HTMLize(tc.Name))
		if tc.Value != "" {
			fmt.Fprintf(b, " with value \"%s\".", value)
		} else {
			b.WriteString(".")
		}
		return branchMove
	}
	if tc.Value != "" {
		fmt.Fprintf(b, " with note \"%s\".", value)
	} else {
		b.WriteString(".")
	}
	return branchMove
}
