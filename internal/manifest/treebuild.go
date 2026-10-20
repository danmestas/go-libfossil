package manifest

// Writing tree manifests, draft-fossil-submanifests-00 §6: the partition
// predicate (§6.1), the canonical construction (§6.2), and the cost that
// selects and re-tunes the partition parameter (§6.3, §6.4). Everything here
// is a pure function of a check-in's file entries and k; storing the result,
// and deciding whether to build a tree at all, is the commit path's.

import (
	"fmt"
	"math/bits"

	"github.com/danmestas/go-libfossil/internal/deck"
	"golang.org/x/crypto/sha3"
)

// The range of the partition parameter k a writer supports (§6.3).
const (
	MinPartitionK = 1
	MaxPartitionK = 12
)

// maxConstructHeight is §6.2 step 3's cap: no node of height 16 is built,
// so a canonical tree stays within the reader's depth bound of §5.1.
const maxConstructHeight = maxTreeDepth

// zeroBits is §6.1's zbits(name): the leading zero bits of the SHA3-256
// digest of the decoded filename.
func zeroBits(name string) int {
	sum := sha3.Sum256([]byte(name))
	n := 0
	for _, b := range sum {
		if b != 0 {
			return n + bits.LeadingZeros8(b)
		}
		n += 8
	}
	return n
}

// partitionLimits are §6.2's MIN and MAX member counts for k.
func partitionLimits(k int) (lo, hi int) {
	if k < MinPartitionK || k > MaxPartitionK {
		panic(fmt.Sprintf("manifest.partitionLimits: k %d outside %d..%d", k, MinPartitionK, MaxPartitionK))
	}
	span := 1 << k
	return span / 4, 4 * span
}

// partitionGroups is §6.2 step 1's scan over the zero-bit counts of ordered
// keys, run at every height: a key starts a new group when the open group
// holds hi members, or when its count reaches threshold and the open group
// holds at least lo; a final group shorter than lo merges into its
// predecessor when the two fit in hi. It returns the group boundaries
// closed by a sentinel, so group i is keys bounds[i]..bounds[i+1].
func partitionGroups(zbits []int, threshold, lo, hi int) []int {
	if len(zbits) == 0 {
		return nil
	}
	bounds := []int{0}
	for i := 1; i < len(zbits); i++ {
		open := i - bounds[len(bounds)-1]
		if open == hi || (zbits[i] >= threshold && open >= lo) {
			bounds = append(bounds, i)
		}
	}
	if n := len(bounds); n >= 2 {
		last, prev := bounds[n-1], bounds[n-2]
		if len(zbits)-last < lo && len(zbits)-prev <= hi {
			bounds = bounds[:n-1]
		}
	}
	return append(bounds, len(zbits))
}

// firstKeys is the zero-bit count of each group's key: its first member's.
func firstKeys(zbits, bounds []int) []int {
	out := make([]int, len(bounds)-1)
	for g := range out {
		out[g] = zbits[bounds[g]]
	}
	return out
}

// treePlan is §6.2's partition of the file entries whose zero-bit counts
// are zbits: the leaf boundaries, then each interior height's boundaries
// over the level below. It is empty when the construction is flat. maxLevels
// is maxConstructHeight for every tree a writer emits; a test passes less
// to reach the cap.
func treePlan(zbits []int, k, maxLevels int) [][]int {
	lo, hi := partitionLimits(k)
	leaves := partitionGroups(zbits, k, lo, hi)
	if len(leaves) < 3 {
		return nil // fewer than two groups: flat (step 2)
	}
	levels := [][]int{leaves}
	keys := firstKeys(zbits, leaves)
	for h := 1; h < maxLevels; h++ {
		bounds := partitionGroups(keys, (h+1)*k, lo, hi)
		groups := len(bounds) - 1
		if groups == 1 || groups == len(keys) {
			break // the manifest carries this level's nodes (step 3)
		}
		levels = append(levels, bounds)
		keys = firstKeys(keys, bounds)
	}
	return levels
}

// Tree is one check-in's canonical construction.
type Tree struct {
	// Subs are the sub-manifest artifacts in storage order: leaves, then
	// each height in turn, so none precedes an artifact it names (§6.2).
	Subs [][]byte
	// S is the manifest's S run, in key order. Both are empty when the
	// construction is flat.
	S []deck.SubmanifestCard
}

// BuildTree runs §6.2's construction over files, the check-in's entries in
// strict canonical filename order, each carrying a hash, at partition
// parameter k. name returns an artifact's name under the repository's hash
// policy; it is called on each sub-manifest in storage order, so a caller
// that stores as it names stores children before parents.
func BuildTree(files []deck.FileCard, k int, name func([]byte) (string, error)) (Tree, error) {
	if name == nil {
		panic("manifest.BuildTree: name must not be nil")
	}
	zbits := make([]int, len(files))
	for i, f := range files {
		if f.UUID == "" {
			panic("manifest.BuildTree: every file must carry a hash")
		}
		if i > 0 && deck.Compare(files[i-1].Name, f.Name) >= 0 {
			panic("manifest.BuildTree: files must ascend strictly by name")
		}
		zbits[i] = zeroBits(f.Name)
	}
	plan := treePlan(zbits, k, maxConstructHeight)
	if len(plan) == 0 {
		return Tree{}, nil
	}
	var tree Tree
	emit := func(data []byte, key string) (deck.SubmanifestCard, error) {
		uuid, err := name(data)
		if err != nil {
			return deck.SubmanifestCard{}, err
		}
		tree.Subs = append(tree.Subs, data)
		return deck.SubmanifestCard{Name: key, UUID: uuid}, nil
	}
	nodes := make([]deck.SubmanifestCard, 0, len(plan[0])-1)
	for g := 0; g+1 < len(plan[0]); g++ {
		group := files[plan[0][g]:plan[0][g+1]]
		node, err := emit(deck.MarshalLeaf(group), group[0].Name)
		if err != nil {
			return Tree{}, err
		}
		nodes = append(nodes, node)
	}
	for _, bounds := range plan[1:] {
		next := make([]deck.SubmanifestCard, 0, len(bounds)-1)
		for g := 0; g+1 < len(bounds); g++ {
			members := nodes[bounds[g]:bounds[g+1]]
			node, err := emit(deck.MarshalInterior(members), members[0].Name)
			if err != nil {
				return Tree{}, err
			}
			next = append(next, node)
		}
		nodes = next
	}
	tree.S = nodes
	return tree, nil
}

// zCardLen is the length of a Z card line: "Z ", 32 hex digits, LF.
const zCardLen = 35

// analysisHashLen is the length of an S card's hash in the cost of §6.3,
// which analyzes every k under the sha3 policy.
const analysisHashLen = 64

// retuneMarginPct is the relative margin m of §6.4, as a percentage of the
// pinned value's cost: an automatic re-tune happens only when the candidate
// costs more than 15% less than the pin. It is zit's margin, so the two
// writers re-tune a shared repository alike.
const retuneMarginPct = 15.0

// partitionCost is §6.3's C(k) for one k. A k whose leaf scan yields fewer
// than two groups is no candidate and has no cost.
type partitionCost struct {
	candidate bool
	total     float64
}

// partitionCosts is C(k) over files for every k from MinPartitionK to
// MaxPartitionK, indexed by k. Each leaf is named by exactly one S card, so
// the cost reads only the leaf partition: S is the length of the S card
// keyed by each leaf's first name, and the leaf lengths are its F cards'
// plus a Z card. The arithmetic is zit's, operation for operation, so the
// two select and re-tune alike.
func partitionCosts(files []deck.FileCard) [MaxPartitionK + 1]partitionCost {
	zbits := make([]int, len(files))
	cards := make([]int, len(files))
	for i, f := range files {
		zbits[i] = zeroBits(f.Name)
		cards[i] = len(deck.LeafCard(f))
	}
	var out [MaxPartitionK + 1]partitionCost
	for k := MinPartitionK; k <= MaxPartitionK; k++ {
		lo, hi := partitionLimits(k)
		bounds := partitionGroups(zbits, k, lo, hi)
		if len(bounds) < 3 {
			continue
		}
		sBytes := 0
		var sizeSum, squareSum float64
		for g := 0; g+1 < len(bounds); g++ {
			leaf := zCardLen
			for i := bounds[g]; i < bounds[g+1]; i++ {
				leaf += cards[i]
			}
			size := float64(leaf)
			sizeSum += size
			squareSum += size * size
			sBytes += len("S ") + len(deck.CanonicalEncode(files[bounds[g]].Name)) + 1 + analysisHashLen + 1
		}
		out[k] = partitionCost{candidate: true, total: float64(sBytes) + squareSum/sizeSum}
	}
	return out
}

// bestPartitionK is the candidate of least cost, the lowest k on a tie
// (§6.3), or 0 when no k is a candidate.
func bestPartitionK(costs [MaxPartitionK + 1]partitionCost) int {
	best := 0
	for k := MinPartitionK; k <= MaxPartitionK; k++ {
		if costs[k].candidate && (best == 0 || costs[k].total < costs[best].total) {
			best = k
		}
	}
	return best
}

// SelectPartitionK is §6.3's automatic selection over a check-in's file
// entries, in canonical order. It reports false when no k is a candidate,
// in which case selection has no result and no value may be substituted.
func SelectPartitionK(files []deck.FileCard) (int, bool) {
	k := bestPartitionK(partitionCosts(files))
	return k, k != 0
}

// retuneTarget is §6.4's gate for an automatic re-tune from pinned: the
// best candidate, when it is distinct and the pin is a candidate costing
// more than retuneMarginPct percent above it.
func retuneTarget(costs [MaxPartitionK + 1]partitionCost, pinned int) (int, bool) {
	best := bestPartitionK(costs)
	if best == 0 || best == pinned || !costs[pinned].candidate {
		return 0, false
	}
	current := costs[pinned].total
	margin := (current - costs[best].total) / current * 100
	if margin <= retuneMarginPct {
		return 0, false
	}
	return best, true
}
