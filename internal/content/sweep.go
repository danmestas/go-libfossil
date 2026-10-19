package content

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ExpandAll expands every non-phantom blob exactly once and hands it to visit
// with its stored name, as fossil's rebuild_step does: it walks each delta
// tree down from its root, expanding a node from its parent's content and
// passing that content on to the node's own delta children. Every stored blob
// is read once and every delta applied once, however deep the chains run.
//
// Expanding blob by blob instead -- even through a Cache -- re-walks a chain
// whenever its interiors have been evicted, which on a real repository (chains
// averaging hundreds of nodes) is O(depth^2) blob reads and hashes per chain.
//
// visit receives what Cache.Expand would return for that blob alone: its
// content, bound to its name, or the same error. A node below a failed one
// gets the failure that stopped its chain. A blob no sound root reaches -- its
// chain cycles, ends in a phantom or a missing row, or exceeds
// maxDeltaChainDepth -- is expanded on its own after the walk, for that
// expansion's error. data is valid only during the call and must not be
// modified; it may be the source of later deltas.
//
// A non-nil error from visit stops the sweep and is returned as is.
//
// Memory held is the content of the nodes whose children are still pending:
// one buffer down a straight chain, more only where a tree branches.
func ExpandAll(q db.Querier, visit func(rid libfossil.FslID, uuid string, data []byte, err error) error) error {
	if q == nil {
		panic("content.ExpandAll: q must not be nil")
	}
	if visit == nil {
		panic("content.ExpandAll: visit must not be nil")
	}

	g, err := loadSweepGraph(q)
	if err != nil {
		return fmt.Errorf("content.ExpandAll: %w", err)
	}
	reached, err := g.walk(q, visit)
	if err != nil {
		return err
	}
	for i, rid := range g.rids {
		if reached[i] {
			continue
		}
		data, err := expandChain(q, rid, nil, nil, true)
		if err := visit(rid, g.uuids[i], data, err); err != nil {
			return err
		}
	}
	return nil
}

// sweepGraph is the delta forest over the non-phantom blobs, in rid order.
// children[i] lists the blobs stored as deltas against blob i.
type sweepGraph struct {
	rids     []libfossil.FslID
	uuids    []string
	isDelta  []bool
	children [][]int32
}

// sweepFrame is one pending node of the walk. base is the parent's content
// and err the parent's failure; a root has neither.
type sweepFrame struct {
	node  int32
	depth int32
	base  []byte
	err   error
}

// walk expands every blob reachable from a root, depth first with an
// explicit stack, and reports which blobs it reached. A delta row names one
// source, so each blob has at most one parent and is pushed at most once.
func (g *sweepGraph) walk(q db.Querier, visit func(libfossil.FslID, string, []byte, error) error) ([]bool, error) {
	reached := make([]bool, len(g.rids))
	stack := make([]sweepFrame, 0, 64)
	for i := len(g.rids) - 1; i >= 0; i-- {
		if !g.isDelta[i] {
			stack = append(stack, sweepFrame{node: int32(i)})
		}
	}

	var inf blob.Inflater
	for pops := 0; len(stack) > 0; pops++ {
		if pops >= len(g.rids) {
			panic("content.ExpandAll: walk visited a blob twice")
		}
		f := stack[len(stack)-1]
		stack[len(stack)-1] = sweepFrame{} // drop the base reference
		stack = stack[:len(stack)-1]
		reached[f.node] = true

		data, err := g.materialize(q, &inf, f)
		if verr := visit(g.rids[f.node], g.uuids[f.node], data, err); verr != nil {
			return nil, verr
		}
		if f.depth+1 >= maxDeltaChainDepth {
			continue // left unreached: expanded alone for its depth error
		}
		for _, c := range g.children[f.node] {
			stack = append(stack, sweepFrame{node: c, depth: f.depth + 1, base: data, err: err})
		}
	}
	return reached, nil
}

// materialize produces f's content from its parent's and binds it to its
// name, failing exactly as expandChain does on the same link.
func (g *sweepGraph) materialize(q db.Querier, inf *blob.Inflater, f sweepFrame) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	rid := g.rids[f.node]
	var data []byte
	if !g.isDelta[f.node] {
		loaded, err := inf.Load(q, rid)
		if err != nil {
			return nil, fmt.Errorf("content.Expand load root rid=%d: %w", rid, err)
		}
		data = loaded
	} else {
		if f.base == nil {
			panic("content.ExpandAll: delta frame without base content")
		}
		deltaBytes, err := inf.Load(q, rid)
		if err != nil {
			return nil, fmt.Errorf("content.Expand load delta rid=%d: %w", rid, err)
		}
		data, err = delta.Apply(f.base, deltaBytes)
		if err != nil {
			return nil, fmt.Errorf("content.Expand apply delta rid=%d: %w", rid, err)
		}
	}
	if err := bindToUUID(rid, g.uuids[f.node], data); err != nil {
		return nil, err
	}
	return data, nil
}

// loadSweepGraph reads every non-phantom blob and the delta edges between
// them. A delta row whose source is not among them still marks its blob as a
// delta, so the blob is no root and is left for ExpandAll's fallback.
func loadSweepGraph(q db.Querier) (*sweepGraph, error) {
	g := &sweepGraph{}
	index := make(map[libfossil.FslID]int32)
	rows, err := q.Query("SELECT rid, uuid FROM blob WHERE size >= 0 ORDER BY rid")
	if err != nil {
		return nil, fmt.Errorf("query blobs: %w", err)
	}
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan blob: %w", err)
		}
		index[rid] = int32(len(g.rids))
		g.rids = append(g.rids, rid)
		g.uuids = append(g.uuids, uuid)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close blobs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blobs: %w", err)
	}

	g.isDelta = make([]bool, len(g.rids))
	g.children = make([][]int32, len(g.rids))
	if err := g.loadEdges(q, index); err != nil {
		return nil, err
	}
	return g, nil
}

// loadEdges records each delta row between two non-phantom blobs as an edge
// from source to delta.
func (g *sweepGraph) loadEdges(q db.Querier, index map[libfossil.FslID]int32) error {
	rows, err := q.Query("SELECT rid, srcid FROM delta")
	if err != nil {
		return fmt.Errorf("query deltas: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid, srcid libfossil.FslID
		if err := rows.Scan(&rid, &srcid); err != nil {
			return fmt.Errorf("scan delta: %w", err)
		}
		child, ok := index[rid]
		if !ok {
			continue
		}
		g.isDelta[child] = true
		if parent, ok := index[srcid]; ok {
			g.children[parent] = append(g.children[parent], child)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("deltas: %w", err)
	}
	return nil
}
