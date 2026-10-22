package content

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// SlotKeep is what an origin keeps of the sub-manifest nodes of the trees a
// horizon keeps, for a request that also carries path members
// (draft-fossil-horizon-00 §6.2): the nodes reached from a kept tree's
// manifest through S-card slots that may hold an in-cone path, as the
// sparse-cone draft's §6.2 server descends them. It withholds every other
// held node, of every age. Each kept node records the lowest root whose walk
// keeps it, which is what §6.2.6's cursor voiding reads.
//
// The walk is zit's walkSlots (src/sparse.zig at 3a2df62515): a node's last
// slot takes its bound from its parent, a node is walked again only under a
// bound that reaches further than every bound it was walked under, and a
// slot that may not reach the cone is never descended, since a child's slot
// lies inside its parent's. It reads the classification's facts, never the
// store, and walks roots in ascending rid order, so a later root that only
// widens a node's bound leaves the earlier root recorded.
type SlotKeep struct {
	facts *ConeReferenceFacts
	kept  map[string]slotKept // by node name
	roots map[string]bool     // the tree manifests walked
	top   libfossil.FslID     // the highest root walked
	// visits counts the node visits of the walk that built this keep from
	// the one it extended: every visit on a fresh walk.
	visits int
}

type slotKept struct {
	bound string          // the widest bound walked under; "" is unbounded
	root  libfossil.FslID // the lowest root whose walk keeps the node
}

type slotRoot struct {
	rid  libfossil.FslID
	uuid string
	deck *deck.Deck
}

// SlotKeep walks the trees of every held check-in manifest, or with reach
// only of those reach names (a structural horizon's, §5.1), for c's path
// members.
func (p *ConeReferences) SlotKeep(ctx context.Context, c *cone.Cone, reach map[string]bool) (*SlotKeep, error) {
	if p == nil || p.facts == nil || ctx == nil || c == nil {
		panic("content.ConeReferences.SlotKeep: nil references, facts, context or cone")
	}
	return extendSlotKeep(ctx, p.facts, c, reach, nil)
}

// extendSlotKeep is from extended by the roots it has not walked, or a fresh
// walk when from is nil, or when a root it walked no longer qualifies or one
// it has not walked lies below its highest: walking roots out of order would
// record a later root for a node an earlier one keeps.
func extendSlotKeep(
	ctx context.Context, facts *ConeReferenceFacts, c *cone.Cone, reach map[string]bool, from *SlotKeep,
) (*SlotKeep, error) {
	if len(c.Include()) == 0 {
		panic("content.extendSlotKeep: a cone without path members withholds no node")
	}
	roots, qualifying := slotRoots(facts, reach)
	if from != nil {
		for uuid := range from.roots {
			if !qualifying[uuid] {
				from = nil
				break
			}
		}
	}
	k := &SlotKeep{facts: facts, kept: map[string]slotKept{}, roots: map[string]bool{}}
	if from != nil {
		fresh := roots[:0:0]
		for _, root := range roots {
			if !from.roots[root.uuid] {
				fresh = append(fresh, root)
			}
		}
		if len(fresh) == 0 && from.facts == facts {
			same := *from // its maps are never written once built
			same.visits = 0
			return &same, nil
		}
		if len(fresh) == 0 || fresh[0].rid > from.top {
			k.kept, k.roots, k.top = maps.Clone(from.kept), maps.Clone(from.roots), from.top
			roots = fresh
		}
	}
	if err := k.walk(ctx, c, facts.byUUID, roots); err != nil {
		return nil, err
	}
	return k, nil
}

// slotRoots lists, ascending by rid, the held check-in manifests that carry
// S cards and that reach names (every one when reach is nil), and the set of
// their names.
func slotRoots(facts *ConeReferenceFacts, reach map[string]bool) ([]slotRoot, map[string]bool) {
	var roots []slotRoot
	names := map[string]bool{}
	for _, record := range facts.records {
		d := record.deck
		if d.Type != deck.Checkin || len(d.S) == 0 || reach != nil && !reach[record.uuid] {
			continue
		}
		rid, ok := facts.held[record.uuid]
		if !ok || names[record.uuid] {
			continue
		}
		names[record.uuid] = true
		roots = append(roots, slotRoot{rid: rid, uuid: record.uuid, deck: d})
	}
	slices.SortFunc(roots, func(a, b slotRoot) int { return cmp.Compare(a.rid, b.rid) })
	return roots, names
}

type slotVisit struct {
	deck  *deck.Deck
	bound string
}

func (k *SlotKeep) walk(ctx context.Context, c *cone.Cone, nodes map[string]*deck.Deck, roots []slotRoot) error {
	var stack []slotVisit
	for _, root := range roots {
		k.roots[root.uuid] = true
		k.top = max(k.top, root.rid)
		stack = append(stack[:0], slotVisit{deck: root.deck})
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			v := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for i, card := range v.deck.S {
				limit := v.bound
				if i+1 < len(v.deck.S) {
					limit = v.deck.S[i+1].Name
				}
				if !c.MayCover(card.Name, limit) {
					continue
				}
				d := nodes[card.UUID]
				if d == nil {
					continue // not a node this store holds
				}
				prev, seen := k.kept[card.UUID]
				if seen && boundCovers(prev.bound, limit) {
					continue
				}
				first := root.rid
				if seen {
					first = prev.root
				}
				k.kept[card.UUID] = slotKept{bound: limit, root: first}
				k.visits++
				if k.visits > coneReferenceNodes {
					return fmt.Errorf("horizon: slot walk bound exceeded")
				}
				stack = append(stack, slotVisit{deck: d, bound: limit})
			}
		}
	}
	return nil
}

// boundCovers reports whether a slot bounded by wide reaches at least as far
// as one bounded by narrow, "" being unbounded: MayCover is monotone in the
// bound, so the wider walk decides for the narrower one.
func boundCovers(wide, narrow string) bool {
	if wide == "" {
		return true
	}
	return narrow != "" && narrow <= wide
}

// Visits is how many node visits the walk that built k made beyond the walk
// it extended: every one for a fresh walk, none for an unchanged store.
func (k *SlotKeep) Visits() int { return k.visits }

// Withholds reports a held sub-manifest node no kept tree reaches through a
// slot that may hold an in-cone path.
func (k *SlotKeep) Withholds(rid libfossil.FslID) bool {
	if k == nil {
		return false
	}
	if _, node := k.facts.nodes[rid]; !node {
		return false
	}
	_, kept := k.kept[k.facts.names[rid]]
	return !kept
}

// Voids reports whether a cursor names a position from before some kept
// node was kept (§6.2.6): the node lies at or below it and every root whose
// walk keeps the node lies above it, so a classification at the cursor
// withheld a node this one keeps and the cursor's holder was never told.
func (k *SlotKeep) Voids(cursor int64) bool {
	if k == nil {
		return false
	}
	for uuid, kept := range k.kept {
		rid := int64(k.facts.held[uuid])
		if rid <= cursor && cursor < int64(kept.root) {
			return true
		}
	}
	return false
}

// Equal reports whether k and o keep the same nodes, each first kept by the
// same root.
func (k *SlotKeep) Equal(o *SlotKeep) bool {
	if k == nil || o == nil {
		return k == o
	}
	return maps.EqualFunc(k.kept, o.kept, func(a, b slotKept) bool { return a.root == b.root }) &&
		maps.Equal(k.roots, o.roots)
}
