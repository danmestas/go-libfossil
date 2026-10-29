package content

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

func TestTreeIDsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 2, 127, 128, 1000, 5000} {
		set := map[uint32]bool{}
		for len(set) < n {
			set[1+rng.Uint32N(1<<31)] = true
		}
		ids := slices.Sorted(func(yield func(uint32) bool) {
			for id := range set {
				if !yield(id) {
					return
				}
			}
		})
		enc := appendTreeIDs(nil, ids)
		if got := decodeTreeIDs(nil, enc); !slices.Equal(got, ids) {
			t.Fatalf("%d ids: round trip differs", n)
		}
	}
	for _, enc := range [][]byte{
		binary.AppendUvarint(binary.AppendUvarint(nil, math.MaxUint32), 1),
		binary.AppendUvarint(binary.AppendUvarint(nil, 1), math.MaxUint64),
		binary.AppendUvarint(nil, 0),
		{0x80},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("% x decoded", enc)
				}
			}()
			decodeTreeIDs(nil, enc)
		}()
	}
	top := []uint32{1, math.MaxUint32}
	if got := decodeTreeIDs(nil, appendTreeIDs(nil, top)); !slices.Equal(got, top) {
		t.Fatalf("decoded %v, want %v", got, top)
	}
	dense := []uint32{1, 2, 3, 4, 5, 100, 227}
	if enc := appendTreeIDs(nil, dense); len(enc) != len(dense) {
		t.Fatalf("dense ids take %d bytes, want one each", len(enc))
	}
}

// treeIndexFixture is a store's trees as a loader reads them, in the order
// it reads them: full check-ins that drift a few files at a time, a few
// unrelated ones (which a set difference would not shrink), delta
// manifests on those baselines, and sub-manifest trees with their nodes.
func treeIndexFixture(rng *rand.Rand) (names []string, trees []*plainTree) {
	file := func(i int) string { return fmt.Sprintf("%040x", i) }
	current := map[int]bool{}
	for i := 0; i < 300; i++ {
		current[i] = true
	}
	next := 300
	add := func(name string, tr *plainTree) {
		names = append(names, name)
		trees = append(trees, tr)
	}
	for i := 0; i < 120; i++ {
		for j := 0; j < 1+rng.IntN(6); j++ {
			for k := range current {
				delete(current, k)
				break
			}
			current[next] = true
			next++
		}
		files := make([]string, 0, len(current)+1)
		for k := range current {
			files = append(files, file(k))
		}
		if i%37 == 5 {
			files = []string{file(next), file(next + 1)}
			next += 2
		}
		if len(files) > 0 {
			files = append(files, files[0]) // two paths, one blob
		}
		rid := libfossil.FslID(1000 - i)
		add(fmt.Sprintf("c%039d", i), &plainTree{rid: rid, size: int64(10 * i), checkin: true, files: files})
		if i%10 == 3 {
			add(fmt.Sprintf("d%039d", i), &plainTree{rid: rid + 2000, size: 7, checkin: true,
				b: fmt.Sprintf("c%039d", i), files: []string{file(next), file(1)}})
			next++
		}
		if i%25 == 9 {
			node := fmt.Sprintf("n%039d", i)
			add(node, &plainTree{rid: rid + 4000, size: 3, files: []string{file(2), file(next)}})
			add(fmt.Sprintf("t%039d", i), &plainTree{rid: rid + 6000, size: 5, checkin: true,
				s: []string{node, "missing" + node}})
			next++
		}
	}
	add(fmt.Sprintf("e%039d", 0), &plainTree{rid: 9000, size: 11})
	return names, trees
}

// draft-fossil-horizon-00 §6.2: a tree's file set read back from the memo's
// encoding is exactly the set its F cards name, whichever order a walk
// asks for the trees in and however long the chain of set differences
// behind it.
func TestTreeIndexRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	names, trees := treeIndexFixture(rng)
	x := newTreeIndex()
	for i, tr := range trees {
		x.add(names[i], tr)
	}
	diffs := 0
	for _, name := range names {
		if p := x.piece(name); p.base != nil {
			diffs++
			if p.depth > treeChain {
				t.Fatalf("%s: chain depth %d past %d", name, p.depth, treeChain)
			}
		}
	}
	if diffs < 50 {
		t.Fatalf("%d of %d trees kept as a set difference", diffs, len(names))
	}
	ascending := slices.Collect(func(yield func(int) bool) {
		for i := range names {
			if !yield(i) {
				return
			}
		}
	})
	descending := slices.Clone(ascending)
	slices.Reverse(descending)
	shuffled := slices.Clone(ascending)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	for _, order := range [][]int{ascending, descending, shuffled} {
		var d treeDecoder
		for _, i := range order {
			sameTree(t, x, &d, names[i], trees[i])
		}
	}
	for _, i := range shuffled {
		var d treeDecoder
		sameTree(t, x, &d, names[i], trees[i])
	}
}

// An extended index adds trees without changing what the one it extends
// answers.
func TestTreeIndexExtendLeavesBase(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	names, trees := treeIndexFixture(rng)
	half := len(names) / 2
	x := newTreeIndex()
	for i := range names[:half] {
		x.add(names[i], trees[i])
	}
	before := x.bytes
	y := x.clone()
	for i := half; i < len(names); i++ {
		y.add(names[i], trees[i])
	}
	if x.bytes != before || y.bytes <= before {
		t.Fatalf("bytes: base %d (was %d), extension %d", x.bytes, before, y.bytes)
	}
	var d treeDecoder
	for i, name := range names {
		if i >= half {
			if x.piece(name) != nil {
				t.Fatalf("%s: the base index holds a tree added to its extension", name)
			}
		} else {
			sameTree(t, x, &d, name, trees[i])
		}
		sameTree(t, y, &d, name, trees[i])
	}
}

func sameTree(t *testing.T, x *treeIndex, d *treeDecoder, name string, want *plainTree) {
	t.Helper()
	p := x.piece(name)
	if p == nil {
		t.Fatalf("%s: not indexed", name)
	}
	if p.rid != want.rid || p.size != want.size || x.names.name(p.b) != want.b {
		t.Fatalf("%s: rid %d size %d b %q, want %d %d %q", name, p.rid, p.size, x.names.name(p.b),
			want.rid, want.size, want.b)
	}
	s := make([]string, len(p.s))
	for i, id := range p.s {
		s[i] = x.names.name(id)
	}
	if !slices.Equal(s, want.s) {
		t.Fatalf("%s: S %v, want %v", name, s, want.s)
	}
	var got []string
	for _, id := range d.files(p) {
		got = append(got, x.names.name(id))
	}
	slices.Sort(got)
	files := slices.Compact(slices.Sorted(slices.Values(want.files)))
	if !slices.Equal(got, files) {
		t.Fatalf("%s: %d files decoded, want %d", name, len(got), len(files))
	}
}

// BenchmarkTreeIndexClone clones an index the size of fossil-scm.org's
// whole tree history, as a publish does.
func BenchmarkTreeIndexClone(b *testing.B) {
	x := newTreeIndex()
	for i := 0; i < 45000; i++ {
		x.intern(fmt.Sprintf("%040x", i))
	}
	for i := 0; i < 21000; i++ {
		x.add(fmt.Sprintf("m%039x", i), &plainTree{rid: libfossil.FslID(i + 1), checkin: true,
			files: []string{fmt.Sprintf("%040x", i), fmt.Sprintf("%040x", i+1)}})
	}
	b.ResetTimer()
	for b.Loop() {
		_ = x.clone()
	}
}
