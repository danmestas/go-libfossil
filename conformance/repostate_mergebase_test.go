package conformance

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/merge"
)

// The merge base is fossil's pivot (pivot.c): the most recent common
// ancestor by event time, found by expanding the newest pending check-in
// first. Each case names a scripted graph and the base fossil's merge-base
// gives; the answer is checked against fossil(1) and go-libfossil's
// merge.FindPivot alike. TestMergeBaseFossilOracle compares the two over
// generated graphs.

// mergeBaseQuery is one `fossil merge-base` question: a primary, its
// secondaries, and the label of the answer, "" for none.
type mergeBaseQuery struct {
	primary      string
	secondaries  []string
	ignoreMerges bool
	want         string
}

func mergeBaseAPICases() []rsAPICase {
	// a1 is one edge from p and s, b3 newer: the pivot is b3.
	skewed := []rsArt{
		checkin("r", 1, nil, rsRoot...),
		checkin("a1", 2, of("r")),
		checkin("b3", 5, of("r")),
		checkin("b4", 6, of("b3")),
		checkin("p", 7, of("a1", "b4")),
		checkin("s", 8, of("b3", "a1")),
	}
	crissCross := func(order ...string) []rsArt {
		at := map[string]rsArt{"a1": checkin("a1", 3, of("r")), "b1": checkin("b1", 3, of("r"))}
		return []rsArt{checkin("r", 1, nil, rsRoot...), at[order[0]], at[order[1]],
			checkin("a2", 3, of("a1", "b1")), checkin("b2", 3, of("b1", "a1"))}
	}
	// m1 merges b into a; m2 merges c, a's child, into m1; m3 merges m1
	// into d, b's child.
	mergesOfMerges := []rsArt{
		checkin("r", 1, nil, rsRoot...),
		checkin("a", 2, of("r")),
		checkin("b", 3, of("r")),
		checkin("m1", 4, of("a", "b")),
		checkin("c", 5, of("a")),
		checkin("m2", 6, of("m1", "c")),
		checkin("d", 7, of("b")),
		checkin("m3", 8, of("d", "m1")),
	}
	return []rsAPICase{
		mergeBases("repostate.3.2.merge-base-newest", skewed,
			mergeBaseQuery{primary: "p", secondaries: []string{"s"}, want: "b3"},
			mergeBaseQuery{primary: "s", secondaries: []string{"p"}, want: "b3"},
			mergeBaseQuery{primary: "p", secondaries: []string{"s"}, ignoreMerges: true, want: "r"}),
		// Every check-in at one second: the higher rid is examined first.
		mergeBases("repostate.3.2.merge-base-equal-times", crissCross("a1", "b1"),
			mergeBaseQuery{primary: "a2", secondaries: []string{"b2"}, want: "b1"},
			mergeBaseQuery{primary: "a2", secondaries: []string{"b2"}, ignoreMerges: true, want: "r"}),
		mergeBases("repostate.3.2.merge-base-equal-times-rid", crissCross("b1", "a1"),
			mergeBaseQuery{primary: "a2", secondaries: []string{"b2"}, want: "a1"}),
		mergeBases("repostate.3.2.merge-base-merges-of-merges", mergesOfMerges,
			mergeBaseQuery{primary: "m2", secondaries: []string{"m3"}, want: "m1"},
			mergeBaseQuery{primary: "m3", secondaries: []string{"c"}, want: "a"},
			mergeBaseQuery{primary: "d", secondaries: []string{"c", "m1"}, want: "b"}),
		{Case: Case{ID: "repostate.3.2.merge-newest-base", Req: "RS-3.2-4"}, arts: mergeContentScript(),
			run: mergeAgainstPivot},
	}
}

// mergeBases checks each query's answer with fossil(1) and go-libfossil.
func mergeBases(id string, arts []rsArt, queries ...mergeBaseQuery) rsAPICase {
	return rsAPICase{Case: Case{ID: id, Req: "RS-3.2-4"}, arts: arts, run: func(e *rsEnv) error {
		r, err := e.open()
		if err != nil {
			return err
		}
		for _, q := range queries {
			if e.ref != nil {
				got, err := e.fossilMergeBase(q)
				if err != nil {
					return err
				}
				if got != q.want {
					return expectf("fossil merge-base %+v = %q; the case expects %q", q, got, q.want)
				}
			}
			got, err := e.goMergeBase(r, q)
			if err != nil {
				return err
			}
			if got != q.want {
				return expectf("FindPivot %+v = %q, want %q", q, got, q.want)
			}
		}
		return nil
	}}
}

// fossilMergeBase is the label of fossil's answer to q.
func (e *rsEnv) fossilMergeBase(q mergeBaseQuery) (string, error) {
	args := []string{"merge-base"}
	if q.ignoreMerges {
		args = append(args, "--ignore-merges")
	}
	for _, label := range append([]string{q.primary}, q.secondaries...) {
		args = append(args, e.script.hashes[label])
	}
	out, err := e.fossilRun(args...)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "No common ancestor found." {
		return "", nil
	}
	if !strings.HasPrefix(out, "pivot=") {
		return "", fmt.Errorf("fossil merge-base: unexpected output %q", out)
	}
	return labelOf(strings.TrimPrefix(out, "pivot="), e.script.labels), nil
}

// goMergeBase is the label of go-libfossil's answer to q.
func (e *rsEnv) goMergeBase(r *gofossil.Repo, q mergeBaseQuery) (string, error) {
	query := merge.PivotQuery{IgnoreMerges: q.ignoreMerges}
	for i, label := range append([]string{q.primary}, q.secondaries...) {
		rid, err := e.goRid(r.Inner(), label)
		if err != nil {
			return "", fmt.Errorf("rid of %s: %w", label, err)
		}
		if i == 0 {
			query.Primary = rid
		} else {
			query.Secondaries = append(query.Secondaries, rid)
		}
	}
	p, err := merge.FindPivot(context.Background(), r.Inner(), query)
	if errors.Is(err, merge.ErrNoCommonAncestor) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return e.labelOfRid(r.Inner(), int64(p.RID)), nil
}

// mergeContentScript is the skewed criss-cross of merge-base-newest with a
// file whose merge differs by base: s undoes b3's change to line 3, which
// against b3 is s's edit and against a1 no edit at all.
func mergeContentScript() []rsArt {
	files := map[string]string{
		"r": spaced("1", "2", "3", "4"), "a1": spaced("1a", "2", "3", "4"), "b3": spaced("1", "2", "3b", "4"),
		"b4": spaced("1", "2", "3b", "4b"), "p": spaced("1a", "2", "3b", "4b"), "s": spaced("1a", "2s", "3", "4"),
	}
	withFile := func(label string, s int, parents []string, tags ...string) rsArt {
		art := checkin(label, s, parents, tags...)
		head, rest, _ := strings.Cut(art.body, "\nD ")
		date, rest, _ := strings.Cut(rest, "\n")
		art.body = head + "\nD " + date + "\nF f.txt {f-" + label + "}\n" + rest
		return art
	}
	labels := []string{"r", "a1", "b3", "b4", "p", "s"}
	var arts []rsArt
	for _, l := range labels {
		arts = append(arts, file("f-"+l, files[l]))
	}
	return append(arts,
		withFile("r", 1, nil, rsRoot...),
		withFile("a1", 2, of("r")),
		withFile("b3", 5, of("r")),
		withFile("b4", 6, of("b3")),
		withFile("p", 7, of("a1", "b4")),
		withFile("s", 8, of("b3", "a1")))
}

// spaced is a file of the given lines with two unchanging lines between
// each, so changes to neighbouring lines merge apart.
func spaced(lines ...string) string {
	return strings.Join(lines, "\nx\nx\n") + "\n"
}

// mergeAgainstPivot merges p into a checkout of s with fossil's merge and
// go-libfossil's: both diff against b3 and write the same bytes, which a
// merge against a1, the base a breadth-first walk finds, would not.
func mergeAgainstPivot(e *rsEnv) error {
	want := spaced("1a", "2s", "3", "4b")
	if e.ref != nil {
		if _, err := e.fossilRun("update", e.script.hashes["s"]); err != nil {
			return err
		}
		if _, err := e.fossilRun("merge", e.script.hashes["p"]); err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(e.fossil.checkout, "f.txt"))
		if err != nil {
			return err
		}
		if string(got) != want {
			return expectf("fossil merge wrote %q; the case expects %q", got, want)
		}
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	dir := filepath.Join(e.t.TempDir(), "co")
	co, err := r.CreateCheckout(dir, gofossil.CheckoutCreateOpts{})
	if err != nil {
		return err
	}
	defer co.Close()
	s, err := e.goRid(r.Inner(), "s")
	if err != nil {
		return err
	}
	p, err := e.goRid(r.Inner(), "p")
	if err != nil {
		return err
	}
	if err := co.Extract(int64(s), gofossil.ExtractOpts{Force: true}); err != nil {
		return err
	}
	res, err := co.Merge(gofossil.CheckoutMergeOpts{Version: int64(p)})
	if err != nil {
		return err
	}
	if got := e.labelOfRid(r.Inner(), res.Ancestor); got != "b3" {
		return expectf("Merge diffed against %s, want b3", got)
	}
	got, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		return err
	}
	if string(got) != want {
		return expectf("Merge wrote %q, want %q", got, want)
	}
	return nil
}

// TestMergeBaseFossilOracle builds graphs at random, with equal and skewed
// times, merges and merges of merges, and asks fossil's merge-base and
// merge.FindPivot the same questions.
func TestMergeBaseFossilOracle(t *testing.T) {
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("needs fossil(1)")
	}
	const graphs, nodes = 6, 16
	compared := 0
	for seed := int64(1); seed <= graphs; seed++ {
		rng := rand.New(rand.NewSource(seed))
		arts := randomGraph(rng, nodes)
		e, err := newEnv(t, ref, arts)
		if err != nil {
			t.Fatalf("graph %d: %v", seed, err)
		}
		r, err := e.open()
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range randomQueries(rng, nodes) {
			want, err := e.fossilMergeBase(q)
			if err != nil {
				t.Fatalf("graph %d: %v", seed, err)
			}
			got, err := e.goMergeBase(r, q)
			if err != nil {
				t.Fatalf("graph %d: %v", seed, err)
			}
			if got != want {
				t.Errorf("graph %d %+v: FindPivot = %q, fossil merge-base = %q", seed, q, got, want)
			}
			compared++
		}
	}
	t.Logf("%d queries over %d graphs agree with fossil merge-base", compared, graphs)
}

// randomGraph is n check-ins, n0 the root, each other one with one to three
// distinct earlier parents, at seconds drawn from 1 to 12, so equal times
// and children older than their parents are common.
func randomGraph(rng *rand.Rand, n int) []rsArt {
	arts := []rsArt{checkin("n0", 1+rng.Intn(12), nil, rsRoot...)}
	for i := 1; i < n; i++ {
		picked := map[int]bool{}
		var parents []string
		for k := 1 + rng.Intn(3); k > 0; k-- {
			p := rng.Intn(i)
			if !picked[p] {
				picked[p] = true
				parents = append(parents, fmt.Sprintf("n%d", p))
			}
		}
		arts = append(arts, checkin(fmt.Sprintf("n%d", i), 1+rng.Intn(12), of(parents...)))
	}
	return arts
}

// randomQueries asks about pairs of the later check-ins, both ways, with and
// without merges, and about a few check-ins with two secondaries.
func randomQueries(rng *rand.Rand, n int) []mergeBaseQuery {
	var out []mergeBaseQuery
	for k := 0; k < 8; k++ {
		a, b := n/2+rng.Intn(n-n/2), n/2+rng.Intn(n-n/2)
		for _, ignore := range []bool{false, true} {
			out = append(out, mergeBaseQuery{primary: fmt.Sprintf("n%d", a),
				secondaries: []string{fmt.Sprintf("n%d", b)}, ignoreMerges: ignore})
		}
	}
	for k := 0; k < 3; k++ {
		picks := []int{rng.Intn(n), rng.Intn(n), rng.Intn(n)}
		sort.Ints(picks[1:])
		out = append(out, mergeBaseQuery{primary: fmt.Sprintf("n%d", picks[0]),
			secondaries: []string{fmt.Sprintf("n%d", picks[1]), fmt.Sprintf("n%d", picks[2])}})
	}
	return out
}
