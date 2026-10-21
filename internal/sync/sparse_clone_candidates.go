package sync

import (
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

type sparsePreparedProof struct {
	facts *content.ConeReferenceFacts
	index *content.ConeSuccessors
	work  content.SuccessorWork
}

type cloneCandidateRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

// cloneCandidates keeps dense behavior unchanged. The prepared path seeks the
// next eligible RID in memory, then performs one indexed blob lookup; excluded
// gaps do not become database rows or content reads on the serving path.
func (h *handler) cloneCandidates() (cloneCandidateRows, error) {
	if h.keep != nil && h.keep.indexed != nil {
		return &sparseCloneRows{handler: h, next: libfossil.FslID(h.cloneSeq)}, nil
	}
	return h.repo.DB().Query(
		"SELECT rid, uuid, size FROM blob WHERE rid >= ? AND rid <= ? AND size >= 0 ORDER BY rid",
		h.cloneSeq, h.cloneSnapMax,
	)
}

type sparseCloneRows struct {
	handler *handler
	next    libfossil.FslID
	current libfossil.FslID
	err     error
}

func (rows *sparseCloneRows) Next() bool {
	if rows.next == 0 {
		return false
	}
	if err := rows.handler.ctx.Err(); err != nil {
		rows.err = err
		return false
	}
	query := rows.handler.keep.indexed
	last := libfossil.FslID(rows.handler.cloneSnapMax)
	rows.current = query.Next(rows.next, last)
	if rows.current == 0 {
		return false
	}
	query.Work.Candidates++
	if rows.current == last {
		rows.next = 0
	} else {
		rows.next = rows.current + 1
	}
	return true
}

func (rows *sparseCloneRows) Scan(dest ...any) error {
	return rows.handler.repo.DB().QueryRow(
		"SELECT rid,uuid,size FROM blob WHERE rid=? AND size>=0", rows.current,
	).Scan(dest...)
}

func (rows *sparseCloneRows) Err() error   { return rows.err }
func (rows *sparseCloneRows) Close() error { return nil }
