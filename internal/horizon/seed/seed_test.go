package seed_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/simio"
)

// world is a dense origin: trunk c1..c4 with C = c3, and a branch feat
// forked at c1, below C, whose second check-in feat2 merges C in. feat2 is
// in K through its merge parent while its primary parent is not, so K's
// entries are c3 and feat2. A propagating hue is set on c1 below C, the
// non-propagating sym-v1 on C, and feat is closed by a propagating closed
// tag on feat1 below C, the latest tag event, at a fractional second.
type world struct {
	t                            *testing.T
	r                            *repo.Repo
	day                          int
	c1, c2, c3, c4, feat1, feat2 libfossil.FslID
	hueAt, v1At, closedAt        time.Time
}

func newRepo(t *testing.T, name string) *repo.Repo {
	t.Helper()
	r, err := repo.CreateWithEnv(t.TempDir()+"/"+name+".fossil", "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func branchTags(name string) []deck.TagCard {
	return []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: name},
		{Type: deck.TagPropagating, Name: "sym-" + name, UUID: "*"},
		{Type: deck.TagCancel, Name: "sym-trunk", UUID: "*"},
	}
}

func (w *world) at(day int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day)
}

func (w *world) commit(name string, parents []libfossil.FslID, tags ...deck.TagCard) libfossil.FslID {
	w.t.Helper()
	w.day++
	opts := manifest.CheckinOpts{
		Files:   []manifest.File{{Name: name + ".txt", Content: []byte(name + "\n")}},
		Comment: name, User: "test", Time: w.at(w.day), Tags: tags,
	}
	if len(parents) > 0 {
		opts.Parent, opts.MergeParents = parents[0], parents[1:]
	}
	rid, _, err := manifest.Checkin(w.r, opts)
	if err != nil {
		w.t.Fatalf("Checkin %s: %v", name, err)
	}
	return rid
}

func (w *world) tag(target libfossil.FslID, name string, typ int, value string, when time.Time) {
	w.t.Helper()
	_, err := tag.AddTag(w.r, tag.TagOpts{TargetRID: target, TagName: name, TagType: typ, Value: value, User: "test", Time: when})
	if err != nil {
		w.t.Fatalf("AddTag %s: %v", name, err)
	}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, r: newRepo(t, "dense")}
	w.c1 = w.commit("c1", nil)
	w.feat1 = w.commit("feat1", []libfossil.FslID{w.c1}, branchTags("feat")...)
	w.c2 = w.commit("c2", []libfossil.FslID{w.c1})
	w.hueAt = w.at(10)
	w.tag(w.c1, "hue", tag.TagPropagating, "red", w.hueAt)
	w.c3 = w.commit("c3", []libfossil.FslID{w.c2})
	w.v1At = w.at(11)
	w.tag(w.c3, "sym-v1", tag.TagSingleton, "", w.v1At)
	w.c4 = w.commit("c4", []libfossil.FslID{w.c3})
	w.feat2 = w.commit("feat2", []libfossil.FslID{w.feat1, w.c3})
	w.closedAt = w.at(12).Add(250 * time.Millisecond)
	w.tag(w.feat1, "closed", tag.TagPropagating, "", w.closedAt)
	return w
}

func (w *world) k(c libfossil.FslID) []libfossil.FslID {
	w.t.Helper()
	k, err := horizon.HeldSet(context.Background(), w.r.DB(), c, cone.RuleDescendants)
	if err != nil {
		w.t.Fatal(err)
	}
	return k
}

func uuidOf(t *testing.T, q db.Querier, rid libfossil.FslID) string {
	t.Helper()
	var u string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&u); err != nil {
		t.Fatal(err)
	}
	return u
}

func ridOf(t *testing.T, q db.Querier, uuid string) libfossil.FslID {
	t.Helper()
	var rid libfossil.FslID
	if err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		t.Fatalf("rid of %s: %v", uuid, err)
	}
	return rid
}

// tagState is the tag state in force at a check-in: every row of a positive
// type, as name, type and value, sorted.
func tagState(t *testing.T, q db.Querier, uuid string) []string {
	t.Helper()
	rows, err := q.Query(`SELECT tag.tagname, tagxref.tagtype, coalesce(tagxref.value, '')
		FROM tagxref JOIN tag USING(tagid) JOIN blob ON blob.rid=tagxref.rid
		WHERE blob.uuid=? AND tagxref.tagtype>0`, uuid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, value string
		var typ int
		if err := rows.Scan(&name, &typ, &value); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s/%d=%s", name, typ, value))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func isLeaf(t *testing.T, q db.Querier, uuid string) bool {
	t.Helper()
	var n int
	if err := q.QueryRow("SELECT count(*) FROM leaf JOIN blob USING(rid) WHERE blob.uuid=?", uuid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// derivesDense reports the first check-in of K whose tag state or leaf
// verdict in clone differs from the dense origin's.
func (w *world) derivesDense(clone *repo.Repo, k []libfossil.FslID) error {
	w.t.Helper()
	for _, rid := range k {
		u := uuidOf(w.t, w.r.DB(), rid)
		want, got := tagState(w.t, w.r.DB(), u), tagState(w.t, clone.DB(), u)
		if !slices.Equal(got, want) {
			return fmt.Errorf("check-in %.10s: tags %v, dense %v", u, got, want)
		}
		if g, d := isLeaf(w.t, clone.DB(), u), isLeaf(w.t, w.r.DB(), u); g != d {
			return fmt.Errorf("check-in %.10s: leaf %v, dense %v", u, g, d)
		}
	}
	return nil
}

// lacking copies every artifact of the origin into a fresh store but the
// check-in manifests outside k: a store that holds K, every control
// artifact, and none of C's ancestry. Its store cone is c3's structural
// horizon, the one the seeds w.mint mints at c3 are bound to.
func (w *world) lacking(k []libfossil.FslID) *repo.Repo {
	w.t.Helper()
	clone := newRepo(w.t, "clone")
	w.setHorizon(clone, w.c3, cone.RuleDescendants)
	rows, err := w.r.DB().Query(`SELECT rid FROM blob WHERE size>=0
		AND (rid NOT IN (SELECT objid FROM event WHERE type='ci') OR rid IN (SELECT value FROM json_each(?)))
		ORDER BY rid`, jsonRids(k))
	if err != nil {
		w.t.Fatal(err)
	}
	var rids []libfossil.FslID
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			w.t.Fatal(err)
		}
		rids = append(rids, rid)
	}
	rows.Close()
	w.copyInto(clone, rids...)
	return clone
}

// setHorizon makes r's store cone the structural horizon at c by rule.
func (w *world) setHorizon(r *repo.Repo, c libfossil.FslID, rule cone.HorizonRule) {
	w.t.Helper()
	h, err := cone.NewHorizon(uuidOf(w.t, w.r.DB(), c), rule, cone.DepthStructural)
	if err != nil {
		w.t.Fatal(err)
	}
	sc, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), sc); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) copyInto(clone *repo.Repo, rids ...libfossil.FslID) {
	w.t.Helper()
	err := clone.WithTx(func(tx *db.Tx) error {
		for _, rid := range rids {
			data, err := content.Expand(w.r.DB(), rid)
			if err != nil {
				return err
			}
			if _, _, err := blob.Store(tx, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := manifest.Crosslink(clone); err != nil {
		w.t.Fatal(err)
	}
}

func jsonRids(rids []libfossil.FslID) string {
	parts := make([]string, len(rids))
	for i, r := range rids {
		parts[i] = fmt.Sprint(r)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (w *world) mint(c libfossil.FslID, login string) (string, []byte) {
	w.t.Helper()
	name, data, err := seed.Mint(w.r.DB(), c, cone.RuleDescendants, w.k(c), login)
	if err != nil {
		w.t.Fatalf("Mint: %v", err)
	}
	return name, data
}

func hold(t *testing.T, r *repo.Repo, name string, data []byte, v seed.Verifier) error {
	t.Helper()
	return r.WithTx(func(tx *db.Tx) error { return seed.Hold(tx, name, data, v) })
}

func apply(t *testing.T, r *repo.Repo, v seed.Verifier) bool {
	t.Helper()
	var applied bool
	err := r.WithTx(func(tx *db.Tx) error {
		var err error
		applied, err = seed.Apply(tx, v)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

func TestEntries(t *testing.T) {
	w := newWorld(t)
	got, err := seed.Entries(w.r.DB(), w.c3, w.k(w.c3))
	if err != nil {
		t.Fatal(err)
	}
	if want := []libfossil.FslID{w.c3, w.feat2}; !slices.Equal(got, want) {
		t.Errorf("entries at c3 = %v, want %v (c3, feat2)", got, want)
	}
	// From c4 only c4 is held: C is its only entry.
	got, err = seed.Entries(w.r.DB(), w.c4, w.k(w.c4))
	if err != nil {
		t.Fatal(err)
	}
	if want := []libfossil.FslID{w.c4}; !slices.Equal(got, want) {
		t.Errorf("entries at c4 = %v, want %v", got, want)
	}
	// A root C names no parent and is still the seed's first entry.
	got, err = seed.Entries(w.r.DB(), w.c1, w.k(w.c1))
	if err != nil {
		t.Fatal(err)
	}
	if want := []libfossil.FslID{w.c1}; !slices.Equal(got, want) {
		t.Errorf("entries at root c1 = %v, want %v", got, want)
	}
}

func TestMintCardsCarryDenseState(t *testing.T) {
	w := newWorld(t)
	name, data := w.mint(w.c3, "origin")
	if name != hash.SHA3(data) {
		t.Errorf("name %s is not the SHA3-256 of the seed", name)
	}
	c3, c4, feat2 := uuidOf(t, w.r.DB(), w.c3), uuidOf(t, w.r.DB(), w.c4), uuidOf(t, w.r.DB(), w.feat2)
	text := string(data)
	for _, line := range []string{
		"T *branch " + c3 + " trunk\n",
		"T *hue " + c3 + " red\n",
		"T *sym-trunk " + c3 + "\n",
		"T +sym-v1 " + c3 + "\n",
		"T *branch " + feat2 + " feat\n",
		"T *closed " + feat2 + "\n",
		"T *sym-feat " + feat2 + "\n",
		// The latest event is closed's, at .250: D is the next whole second,
		// written without a fraction, as zit writes it.
		"D 2026-01-13T00:00:01\n",
		"U origin\n",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("seed lacks %q:\n%s", line, text)
		}
	}
	if strings.Contains(text, c4) {
		t.Errorf("seed names c4, whose state propagates from C:\n%s", text)
	}
	d, err := deck.Parse(data)
	if err != nil || d.Type != deck.Control {
		t.Fatalf("seed parses as %v, %v; want a control artifact", d, err)
	}
	// Every card but the binding card is exactly a row in force at its
	// target, and every such row has its card.
	cards := map[string][]string{}
	for _, tc := range d.T {
		if tc.Name == seed.BindingTag {
			continue
		}
		typ := map[deck.TagType]int{deck.TagPropagating: tag.TagPropagating, deck.TagSingleton: tag.TagSingleton}[tc.Type]
		cards[tc.UUID] = append(cards[tc.UUID], fmt.Sprintf("%s/%d=%s", tc.Name, typ, tc.Value))
	}
	for _, u := range []string{c3, feat2} {
		got := cards[u]
		slices.Sort(got)
		if want := tagState(t, w.r.DB(), u); !slices.Equal(got, want) {
			t.Errorf("cards at %.10s = %v, dense rows %v", u, got, want)
		}
	}
}

func TestMintDateIsTheLatestEvent(t *testing.T) {
	w := newWorld(t)
	// c4 holds only inherited rows, the latest of them hue's; D is the
	// latest tag event anywhere in the repository instead (§5.2.2): closed's,
	// on feat1, rounded up to the next whole second.
	_, data := w.mint(w.c4, "origin")
	if want := "D 2026-01-13T00:00:01\n"; !strings.Contains(string(data), want) {
		t.Errorf("seed at c4 lacks %q:\n%s", want, data)
	}
	// Minting twice over unchanged state yields the same seed.
	name1, _ := w.mint(w.c3, "origin")
	name2, _ := w.mint(w.c3, "origin")
	if name1 != name2 {
		t.Errorf("two mints over one state differ: %s, %s", name1, name2)
	}
	// A later tag event moves D past it.
	w.tag(w.c3, "sym-v2", tag.TagSingleton, "", w.at(20))
	_, data = w.mint(w.c3, "origin")
	if want := "D 2026-01-21T00:00:00\n"; !strings.Contains(string(data), want) {
		t.Errorf("re-minted seed lacks %q:\n%s", want, data)
	}
}

func TestMintRefusesWhatIsNotK(t *testing.T) {
	w := newWorld(t)
	if _, _, err := seed.Mint(w.r.DB(), w.c3, cone.RuleDescendants, []libfossil.FslID{w.c4}, "origin"); err == nil {
		t.Error("Mint accepted a K without C")
	}
}

func TestHoldDerivesDenseState(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	if w.derivesDense(clone, k) == nil {
		t.Fatal("the lacking store derives dense state without a seed: the test proves nothing")
	}
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.derivesDense(clone, k); err != nil {
		t.Error(err)
	}
	held, err := seed.Held(clone.DB())
	if err != nil || string(held) != string(data) {
		t.Errorf("Held = %q, %v; want the seed", held, err)
	}
	// Applying again changes nothing.
	if !apply(t, clone, nil) {
		t.Error("Apply reports no seed held")
	}
	if err := w.derivesDense(clone, k); err != nil {
		t.Errorf("after re-applying: %v", err)
	}
}

func TestHoldBeforeEntryArrives(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	feat2 := uuidOf(t, w.r.DB(), w.feat2)
	var rest []libfossil.FslID
	for _, rid := range k {
		if rid != w.feat2 {
			rest = append(rest, rid)
		}
	}
	clone := w.lacking(rest)
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	// feat2 arrives after the seed: linking it and applying the held seed
	// again, as the sync layer does after each round, derives it densely.
	w.copyInto(clone, w.feat2)
	apply(t, clone, nil)
	if err := w.derivesDense(clone, k); err != nil {
		t.Errorf("entry %.10s linked after the seed: %v", feat2, err)
	}
}

// An entry stored but not yet linked when the seed applies is left to the
// crosslink: the seed's rows name their target as their source, and a row
// sourced from an unlinked artifact would read as that artifact's own link,
// so the sweep would never link it. A sync's receive path stores before it
// links, so this is the order a clone sees.
func TestApplySkipsAnUnlinkedEntry(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	var rest []libfossil.FslID
	for _, rid := range k {
		if rid != w.feat2 {
			rest = append(rest, rid)
		}
	}
	clone := w.lacking(rest)
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := content.Expand(w.r.DB(), w.feat2)
	if err != nil {
		t.Fatal(err)
	}
	if err := clone.WithTx(func(tx *db.Tx) error { _, _, err := blob.Store(tx, stored); return err }); err != nil {
		t.Fatal(err)
	}
	apply(t, clone, nil)
	if _, err := manifest.Crosslink(clone); err != nil {
		t.Fatal(err)
	}
	apply(t, clone, nil)
	feat2 := uuidOf(t, w.r.DB(), w.feat2)
	var linked int
	clone.DB().QueryRow("SELECT count(*) FROM event JOIN blob ON rid=objid WHERE uuid=?", feat2).Scan(&linked)
	if linked != 1 {
		t.Fatalf("entry %.10s stored before the seed applied was never linked", feat2)
	}
	if err := w.derivesDense(clone, k); err != nil {
		t.Error(err)
	}
}

func TestHeldSeedIsNeverSendable(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	counts := func() string {
		var s strings.Builder
		for _, table := range []string{"blob", "unsent", "unclustered", "private", "phantom", "delta"} {
			var n int
			if err := clone.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&s, "%s=%d ", table, n)
		}
		return s.String()
	}
	before := counts()
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	if after := counts(); after != before {
		t.Errorf("holding the seed changed the artifact store: %s, was %s", after, before)
	}
	var n int
	if err := clone.DB().QueryRow("SELECT count(*) FROM blob WHERE uuid=?", name).Scan(&n); err != nil || n != 0 {
		t.Errorf("the seed's name is in blob (%d, %v): a peer could ask for it", n, err)
	}
	// A rebuild re-marks every blob unsent; the seed is no blob, so it is
	// still never offered.
	if _, err := verify.Rebuild(clone); err != nil {
		t.Fatal(err)
	}
	if err := clone.DB().QueryRow("SELECT count(*) FROM blob WHERE uuid=?", name).Scan(&n); err != nil || n != 0 {
		t.Errorf("after rebuild the seed's name is in blob (%d, %v)", n, err)
	}
}

func TestRebuildAppliesTheHeldSeed(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := verify.Rebuild(clone); err != nil {
		t.Fatal(err)
	}
	if err := w.derivesDense(clone, k); err != nil {
		t.Errorf("after rebuild: %v", err)
	}
}

func TestReseedReplacesTheOldSeed(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	// hue is cancelled below the horizon and the branch below feat2 renamed;
	// the clone receives both control artifacts, then the new seed.
	w.tag(w.c1, "hue", tag.TagCancel, "", w.at(30))
	w.tag(w.feat1, "branch", tag.TagPropagating, "feature", w.at(31))
	var controls []libfossil.FslID
	rows, err := w.r.DB().Query("SELECT objid FROM event WHERE type='g' AND mtime>=julianday(?)", w.at(30).Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var rid libfossil.FslID
		rows.Scan(&rid)
		controls = append(controls, rid)
	}
	rows.Close()
	if len(controls) != 2 {
		t.Fatalf("found %d new control artifacts, want 2", len(controls))
	}
	w.copyInto(clone, controls...)
	name2, data2 := w.mint(w.c3, "origin")
	if name2 == name {
		t.Fatal("the state moved but the seed did not")
	}
	if err := hold(t, clone, name2, data2, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.derivesDense(clone, k); err != nil {
		t.Errorf("after re-seeding: %v", err)
	}
}

func TestDiscardRemovesTheSeedsState(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	bare := map[string][]string{}
	for _, rid := range k {
		u := uuidOf(t, w.r.DB(), rid)
		bare[u] = tagState(t, clone.DB(), u)
	}
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	if err := clone.WithTx(seed.Discard); err != nil {
		t.Fatal(err)
	}
	if held, err := seed.Held(clone.DB()); err != nil || held != nil {
		t.Errorf("Held after Discard = %q, %v", held, err)
	}
	for u, want := range bare {
		if got := tagState(t, clone.DB(), u); !slices.Equal(got, want) {
			t.Errorf("check-in %.10s after Discard: %v, without a seed %v", u, got, want)
		}
	}
}

func TestHoldRefusesWhatIsNoSeed(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	name, data := w.mint(w.c3, "origin")
	manifestBytes, err := content.Expand(w.r.DB(), w.c3)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what string
		name string
		data []byte
	}{
		{"bytes not hashing to the name", name, append([]byte("x"), data...)},
		{"a check-in manifest", hash.SHA3(manifestBytes), manifestBytes},
		{"unparsable bytes", hash.SHA3([]byte("junk\n")), []byte("junk\n")},
	} {
		if err := hold(t, clone, tc.name, tc.data, nil); !errors.Is(err, seed.ErrNotSeed) {
			t.Errorf("%s: Hold = %v, want ErrNotSeed", tc.what, err)
		}
	}
	if held, err := seed.Held(clone.DB()); err != nil || held != nil {
		t.Errorf("Held after refusals = %q, %v", held, err)
	}
}

func TestVerifierDiscardsAnUnattributedSeed(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	name, data := w.mint(w.c3, "origin")
	unsigned := errors.New("unsigned")
	reject := func([]byte) error { return unsigned }
	if err := hold(t, clone, name, data, reject); !errors.Is(err, seed.ErrProvenance) || !errors.Is(err, unsigned) {
		t.Errorf("Hold under a rejecting key = %v, want ErrProvenance wrapping the verifier's error", err)
	}
	if held, _ := seed.Held(clone.DB()); held != nil {
		t.Error("a seed the verifier rejected was held")
	}
	// A seed already held is verified again when it is applied, and
	// discarded when it fails, leaving the content horizon's derivation.
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	if apply(t, clone, reject) {
		t.Error("Apply under a rejecting key reports the seed applied")
	}
	if held, _ := seed.Held(clone.DB()); held != nil {
		t.Error("a held seed that failed verification was kept")
	}
	if w.derivesDense(clone, k) == nil {
		t.Error("the discarded seed's state survived")
	}
}

// An artifact of a seed's shape that carries neither this store's seed label
// nor a binding card is an ordinary control artifact (§5.2.3): stored,
// linked, offered to peers, and never held as the seed.
func TestSeedShapedArtifactIsAnOrdinaryControl(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	_, minted := w.mint(w.c3, "origin")
	name, data := reseal(t, minted, func(cards []deck.TagCard) []deck.TagCard {
		return slices.DeleteFunc(cards, func(c deck.TagCard) bool { return c.Name == seed.BindingTag })
	})
	var rid libfossil.FslID
	err := clone.WithTx(func(tx *db.Tx) error {
		var err error
		rid, _, err = blob.Store(tx, data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(clone); err != nil {
		t.Fatal(err)
	}
	if got := ridOf(t, clone.DB(), name); got != rid {
		t.Fatalf("stored under rid %d, named rid %d", rid, got)
	}
	var typ string
	if err := clone.DB().QueryRow("SELECT type FROM event WHERE objid=?", rid).Scan(&typ); err != nil || typ != "g" {
		t.Errorf("event type %q, %v; want the control artifact's g row", typ, err)
	}
	var srcRows int
	if err := clone.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", rid).Scan(&srcRows); err != nil || srcRows == 0 {
		t.Errorf("no tagxref row names the artifact as its source (%d, %v)", srcRows, err)
	}
	if held, err := seed.Held(clone.DB()); err != nil || held != nil {
		t.Errorf("an ordinary artifact was held as the seed: %q, %v", held, err)
	}
	var unsent int
	if err := clone.DB().QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid).Scan(&unsent); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if unsent != 1 {
		t.Error("the ordinary control artifact is not offered to peers like any other")
	}
}

// A seed that carries a binding card is never linked, whatever path stored
// it (§5.2.3): its tags never reach derived state.
func TestSeedWithBindingCardNotLinked(t *testing.T) {
	w := newWorld(t)
	clone := w.lacking(w.k(w.c3))
	_, data := w.mint(w.c3, "origin")
	var rid libfossil.FslID
	err := clone.WithTx(func(tx *db.Tx) error {
		var err error
		rid, _, err = blob.Store(tx, data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(clone); err != nil {
		t.Fatal(err)
	}
	var srcRows int
	if err := clone.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", rid).Scan(&srcRows); err != nil || srcRows != 0 {
		t.Errorf("the seed was linked: %d tag rows (%v)", srcRows, err)
	}
}

// reseal returns data with its T cards edited, as a control artifact again
// under its new name: a seed some origin could have minted.
func reseal(t *testing.T, data []byte, edit func([]deck.TagCard) []deck.TagCard) (string, []byte) {
	t.Helper()
	d, err := deck.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	out, err := (&deck.Deck{Type: deck.Control, D: d.D, T: edit(slices.Clone(d.T)), U: d.U}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return hash.SHA3(out), out
}

// bindings returns the seed's horizon-seed cards.
func bindings(t *testing.T, data []byte) []deck.TagCard {
	t.Helper()
	d, err := deck.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	var out []deck.TagCard
	for _, tc := range d.T {
		if tc.Name == seed.BindingTag {
			out = append(out, tc)
		}
	}
	return out
}

// TestMintCarriesTheBindingCard: every seed carries one binding card, a
// singleton horizon-seed tag on C whose value is the rule (§5.2.1).
func TestMintCarriesTheBindingCard(t *testing.T) {
	w := newWorld(t)
	c3 := uuidOf(t, w.r.DB(), w.c3)
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		_, data, err := seed.Mint(w.r.DB(), w.c3, rule, w.k(w.c3), "origin")
		if err != nil {
			t.Fatal(err)
		}
		if line := "T +horizon-seed " + c3 + " " + rule.String() + "\n"; !strings.Contains(string(data), line) {
			t.Errorf("%v seed lacks %q:\n%s", rule, line, data)
		}
		if got := bindings(t, data); len(got) != 1 {
			t.Errorf("%v seed carries %d binding cards, want 1", rule, len(got))
		}
	}
}

// TestMintNeverDerivesTheBindingTag: a horizon-seed tag in force at an entry
// is never derived into a seed, which carries its own binding card alone
// (§5.2.1); with no other tag in force there is nothing to seed.
func TestMintNeverDerivesTheBindingTag(t *testing.T) {
	w := newWorld(t)
	w.tag(w.c3, seed.BindingTag, tag.TagSingleton, "leaves", w.at(20))
	var rows int
	if err := w.r.DB().QueryRow(`SELECT count(*) FROM tagxref JOIN tag USING(tagid)
		WHERE tagname=?`, seed.BindingTag).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("fixture: %d horizon-seed rows (%v), want the tag in force", rows, err)
	}
	_, data := w.mint(w.c3, "origin")
	if got := bindings(t, data); len(got) != 1 || got[0].Value != "descendants" {
		t.Errorf("binding cards %+v, want the seed's own alone:\n%s", got, data)
	}
	// Its event is still a tag event in the repository, which dates D
	// (§5.2.2), though it is no card of the seed.
	if want := "D 2026-01-21T00:00:00\n"; !strings.Contains(string(data), want) {
		t.Errorf("seed lacks %q:\n%s", want, data)
	}
	if _, err := w.r.DB().Exec(`DELETE FROM tagxref WHERE tagid NOT IN
		(SELECT tagid FROM tag WHERE tagname=?)`, seed.BindingTag); err != nil {
		t.Fatal(err)
	}
	if _, _, err := seed.Mint(w.r.DB(), w.c3, cone.RuleDescendants, w.k(w.c3), "origin"); !errors.Is(err, seed.ErrNothingToSeed) {
		t.Errorf("Mint with only a horizon-seed tag in force = %v, want ErrNothingToSeed", err)
	}
}

// TestHoldRefusesAnUnboundSeed: a clone holds no seed whose binding card is
// missing, doubled, or names another C or rule than its store cone's, and
// none at all without a horizon; each is refused as a wrongly signed seed is
// (§5.2.4).
func TestHoldRefusesAnUnboundSeed(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	_, good := w.mint(w.c3, "origin")
	otherC, otherCData := w.mint(w.c4, "origin")
	leaves, leavesData, err := seed.Mint(w.r.DB(), w.c3, cone.RuleLeaves, k, "origin")
	if err != nil {
		t.Fatal(err)
	}
	// The extra card sorts before C's, so C's is the last a reader sees.
	below := strings.Repeat("0", 40)
	unbound, unboundData := reseal(t, good, func(cards []deck.TagCard) []deck.TagCard {
		return slices.DeleteFunc(cards, func(tc deck.TagCard) bool { return tc.Name == seed.BindingTag })
	})
	twice, twiceData := reseal(t, good, func(cards []deck.TagCard) []deck.TagCard {
		return append(cards, deck.TagCard{Type: deck.TagSingleton, Name: seed.BindingTag, UUID: below, Value: "descendants"})
	})
	for _, tc := range []struct {
		what string
		name string
		data []byte
	}{
		{"another C", otherC, otherCData},
		{"another rule", leaves, leavesData},
		{"no binding card", unbound, unboundData},
		{"two binding cards", twice, twiceData},
	} {
		err := hold(t, clone, tc.name, tc.data, nil)
		if !errors.Is(err, seed.ErrUnbound) || !errors.Is(err, seed.ErrProvenance) {
			t.Errorf("%s: Hold = %v, want ErrUnbound as ErrProvenance", tc.what, err)
		}
	}
	if held, err := seed.Held(clone.DB()); err != nil || held != nil {
		t.Errorf("Held after refusals = %q, %v", held, err)
	}
	dense := newRepo(t, "dense-store")
	if err := hold(t, dense, hash.SHA3(good), good, nil); !errors.Is(err, seed.ErrUnbound) {
		t.Errorf("a store with no horizon: Hold = %v, want ErrUnbound", err)
	}
}

// TestHoldRefusesAnOlderSeed: a seed dated before the one held is a replay
// and refused; one dated the same is accepted; and once the held seed is
// discarded, as a deepen discards it, the floor is gone (§5.2.4).
func TestHoldRefusesAnOlderSeed(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	oldName, oldData := w.mint(w.c3, "origin")
	w.tag(w.c3, "sym-v2", tag.TagSingleton, "", w.at(20))
	newName, newData := w.mint(w.c3, "origin")
	if err := hold(t, clone, newName, newData, nil); err != nil {
		t.Fatal(err)
	}
	err := hold(t, clone, oldName, oldData, nil)
	if !errors.Is(err, seed.ErrReplay) || !errors.Is(err, seed.ErrProvenance) {
		t.Errorf("Hold of an older seed = %v, want ErrReplay as ErrProvenance", err)
	}
	if held, _ := seed.Held(clone.DB()); string(held) != string(newData) {
		t.Errorf("the refused replay replaced the held seed:\n%s", held)
	}
	sameName, sameData := w.mint(w.c3, "another-origin")
	if sameName == newName {
		t.Fatal("two labels mint one seed: the equal-D case proves nothing")
	}
	if err := hold(t, clone, sameName, sameData, nil); err != nil {
		t.Errorf("Hold of a seed dated as the held one = %v, want it accepted", err)
	}
	if err := clone.WithTx(seed.Discard); err != nil {
		t.Fatal(err)
	}
	if err := hold(t, clone, oldName, oldData, nil); err != nil {
		t.Errorf("Hold of the older seed once none is held = %v, want it accepted", err)
	}
}

// TestApplyStripsTheBindingCard: the binding card is no tag in force, so a
// held seed applies every card but it (§5.2.1).
func TestApplyStripsTheBindingCard(t *testing.T) {
	w := newWorld(t)
	clone := w.lacking(w.k(w.c3))
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := clone.DB().QueryRow(`SELECT count(*) FROM tagxref JOIN tag USING(tagid)
		WHERE tagname=?`, seed.BindingTag).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d horizon-seed rows derived (%v), want none", n, err)
	}
}

// TestApplyDiscardsAnUnboundHeldSeed: a held seed no longer bound to the
// store's horizon, one held before seeds carried a binding card or swapped
// on disk, is discarded when applied, as one failing its signature is.
func TestApplyDiscardsAnUnboundHeldSeed(t *testing.T) {
	w := newWorld(t)
	k := w.k(w.c3)
	clone := w.lacking(k)
	name, data := w.mint(w.c3, "origin")
	if err := hold(t, clone, name, data, nil); err != nil {
		t.Fatal(err)
	}
	_, legacy := reseal(t, data, func(cards []deck.TagCard) []deck.TagCard {
		return slices.DeleteFunc(cards, func(tc deck.TagCard) bool { return tc.Name == seed.BindingTag })
	})
	if _, err := clone.DB().Exec("UPDATE horizonseed SET seed=?", legacy); err != nil {
		t.Fatal(err)
	}
	if apply(t, clone, nil) {
		t.Error("Apply reports an unbound held seed applied")
	}
	if held, _ := seed.Held(clone.DB()); held != nil {
		t.Error("an unbound held seed was kept")
	}
	if w.derivesDense(clone, k) == nil {
		t.Error("the discarded seed's state survived")
	}
}

// A cancel, here and at an ancestor, is a tag event: D never moves earlier
// when a newer tag is cancelled (§5.2.2), where dated from the rows in force
// it fell back to the row before.
// TestMintDateKeepsTheMintedFloor removes the latest tag event's rows, as a
// shun and purge does, and mints again: the seed keeps the D already minted
// (§5.2.2), and the same seed after the repository is closed and reopened,
// and a later event still moves it on.
func TestMintDateKeepsTheMintedFloor(t *testing.T) {
	w := newWorld(t)
	w.tag(w.c3, "late", tag.TagSingleton, "", w.at(300))
	_, before := w.mint(w.c3, "origin")
	if _, err := w.r.DB().Exec(`DELETE FROM tagxref WHERE tagid=(SELECT tagid FROM tag WHERE tagname='late')`); err != nil {
		t.Fatal(err)
	}
	const floor = "D 2026-10-28T00:00:00\n"
	if !strings.HasPrefix(string(before), floor) {
		t.Fatalf("the first seed is not dated %q:\n%s", floor, before)
	}
	_, purged := w.mint(w.c3, "origin")
	if !strings.HasPrefix(string(purged), floor) {
		t.Fatalf("after the purge the seed is not dated %q:\n%s", floor, purged)
	}
	path := w.r.Path()
	if err := w.r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w.r = r
	t.Cleanup(func() { r.Close() })
	_, reopened := w.mint(w.c3, "origin")
	if string(reopened) != string(purged) {
		t.Fatalf("after reopening the seed changed:\n%s\nwant\n%s", reopened, purged)
	}
	w.tag(w.c3, "later", tag.TagSingleton, "", w.at(301))
	_, later := w.mint(w.c3, "origin")
	if want := "D 2026-10-29T00:00:00\n"; !strings.HasPrefix(string(later), want) {
		t.Errorf("a later event left the seed without %q:\n%s", want, later)
	}
}

// TestMintFloorIsBestEffort mints through a read-only handle, which cannot
// keep the mark: the seed is still served, dated from the tag rows. It then
// replaces a stored floor too large to read, which a cast would rank above
// every D and keep for good.
func TestMintFloorIsBestEffort(t *testing.T) {
	w := newWorld(t)
	w.tag(w.c3, "late", tag.TagSingleton, "", w.at(300))
	k := w.k(w.c3)
	path := w.r.Path()
	if err := w.r.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := db.OpenSQL(path, db.OpenConfig{}, map[string]string{"mode": "ro"})
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := seed.Mint(ro, w.c3, cone.RuleDescendants, k, "origin")
	ro.Close()
	if err != nil {
		t.Fatalf("Mint on a read-only handle: %v", err)
	}
	if want := "D 2026-10-28T00:00:00\n"; !strings.HasPrefix(string(data), want) {
		t.Fatalf("the read-only seed is not dated %q:\n%s", want, data)
	}
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w.r = r
	t.Cleanup(func() { r.Close() })
	if _, err := r.DB().Exec(`INSERT INTO config(name, value) VALUES('horizon-seed-floor', '99999999999999999999')`); err != nil {
		t.Fatal(err)
	}
	_, data = w.mint(w.c3, "origin")
	if want := "D 2026-10-28T00:00:00\n"; !strings.HasPrefix(string(data), want) {
		t.Fatalf("over an unreadable floor the seed is not dated %q:\n%s", want, data)
	}
	var floor string
	if err := r.DB().QueryRow(`SELECT value FROM config WHERE name='horizon-seed-floor'`).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	if want := "1793145600000"; floor != want {
		t.Errorf("the unreadable floor became %q, want %q", floor, want)
	}
}

func TestMintDateCountsCancels(t *testing.T) {
	for _, target := range []string{"C", "ancestor"} {
		w := newWorld(t)
		on := w.c3
		if target == "ancestor" {
			on = w.c1
		}
		w.tag(on, "tint", tag.TagPropagating, "", w.at(30))
		_, before := w.mint(w.c3, "origin")
		w.tag(on, "tint", tag.TagCancel, "", w.at(31))
		_, after := w.mint(w.c3, "origin")
		if want := "D 2026-01-31T00:00:00\n"; !strings.Contains(string(before), want) {
			t.Fatalf("%s: before the cancel the seed lacks %q:\n%s", target, want, before)
		}
		if want := "D 2026-02-01T00:00:00\n"; !strings.Contains(string(after), want) {
			t.Errorf("%s: after the cancel the seed lacks %q:\n%s", target, want, after)
		}
	}
}
