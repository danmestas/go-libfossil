package content

import (
	"context"
	"fmt"
	"math/bits"
	"sort"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

type coneAlias struct {
	path string
	rid  libfossil.FslID
}

// ConeSuccessors is an immutable, cone-independent merge-sort range tree over
// verified raw F-card aliases. Its fixed retained list includes unknown and
// structural artifacts and all tree nodes. This is not tree-minimal emission
// under §6.2; §5.1's permitted holdings alone do not prove that serving contract.
// The range tree takes O(A log A) space/work; each successor costs
// O(C log A log A), independent of excluded RIDs between eligible artifacts.
// Compiling request-local spans costs O(C*C + C log A), with C <= 255.
type ConeSuccessors struct {
	aliases []coneAlias
	always  []libfossil.FslID
	base    int
	tree    [][]libfossil.FslID
}

type SuccessorWork struct {
	NextCalls       int
	IndexVisits     int
	RIDComparisons  int
	PathComparisons int
	SpanDecisions   int
	Candidates      int
}

type ConeSuccessorQuery struct {
	index *ConeSuccessors
	spans [][2]int
	Work  SuccessorWork
}

func NewConeSuccessors(
	ctx context.Context, facts *ConeReferenceFacts, visible []libfossil.FslID,
	maxAliases, maxSlots int,
) (*ConeSuccessors, error) {
	if ctx == nil || facts == nil {
		panic("content.NewConeSuccessors: nil context or facts")
	}
	if maxAliases <= 0 || maxSlots <= 0 {
		return nil, fmt.Errorf("sparse index: invalid budget")
	}
	if len(visible) > maxSlots || len(facts.records) > maxSlots {
		return nil, fmt.Errorf("sparse index: record budget exceeded")
	}
	// Count before allocating aliases. Count all references, even duplicates
	// and invisible targets: none can bypass the preparation work bound.
	count := 0
	for _, record := range facts.records {
		if len(record.deck.F) > maxAliases-count {
			return nil, fmt.Errorf("sparse index: alias budget exceeded")
		}
		count += len(record.deck.F)
	}
	base := 1
	for base < count {
		if base > maxSlots/2 {
			return nil, fmt.Errorf("sparse index: slot budget exceeded")
		}
		base *= 2
	}
	levels := bits.Len(uint(base))
	if base > maxSlots/6 || count > (maxSlots-6*base)/levels {
		return nil, fmt.Errorf("sparse index: slot budget exceeded")
	}
	index := &ConeSuccessors{base: base, tree: make([][]libfossil.FslID, 2*base)}
	index.aliases = make([]coneAlias, 0, count)
	if err := index.prepareAliases(ctx, facts, visible); err != nil {
		return nil, err
	}
	for i, alias := range index.aliases {
		index.tree[base+i] = []libfossil.FslID{alias.rid}
	}
	for node := base - 1; node > 0; node-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index.tree[node] = mergeConeRIDs(index.tree[2*node], index.tree[2*node+1])
	}
	return index, nil
}

func (index *ConeSuccessors) prepareAliases(
	ctx context.Context, facts *ConeReferenceFacts, visible []libfossil.FslID,
) error {
	allowed := make(map[libfossil.FslID]bool, len(visible))
	for _, rid := range visible {
		allowed[rid] = true
	}
	required := &ConeReferences{required: map[string]bool{}}
	hasAlias := make(map[libfossil.FslID]bool)
	for _, uuid := range facts.unreadable {
		required.required[uuid] = true
	}
	for _, record := range facts.records {
		if err := ctx.Err(); err != nil {
			return err
		}
		required.required[record.uuid] = true
		required.structural(record.deck)
		for _, child := range record.deck.S {
			required.required[child.UUID] = true
		}
		for _, f := range record.deck.F {
			rid := facts.held[f.UUID]
			if f.UUID != "" && allowed[rid] {
				index.aliases = append(index.aliases, coneAlias{path: f.Name, rid: rid})
				hasAlias[rid] = true
			}
		}
	}
	for _, rid := range visible {
		if required.required[facts.names[rid]] || !hasAlias[rid] {
			index.always = append(index.always, rid)
		}
	}
	sort.Slice(index.always, func(i, j int) bool { return index.always[i] < index.always[j] })
	sort.Slice(index.aliases, func(i, j int) bool {
		a, b := index.aliases[i], index.aliases[j]
		if a.path != b.path {
			return a.path < b.path
		}
		return a.rid < b.rid
	})
	return nil
}

func mergeConeRIDs(a, b []libfossil.FslID) []libfossil.FslID {
	out := make([]libfossil.FslID, 0, len(a)+len(b))
	for i, j := 0, 0; i < len(a) || j < len(b); {
		if j == len(b) || i < len(a) && a[i] < b[j] {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return out
}

// Query partitions alias positions only at cone boundaries, not by scanning
// the alias list. Equality and slash-descendant intervals are distinct: src-x
// is not covered by src. Re-inclusions/exclusions are decided on each span.
func (index *ConeSuccessors) Query(c *cone.Cone) *ConeSuccessorQuery {
	q := &ConeSuccessorQuery{index: index}
	if c == nil {
		q.spans = append(q.spans, [2]int{0, len(index.aliases)})
		return q
	}
	positions := make([]int, 0, 2+4*cone.MaxMembers)
	positions = append(positions, 0, len(index.aliases))
	for _, members := range [2][]string{c.Include(), c.Exclude()} {
		for _, member := range members {
			for _, boundary := range [4]string{member, member + "\x00", member + "/", member + "0"} {
				position := sort.Search(len(index.aliases), func(i int) bool {
					q.Work.PathComparisons++
					return index.aliases[i].path >= boundary
				})
				positions = append(positions, position)
			}
		}
	}
	sort.Ints(positions)
	for i := 1; i < len(positions); i++ {
		first, last := positions[i-1], positions[i]
		if first == last {
			continue
		}
		q.Work.SpanDecisions++
		if c.Covers(index.aliases[first].path) {
			q.spans = append(q.spans, [2]int{first, last})
		}
	}
	return q
}

func (q *ConeSuccessorQuery) Next(first, last libfossil.FslID) libfossil.FslID {
	q.Work.NextCalls++
	best := q.search(q.index.always, first)
	for _, span := range q.spans {
		left, right := span[0]+q.index.base, span[1]+q.index.base
		for left < right {
			if left&1 != 0 {
				best = q.minimum(best, q.search(q.index.tree[left], first))
				left++
			}
			if right&1 != 0 {
				right--
				best = q.minimum(best, q.search(q.index.tree[right], first))
			}
			left, right = left/2, right/2
		}
	}
	if best > last {
		return 0
	}
	return best
}

func (q *ConeSuccessorQuery) minimum(a, b libfossil.FslID) libfossil.FslID {
	if a == 0 || b != 0 && b < a {
		return b
	}
	return a
}

func (q *ConeSuccessorQuery) search(rids []libfossil.FslID, first libfossil.FslID) libfossil.FslID {
	q.Work.IndexVisits++
	at := sort.Search(len(rids), func(i int) bool {
		q.Work.RIDComparisons++
		return rids[i] >= first
	})
	if at == len(rids) {
		return 0
	}
	return rids[at]
}
