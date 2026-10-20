package manifest

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// These tests pin the writer of draft-fossil-submanifests-00 §6 against the
// binding vectors of Appendix A: the partition predicate, the scan with its
// group limits and final-group merge, and the artifacts the construction
// emits, named under the sha3 policy as the vectors are.

// sha3Name names an artifact the way a sha3 repository does, storing nothing.
func sha3Name(data []byte) (string, error) { return hash.SHA3(data), nil }

// a1Files is the Appendix A.1 file set.
func a1Files() []deck.FileCard {
	return []deck.FileCard{
		{Name: "README.md", UUID: hash.SHA3([]byte("# Example\n"))},
		{Name: "src/main.c", UUID: hash.SHA3([]byte("int main(void){return 0;}\n"))},
		{Name: "src/node11.c", UUID: hash.SHA3([]byte("/* node11 */\n"))},
		{Name: "src/util.c", UUID: hash.SHA3([]byte("/* util */\n"))},
	}
}

// a4Files is the Appendix A.4 file set: fNNNN.c holding "/* NNNN */\n".
func a4Files() []deck.FileCard {
	var out []deck.FileCard
	for _, n := range []string{"1257", "1346", "1523", "2146", "2260", "2468", "2614", "2954", "3514"} {
		out = append(out, deck.FileCard{Name: "f" + n + ".c", UUID: hash.SHA3([]byte("/* " + n + " */\n"))})
	}
	return out
}

func mustBuildTree(t *testing.T, files []deck.FileCard, k int) Tree {
	t.Helper()
	tree, err := BuildTree(files, k, sha3Name)
	if err != nil {
		t.Fatalf("BuildTree(k=%d): %v", k, err)
	}
	return tree
}

// §6.1: zbits is the count of leading zero bits of SHA3-256(name), Tables 3
// and 4.
func TestZeroBitsMatchesAppendixTables(t *testing.T) {
	want := map[string]int{
		"README.md": 1, "src/main.c": 0, "src/node11.c": 8, "src/util.c": 0,
		"f1257.c": 0, "f1346.c": 1, "f1523.c": 3, "f2146.c": 1, "f2260.c": 1,
		"f2468.c": 1, "f2614.c": 9, "f2954.c": 0, "f3514.c": 0,
	}
	for name, bits := range want {
		if got := zeroBits(name); got != bits {
			t.Errorf("zeroBits(%q) = %d, want %d", name, got, bits)
		}
	}
}

// Appendix A.2 and A.3: at k = 3 the example yields two leaves whose S cards
// the manifest carries directly.
func TestBuildTreeReproducesAppendixA3(t *testing.T) {
	tree := mustBuildTree(t, a1Files(), 3)
	want := []deck.SubmanifestCard{
		{Name: "README.md", UUID: "aa9d404c14fe4dadd2ab398961fb0dfe650a877c061f650d8233a5888e920acf"},
		{Name: "src/node11.c", UUID: "a50a722b465ed67b42ee0fbe61e4a19d6b1d22940ba5740e99e450921e3f52ea"},
	}
	if !reflect.DeepEqual(tree.S, want) {
		t.Fatalf("S run %+v, want %+v", tree.S, want)
	}
	if len(tree.Subs) != 2 || len(tree.Subs[0]) != 190 || len(tree.Subs[1]) != 193 {
		t.Fatalf("leaves of %d artifacts, want the 190- and 193-byte leaves", len(tree.Subs))
	}
}

// Appendix A.1: at k = 4, MIN = 4 suppresses the only boundary, so the
// construction is flat.
func TestBuildTreeFlatWhenMinSuppressesBoundary(t *testing.T) {
	if tree := mustBuildTree(t, a1Files(), 4); len(tree.S) != 0 || len(tree.Subs) != 0 {
		t.Fatalf("k=4 built %d S cards, want flat", len(tree.S))
	}
}

// Appendix A.4: the final-group merge applies at height 1 as at height 0, so
// the manifest carries the three leaves directly.
func TestBuildTreeReproducesAppendixA4(t *testing.T) {
	tree := mustBuildTree(t, a4Files(), 3)
	want := []string{
		"b8e53d0c46cb57b616cc0ed6de5ce2453715be6157b73d247bcd5de8a6510aaa",
		"cff23e5ba71569397c1d816afa2dd0bfffc42339e402df36b63623846bd5fcbe",
		"9604c3f9774f7472d7bff44d5965f30250358a3e7a18046eeaf5f9d83bd5cdc6",
	}
	var got []string
	for _, s := range tree.S {
		got = append(got, s.UUID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("S run %q, want %q", got, want)
	}
}

// §6.2 step 2: fewer than two groups is flat, for N = 0 and N = 1 alike.
func TestBuildTreeFlatBelowTwoGroups(t *testing.T) {
	for _, files := range [][]deck.FileCard{nil, a1Files()[:1]} {
		for k := MinPartitionK; k <= MaxPartitionK; k++ {
			if tree := mustBuildTree(t, files, k); len(tree.S) != 0 {
				t.Fatalf("N=%d k=%d built a tree", len(files), k)
			}
		}
	}
}

// §6.2 step 1 on synthetic zero-bit counts: MAX forces a boundary, MIN
// suppresses one, and a short final group merges into its predecessor
// only when the two fit in MAX.
func TestPartitionGroups(t *testing.T) {
	cases := []struct {
		zbits               []int
		threshold, min, max int
		want                []int
	}{
		{make([]int, 20), 3, 2, 8, []int{0, 8, 16, 20}},             // forced at MAX
		{[]int{0, 9, 0, 0}, 3, 2, 8, []int{0, 4}},                   // MIN suppresses
		{[]int{0, 0, 9, 0}, 3, 2, 8, []int{0, 2, 4}},                // natural boundary
		{[]int{0, 0, 0, 9}, 3, 2, 8, []int{0, 4}},                   // final group of 1 merges
		{[]int{0, 0, 0, 0, 0, 0, 0, 9, 0}, 3, 2, 8, []int{0, 7, 9}}, // a late natural boundary
		{make([]int, 9), 3, 2, 8, []int{0, 8, 9}},                   // merge would exceed MAX
		{nil, 3, 2, 8, nil},
	}
	for i, c := range cases {
		if got := partitionGroups(c.zbits, c.threshold, c.min, c.max); !reflect.DeepEqual(got, c.want) {
			t.Errorf("case %d: bounds %v, want %v", i, got, c.want)
		}
	}
}

// §6.2 step 3: a non-reducing scan and a single-group scan both stop the
// construction, and the height cap stops it whatever the scan yields.
func TestTreePlanStops(t *testing.T) {
	// k = 1: MIN 0, MAX 8. Sixteen entries all at zbits 1 split into
	// sixteen leaves; at height 1 (threshold 2) no key qualifies, so the
	// scan forms groups of MAX: two interiors, then one group -- stop.
	zbits := make([]int, 16)
	for i := range zbits {
		zbits[i] = 1
	}
	plan := treePlan(zbits, 1, maxConstructHeight)
	if len(plan) != 2 || len(plan[0]) != 17 || !reflect.DeepEqual(plan[1], []int{0, 8, 16}) {
		t.Fatalf("plan %v", plan)
	}
	// Every key qualifies at every height: each scan is non-reducing.
	for i := range zbits {
		zbits[i] = 200
	}
	if plan := treePlan(zbits, 1, maxConstructHeight); len(plan) != 1 {
		t.Fatalf("non-reducing scan kept %d levels", len(plan))
	}
	// The cap: with cap 1 only the leaves are planned.
	for i := range zbits {
		zbits[i] = 1
	}
	if plan := treePlan(zbits, 1, 1); len(plan) != 1 {
		t.Fatalf("cap 1 kept %d levels", len(plan))
	}
}

// §6.2: a three-level tree stores leaves first, then interiors, so nothing
// is stored before what it references; interior S cards name their members.
func TestBuildTreeStorageOrderAndInteriors(t *testing.T) {
	var files []deck.FileCard
	for i := 0; i < 400; i++ {
		name := fmt.Sprintf("d%02d/f%03d.c", i%17, i)
		files = append(files, deck.FileCard{Name: name, UUID: hash.SHA3([]byte(name))})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	var named []string
	tree, err := BuildTree(files, 1, func(data []byte) (string, error) {
		d, err := deck.Parse(data)
		if err != nil {
			return "", err
		}
		for _, s := range d.S {
			if !contains(named, s.UUID) {
				return "", fmt.Errorf("interior names %s before it is stored", s.UUID)
			}
		}
		named = append(named, hash.SHA3(data))
		return hash.SHA3(data), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	interiors := 0
	for _, sub := range tree.Subs {
		if sub[0] == 'S' {
			interiors++
		}
	}
	if interiors == 0 || len(tree.S) < 2 {
		t.Fatalf("%d interiors, %d S cards: want a three-level tree", interiors, len(tree.S))
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// §6.1: the partition reads filenames only, so the same names under either
// hash policy, any content, permission or old name, partition identically.
func TestPartitionIgnoresAllButNames(t *testing.T) {
	a := a4Files()
	b := a4Files()
	for i := range b {
		b[i].UUID = hash.SHA1([]byte(b[i].Name))
		b[i].Perm = "x"
	}
	ta, tb := mustBuildTree(t, a, 3), mustBuildTree(t, b, 3)
	if len(ta.S) != len(tb.S) {
		t.Fatalf("%d and %d groups", len(ta.S), len(tb.S))
	}
	for i := range ta.S {
		if ta.S[i].Name != tb.S[i].Name {
			t.Fatalf("group %d keyed %q and %q", i, ta.S[i].Name, tb.S[i].Name)
		}
	}
}

// §6.3: C(k) over the Appendix A.1 set. k = 1, 2 and 3 partition it the
// same way -- leaves of 190 and 193 bytes, S cards of 77 and 80 -- so they
// tie, and the lowest k wins; from k = 4 nothing is a candidate.
func TestPartitionCostAndSelection(t *testing.T) {
	costs := partitionCosts(a1Files())
	want := 157 + float64(190*190+193*193)/float64(190+193)
	for k := 1; k <= 3; k++ {
		if !costs[k].candidate || costs[k].total != want {
			t.Fatalf("C(%d) = %+v, want %v", k, costs[k], want)
		}
	}
	for k := 4; k <= MaxPartitionK; k++ {
		if costs[k].candidate {
			t.Fatalf("k=%d is a candidate", k)
		}
	}
	if k, ok := SelectPartitionK(a1Files()); !ok || k != 1 {
		t.Fatalf("SelectPartitionK = %d, %v; want 1 by the tie-break", k, ok)
	}
	// Appendix A.5: one name has no candidate at any k.
	if k, ok := SelectPartitionK(a1Files()[:1]); ok {
		t.Fatalf("SelectPartitionK over one name = %d", k)
	}
}

// §6.4's gate: the candidate replaces the pin only when distinct and better
// by more than the margin, and never when the pin is no candidate.
func TestRetuneGate(t *testing.T) {
	costs := func(totals map[int]float64) [MaxPartitionK + 1]partitionCost {
		var out [MaxPartitionK + 1]partitionCost
		for k, c := range totals {
			out[k] = partitionCost{candidate: true, total: c}
		}
		return out
	}
	cases := []struct {
		totals map[int]float64
		pinned int
		want   int
	}{
		{map[int]float64{2: 100, 3: 80}, 2, 3}, // 20% better: re-tune
		{map[int]float64{2: 100, 3: 85}, 2, 0}, // exactly the margin: keep
		{map[int]float64{2: 100, 3: 90}, 2, 0}, // within the margin: keep
		{map[int]float64{3: 80}, 2, 0},         // pin is no candidate: keep
		{map[int]float64{2: 80, 3: 80}, 3, 0},  // a tie gains nothing
		{map[int]float64{}, 2, 0},              // no candidate at all
		{map[int]float64{2: 100, 5: 10}, 5, 0}, // already the optimum
	}
	for i, c := range cases {
		if got, ok := retuneTarget(costs(c.totals), c.pinned); got != c.want || ok != (c.want != 0) {
			t.Errorf("case %d: retuneTarget = %d, %v; want %d", i, got, ok, c.want)
		}
	}
}
