// Package bisect implements binary search through Fossil commit history.
// It stores session state in the checkout DB's vvar table and uses BFS
// path-finding to locate the midpoint between known-good and known-bad commits.
package bisect

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/path"
)

// ErrBisectComplete is returned by Next when the search has converged.
var ErrBisectComplete = errors.New("bisect complete")

// Session manages a bisect operation, persisting state in the vvar table.
type Session struct {
	db *sql.DB
}

// StatusInfo describes the current bisect state.
type StatusInfo struct {
	Good  libfossil.FslID
	Bad   libfossil.FslID
	Log   string
	Steps int
}

// ListEntry describes a single node in the bisect path.
type ListEntry struct {
	RID   libfossil.FslID
	UUID  string
	Date  string
	Label string
}

// NewSession creates a new bisect session backed by the given checkout DB.
func NewSession(db *sql.DB) *Session {
	if db == nil {
		panic("bisect.NewSession: db must not be nil")
	}
	return &Session{db: db}
}

// MarkGood records rid as a known-good commit.
func (s *Session) MarkGood(rid libfossil.FslID) error {
	if rid <= 0 {
		panic("bisect.MarkGood: rid must be positive")
	}
	if _, err := s.db.Exec("REPLACE INTO vvar(name,value) VALUES('bisect-good',?)", strconv.FormatInt(int64(rid), 10)); err != nil {
		return fmt.Errorf("bisect: mark good: %w", err)
	}
	return s.appendLog(rid)
}

// MarkBad records rid as a known-bad commit.
func (s *Session) MarkBad(rid libfossil.FslID) error {
	if rid <= 0 {
		panic("bisect.MarkBad: rid must be positive")
	}
	if _, err := s.db.Exec("REPLACE INTO vvar(name,value) VALUES('bisect-bad',?)", strconv.FormatInt(int64(rid), 10)); err != nil {
		return fmt.Errorf("bisect: mark bad: %w", err)
	}
	return s.appendLog(-rid)
}

// Skip marks rid as skipped (neither good nor bad).
func (s *Session) Skip(rid libfossil.FslID) error {
	if rid <= 0 {
		panic("bisect.Skip: rid must be positive")
	}
	return s.appendLogEntry(fmt.Sprintf("s%d", rid))
}

// Next returns the midpoint commit to test next.
// If the bisect has converged, it returns ErrBisectComplete with the bad RID
// embedded in the error message.
func (s *Session) Next() (libfossil.FslID, error) {
	good, bad, err := s.loadEndpoints()
	if err != nil {
		return 0, err
	}

	skip := s.parseSkipSet()

	p, err := path.Shortest(s.db, good, bad, true, nil)
	if err != nil {
		return 0, fmt.Errorf("bisect: path: %w", err)
	}

	if len(p) <= 2 {
		return 0, fmt.Errorf("%w: first bad commit is %d", ErrBisectComplete, bad)
	}

	// Pick midpoint, avoiding skipped nodes.
	mid := len(p) / 2
	if skip != nil {
		// Search outward from the midpoint for a non-skipped node.
		best := -1
		for d := 0; d < len(p); d++ {
			for _, idx := range []int{mid + d, mid - d} {
				if idx > 0 && idx < len(p)-1 && !skip[p[idx].RID] {
					best = idx
					break
				}
			}
			if best >= 0 {
				break
			}
		}
		if best < 0 {
			return 0, fmt.Errorf("%w: first bad commit is %d", ErrBisectComplete, bad)
		}
		mid = best
	}

	return p[mid].RID, nil
}

// Status returns information about the current bisect session.
func (s *Session) Status() StatusInfo {
	good, bad, _ := s.loadEndpoints()
	log := s.loadVvar("bisect-log")

	steps := 0
	if good != 0 && bad != 0 {
		skip := s.parseSkipSet()
		p, err := path.Shortest(s.db, good, bad, true, skip)
		if err == nil && len(p) > 2 {
			steps = int(math.Log2(float64(len(p))))
		}
	}

	return StatusInfo{
		Good:  good,
		Bad:   bad,
		Log:   log,
		Steps: steps,
	}
}

// List returns all commits between good and bad, labelled appropriately.
func (s *Session) List(currentRID libfossil.FslID) ([]ListEntry, error) {
	good, bad, err := s.loadEndpoints()
	if err != nil {
		return nil, err
	}

	skip := s.parseSkipSet()

	p, err := path.Shortest(s.db, good, bad, true, nil)
	if err != nil {
		return nil, fmt.Errorf("bisect: list path: %w", err)
	}

	// Determine "next" for labelling.
	var nextRID libfossil.FslID
	pNoSkip, err := path.Shortest(s.db, good, bad, true, skip)
	if err == nil && len(pNoSkip) > 2 {
		nextRID = pNoSkip[len(pNoSkip)/2].RID
	}

	entries := make([]ListEntry, 0, len(p))
	for _, node := range p {
		var uuid, date string
		row := s.db.QueryRow(`SELECT b.uuid, COALESCE(e.mtime, 0)
			FROM blob b LEFT JOIN event e ON e.objid=b.rid
			WHERE b.rid=?`, node.RID)
		var mtimeRaw any
		if err := row.Scan(&uuid, &mtimeRaw); err != nil {
			uuid = "?"
		}
		if mtime, ok := db.ScanJulianDay(mtimeRaw); ok && mtime != 0 {
			date = fmt.Sprintf("%.6f", mtime)
		}

		label := ""
		switch {
		case node.RID == good:
			label = "GOOD"
		case node.RID == bad:
			label = "BAD"
		case node.RID == currentRID:
			label = "CURRENT"
		case node.RID == nextRID:
			label = "NEXT"
		case skip[node.RID]:
			label = "SKIP"
		}

		entries = append(entries, ListEntry{
			RID:   node.RID,
			UUID:  uuid,
			Date:  date,
			Label: label,
		})
	}

	return entries, nil
}

// Reset clears all bisect state from the vvar table.
func (s *Session) Reset() {
	if _, err := s.db.Exec("DELETE FROM vvar WHERE name IN ('bisect-good','bisect-bad','bisect-log')"); err != nil {
		panic(fmt.Sprintf("bisect.Reset: failed to clear bisect state: %v", err))
	}
}

// --- internal helpers ---

func (s *Session) loadEndpoints() (good, bad libfossil.FslID, err error) {
	gStr := s.loadVvar("bisect-good")
	bStr := s.loadVvar("bisect-bad")
	if gStr == "" || bStr == "" {
		return 0, 0, fmt.Errorf("bisect: good and bad must both be set")
	}
	g, err := strconv.ParseInt(gStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bisect: parse good: %w", err)
	}
	b, err := strconv.ParseInt(bStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bisect: parse bad: %w", err)
	}
	return libfossil.FslID(g), libfossil.FslID(b), nil
}

func (s *Session) loadVvar(name string) string {
	var val string
	if err := s.db.QueryRow("SELECT value FROM vvar WHERE name=?", name).Scan(&val); err != nil {
		return ""
	}
	return val
}

func (s *Session) appendLog(rid libfossil.FslID) error {
	return s.appendLogEntry(strconv.FormatInt(int64(rid), 10))
}

func (s *Session) appendLogEntry(entry string) error {
	cur := s.loadVvar("bisect-log")
	if cur != "" {
		cur += " "
	}
	cur += entry
	_, err := s.db.Exec("REPLACE INTO vvar(name,value) VALUES('bisect-log',?)", cur)
	if err != nil {
		return fmt.Errorf("bisect: update log: %w", err)
	}
	return nil
}

func (s *Session) parseSkipSet() map[libfossil.FslID]bool {
	log := s.loadVvar("bisect-log")
	if log == "" {
		return nil
	}
	skip := make(map[libfossil.FslID]bool)
	for _, tok := range strings.Fields(log) {
		if strings.HasPrefix(tok, "s") {
			if id, err := strconv.ParseInt(tok[1:], 10, 64); err == nil {
				skip[libfossil.FslID(id)] = true
			}
		}
	}
	if len(skip) == 0 {
		return nil
	}
	return skip
}
