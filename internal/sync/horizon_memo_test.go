package sync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// memoWorld is an origin and the history a client writes for it. Artifacts
// are made ahead of the pushes that deliver them, so a push can carry any
// of them in any order: a child before its parent, a delta manifest before
// its baseline, a tree before its nodes, a blob after the manifest naming
// it. An announcement makes a phantom of an artifact before it arrives.
type memoWorld struct {
	t        *testing.T
	r        *repo.Repo
	rng      *rand.Rand
	when     time.Time
	checkins []string                     // every check-in made, pushed or not
	trees    map[string]map[string]string // each check-in's tree, name -> blob
	form     map[string]string            // "full", "delta" over a baseline, "tree" or "docs"
	baseline map[string]string            // a delta manifest's baseline
	pending  map[string][]byte            // made and not yet pushed
	version  int
}

var memoFiles = []string{"doc/r.txt", "lib/x.c", "lib/y.c", "src/a.c", "src/b.c"}

func newMemoWorld(t *testing.T, seed int64) *memoWorld {
	t.Helper()
	return &memoWorld{t: t, r: setupPushableTestRepo(t), rng: rand.New(rand.NewSource(seed)),
		when: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), trees: map[string]map[string]string{},
		form: map[string]string{}, baseline: map[string]string{}, pending: map[string][]byte{}}
}

func (w *memoWorld) make(data []byte) string {
	uuid := hash.SHA1(data)
	if w.pending[uuid] == nil {
		w.pending[uuid] = data
	}
	return uuid
}

func (w *memoWorld) marshal(d *deck.Deck) string {
	w.t.Helper()
	data, err := d.Marshal()
	if err != nil {
		w.t.Fatal(err)
	}
	return w.make(data)
}

// commit makes a check-in on random parents, changing one or two files, in
// a random manifest form.
func (w *memoWorld) commit() {
	w.t.Helper()
	tree := map[string]string{}
	var parents []string
	if len(w.checkins) > 0 && w.rng.Intn(8) != 0 {
		parents = append(parents, w.checkins[w.rng.Intn(len(w.checkins))])
		maps.Copy(tree, w.trees[parents[0]])
		if len(w.checkins) > 2 && w.rng.Intn(5) == 0 {
			if merge := w.checkins[w.rng.Intn(len(w.checkins))]; merge != parents[0] {
				parents = append(parents, merge)
			}
		}
	}
	for _, name := range memoFiles {
		if tree[name] == "" {
			tree[name] = w.make([]byte(name + " initial\n"))
		}
	}
	for i := 0; i <= w.rng.Intn(2); i++ {
		w.version++
		name := memoFiles[w.rng.Intn(len(memoFiles))]
		tree[name] = w.make([]byte(fmt.Sprintf("%s v%d\n", name, w.version)))
	}
	w.when = w.when.Add(time.Hour)
	d := &deck.Deck{Type: deck.Checkin, C: fmt.Sprintf("c%d", len(w.checkins)), D: w.when, U: deck.User("u"), P: parents}
	form := []string{"full", "delta", "tree", "docs"}[w.rng.Intn(4)]
	base := ""
	if len(parents) > 0 {
		switch w.form[parents[0]] {
		case "full":
			base = parents[0]
		case "delta":
			base = w.baseline[parents[0]]
		}
	}
	if form == "delta" && base == "" {
		form = "full"
	}
	switch form {
	case "full":
		d.F = fileCards(tree, func(string) bool { return true })
	case "delta":
		d.B = base
		d.F = fileCards(tree, func(name string) bool { return w.trees[base][name] != tree[name] })
	case "docs":
		// The doc/ node named last: its slot runs to the end of the tree and
		// may reach src, so a doc/ node another tree's slot bounded at lib/
		// withheld is kept, older than the tree that keeps it (§6.2.6).
		for name := range tree {
			if !strings.HasPrefix(name, "doc/") {
				delete(tree, name)
			}
		}
		node := &deck.Deck{F: fileCards(tree, func(name string) bool { return strings.HasPrefix(name, "doc/") })}
		d.S = []deck.SubmanifestCard{{Name: node.F[0].Name, UUID: w.marshal(node)}}
	case "tree":
		for _, dir := range []string{"doc/", "lib/", "src/"} {
			node := &deck.Deck{F: fileCards(tree, func(name string) bool { return strings.HasPrefix(name, dir) })}
			d.S = append(d.S, deck.SubmanifestCard{Name: node.F[0].Name, UUID: w.marshal(node)})
		}
	}
	uuid := w.marshal(d)
	w.checkins = append(w.checkins, uuid)
	w.trees[uuid], w.form[uuid] = tree, form
	if form == "delta" {
		w.baseline[uuid] = base
	}
}

func fileCards(tree map[string]string, keep func(string) bool) []deck.FileCard {
	var out []deck.FileCard
	for _, name := range memoFiles {
		if keep(name) {
			out = append(out, deck.FileCard{Name: name, UUID: tree[name]})
		}
	}
	return out
}

func (w *memoWorld) pendingNames() []string {
	names := slices.Sorted(maps.Keys(w.pending))
	w.rng.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	return names
}

func (w *memoWorld) ask(cards ...xfer.Card) *xfer.Message {
	w.t.Helper()
	resp, err := HandleSync(context.Background(), w.r, &xfer.Message{Cards: cards})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, e := range findCards[*xfer.ErrorCard](resp) {
		w.t.Fatalf("origin error: %s", e.Message)
	}
	return resp
}

// push delivers a random part of what is pending, in random order.
func (w *memoWorld) push() {
	names := w.pendingNames()
	cards := []xfer.Card{&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}}
	for _, uuid := range names[:w.rng.Intn(len(names))+1] {
		cards = append(cards, &xfer.FileCard{UUID: uuid, Content: w.pending[uuid]})
		delete(w.pending, uuid)
	}
	w.ask(cards...)
}

// announce makes phantoms at the origin of pending artifacts.
func (w *memoWorld) announce() {
	names := w.pendingNames()
	cards := []xfer.Card{&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}}
	for _, uuid := range names[:w.rng.Intn(len(names))+1] {
		cards = append(cards, &xfer.IGotCard{UUID: uuid})
	}
	w.ask(cards...)
}

// cones are a path cone and the horizons of c, both rules, both depths,
// with and without the path cone.
func (w *memoWorld) cones(c string) []*cone.Cone {
	w.t.Helper()
	paths, err := cone.New([]string{"src"}, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	out := []*cone.Cone{paths}
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		for _, depth := range []cone.HorizonDepth{cone.DepthContent, cone.DepthStructural} {
			h, err := cone.NewHorizon(c, rule, depth)
			if err != nil {
				w.t.Fatal(err)
			}
			for _, include := range [][]string{nil, {"src"}} {
				hc, err := cone.NewWithHorizon(include, nil, h)
				if err != nil {
					w.t.Fatal(err)
				}
				out = append(out, hc)
			}
		}
	}
	return out
}

// pull asks the origin for c's horizon, which classifies through the
// origin's memo and publishes it, and returns how many artifacts the
// classification read.
func (w *memoWorld) pull(c *cone.Cone) int {
	w.t.Helper()
	reads := &memoReads{}
	cards := []xfer.Card{&xfer.PullCard{ServerCode: "s", ProjectCode: testPC}}
	_, err := HandleSyncWithOpts(context.Background(), w.r,
		&xfer.Message{Cards: append(cards, conePragmas(c, noSeed)...)}, HandleOpts{Observer: reads})
	if err != nil {
		w.t.Fatal(err)
	}
	return reads.classified[0]
}

// sameKeep fails unless the origin's memo keeps, for c, exactly what a
// fresh classification of the store keeps: every artifact, and the held
// set and reach of c's horizon.
func sameKeep(t *testing.T, r *repo.Repo, v *content.MemoView, c *cone.Cone) (joined int) {
	t.Helper()
	ctx := context.Background()
	q := r.DB()
	fresh, err := buildKeep(ctx, q, c, freshClassifier{})
	if err != nil {
		t.Fatal(err)
	}
	memo, err := buildKeep(ctx, q, c, memoClassifier{v})
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := c.Horizon(); ok {
		fk, freach, fplaced, err := freshClassifier{}.reach(ctx, q, h)
		if err != nil {
			t.Fatal(err)
		}
		mk, mreach, mplaced, err := memoClassifier{v}.reach(ctx, q, h)
		if err != nil {
			t.Fatal(err)
		}
		if fplaced != mplaced || !slices.Equal(fk, mk) || !maps.Equal(freach, mreach) {
			t.Fatalf("%s: fresh K=%v placed=%v reach=%d, memo K=%v placed=%v reach=%d",
				c, fk, fplaced, len(freach), mk, mplaced, len(mreach))
		}
	}
	rids, err := ridSetOf(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, rid := range rids {
		if fresh.keeps(rid) != memo.keeps(rid) {
			t.Fatalf("%s rid=%d: fresh keeps=%v, memo keeps=%v", c, rid, fresh.keeps(rid), memo.keeps(rid))
		}
	}
	if !fresh.slots.Equal(memo.slots) {
		t.Fatalf("%s: the memo's slot walk differs from a fresh one", c)
	}
	for cursor := int64(0); len(rids) > 0 && cursor <= int64(rids[len(rids)-1]); cursor++ {
		fv, ferr := fresh.voids(cursor)
		mv, merr := memo.voids(cursor)
		if ferr != nil || merr != nil {
			t.Fatalf("%s cursor=%d: fresh voids %v, memo voids %v", c, cursor, ferr, merr)
		}
		if fv != mv {
			t.Fatalf("%s cursor=%d: fresh voids=%v, memo voids=%v", c, cursor, fv, mv)
		}
		if fresh.joined != nil {
			j, err := fresh.joined(cursor)
			if err != nil {
				t.Fatal(err)
			}
			if j {
				joined++
			}
		}
	}
	return joined
}

// buildKeep is the keep set an origin's inventory applies for c under cl,
// with a structural horizon's reach when the store places it.
func buildKeep(ctx context.Context, q db.Querier, c *cone.Cone, cl classifier) (*sparseKeep, error) {
	k, err := newSparseKeepWith(ctx, q, c, cl)
	if err != nil {
		return nil, err
	}
	h, ok := c.Horizon()
	if !ok || h.Depth() != cone.DepthStructural {
		return k, k.withSlots(ctx, cl, c, nil)
	}
	_, reach, placed, err := cl.reach(ctx, q, h)
	if err != nil || !placed {
		return k, err
	}
	s := &structuralServe{reach: reach}
	if err := k.withStructure(q, s); err != nil {
		return nil, err
	}
	return k, k.withSlots(ctx, cl, c, s)
}

func ridSetOf(q db.Querier) ([]libfossil.FslID, error) {
	rows, err := q.Query("SELECT rid FROM blob ORDER BY rid")
	if err != nil {
		return nil, err
	}
	var out []libfossil.FslID
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, rid)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// memoReads records what each request's classification read.
type memoReads struct {
	nopObserver
	classified []int
}

func (o *memoReads) HandleCompleted(_ context.Context, end HandleEnd) {
	o.classified = append(o.classified, end.SparseClassified)
}

// draft-fossil-horizon-00 §6.2: after any sequence of commits, pushes in any
// order and announcements, the origin's memoized classification equals a
// fresh one, for path cones, content and structural horizons, both rules;
// and the memo extended rather than re-read for most of the run.
func TestHorizonMemoEqualsFresh(t *testing.T) {
	// §6.2.6: some world must move a leaves-rule meeting point under a
	// cursor, or the void it causes goes unexercised.
	joinedAfter := map[int64]int{}
	t.Cleanup(func() {
		total := 0
		for _, n := range joinedAfter {
			total += n
		}
		if total == 0 && !t.Failed() {
			t.Error("no world voided a cursor by a check-in joining the held set")
		}
	})
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			ctx := context.Background()
			w := newMemoWorld(t, seed)
			memo := originMemo(w.r.DB())
			if memo == nil {
				t.Fatal("an origin over a pool DB has no memo")
			}
			extended, full, joined := 0, 0, 0
			for step := 0; step < 40; step++ {
				switch n := w.rng.Intn(10); {
				case n < 4 || len(w.pending) == 0:
					w.commit()
				case n < 8:
					w.push()
				default:
					w.announce()
				}
				if len(w.checkins) == 0 {
					continue
				}
				cones := w.cones(w.checkins[w.rng.Intn(len(w.checkins))])
				var names int
				if err := w.r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&names); err != nil {
					t.Fatal(err)
				}
				read := 0
				if w.rng.Intn(2) == 0 {
					read = w.pull(cones[w.rng.Intn(len(cones))])
				}
				// Unpublished: the next view extends from what the pulls left.
				v, err := memo.View(ctx, w.r.DB())
				if err != nil {
					t.Fatal(err)
				}
				read += v.Read()
				switch {
				case read >= names && names > 0:
					full++
				case read > 0:
					extended++
				}
				for _, c := range cones {
					joined += sameKeep(t, w.r, v, c)
				}
			}
			if extended == 0 {
				t.Fatalf("the memo never extended (%d full reads)", full)
			}
			joinedAfter[seed] = joined
		})
	}
}

// pushInOrder delivers everything pending, blobs and nodes before the
// manifests that name them, parents before children.
func (w *memoWorld) pushInOrder() {
	w.t.Helper()
	cards := []xfer.Card{&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}}
	var manifests []string
	for _, uuid := range slices.Sorted(maps.Keys(w.pending)) {
		if w.trees[uuid] != nil {
			manifests = append(manifests, uuid)
			continue
		}
		cards = append(cards, &xfer.FileCard{UUID: uuid, Content: w.pending[uuid]})
	}
	for _, uuid := range w.checkins {
		if slices.Contains(manifests, uuid) {
			cards = append(cards, &xfer.FileCard{UUID: uuid, Content: w.pending[uuid]})
		}
	}
	clear(w.pending)
	w.ask(cards...)
}

// draft-fossil-horizon-00 §6.2: while artifacts arrive in order, every pull
// after the first classifies only what was appended since the last one, and
// a pull over an unchanged store reads nothing.
func TestHorizonMemoInOrderReadsAppended(t *testing.T) {
	w := newMemoWorld(t, 42)
	for i := 0; i < 6; i++ {
		w.commit()
	}
	w.pushInOrder()
	cones := w.cones(w.checkins[2])
	w.pull(cones[0])
	for i := 0; i < 10; i++ {
		before := maxRIDOf(t, w.r)
		w.commit()
		w.pushInOrder()
		appended := int(maxRIDOf(t, w.r) - before)
		c := cones[w.rng.Intn(len(cones))]
		if read := w.pull(c); read != appended {
			t.Fatalf("pull %d of %s read %d artifacts, want the %d appended", i, c, read, appended)
		}
		if read := w.pull(c); read != 0 {
			t.Fatalf("a repeated pull read %d artifacts", read)
		}
	}
}

// A blob that arrives after the manifest naming it is classified alone; a
// parent that arrives after its child, which a held P card cited while it
// was absent, sends the classification back to a full read (§6.2).
func TestHorizonMemoLateArrivals(t *testing.T) {
	w := newMemoWorld(t, 7)
	w.commit()
	w.pushInOrder()
	cones := w.cones(w.checkins[0])
	c := cones[1]
	w.pull(c)
	parent := w.checkins[0]
	w.version++
	blob := w.make([]byte("late blob\n"))
	tree := maps.Clone(w.trees[parent])
	tree["src/a.c"] = blob
	w.when = w.when.Add(time.Hour)
	child := w.marshal(&deck.Deck{Type: deck.Checkin, C: "late", D: w.when, U: deck.User("u"), P: []string{parent},
		F: fileCards(tree, func(string) bool { return true })})
	w.ask(&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}, &xfer.FileCard{UUID: child, Content: w.pending[child]})
	if read := w.pull(c); read != 2 {
		t.Fatalf("the manifest's arrival read %d artifacts, want it and its blob's phantom", read)
	}
	w.ask(&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}, &xfer.FileCard{UUID: blob, Content: w.pending[blob]})
	if read := w.pull(c); read != 1 {
		t.Fatalf("the late blob read %d artifacts, want 1", read)
	}
	late, err := originMemo(w.r.DB()).View(context.Background(), w.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cones {
		sameKeep(t, w.r, late, c)
	}

	grand := w.marshal(&deck.Deck{Type: deck.Checkin, C: "orphan", D: w.when.Add(2 * time.Hour), U: deck.User("u"),
		P: []string{w.marshal(&deck.Deck{Type: deck.Checkin, C: "absent", D: w.when.Add(time.Hour), U: deck.User("u"),
			P: []string{child}, F: fileCards(tree, func(string) bool { return true })})},
		F: fileCards(tree, func(string) bool { return true })})
	w.ask(&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}, &xfer.FileCard{UUID: grand, Content: w.pending[grand]})
	w.pull(c)
	var absent string
	for uuid := range w.pending {
		if uuid != grand && uuid != blob && uuid != child {
			absent = uuid
		}
	}
	w.ask(&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}, &xfer.FileCard{UUID: absent, Content: w.pending[absent]})
	var names int
	if err := w.r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&names); err != nil {
		t.Fatal(err)
	}
	if read := w.pull(c); read != names {
		t.Fatalf("the cited parent's arrival read %d artifacts, want the store's %d", read, names)
	}
	v, err := originMemo(w.r.DB()).View(context.Background(), w.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cones {
		sameKeep(t, w.r, v, c)
	}
}

func maxRIDOf(t *testing.T, r *repo.Repo) libfossil.FslID {
	t.Helper()
	var n libfossil.FslID
	if err := r.DB().QueryRow("SELECT max(rid) FROM blob").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
