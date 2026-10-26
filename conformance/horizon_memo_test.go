package conformance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// The origin's classification memo (draft-fossil-horizon-00 §6.2): the
// handler reports what each request's classification read from the store
// (HandleEnd.SparseClassified), and every reply's announcement is compared
// with the one a second handle on the same store, whose memo starts empty,
// gives: a fresh classification.

var (
	horizonMemoExtended = Case{ID: "horizon.6.2.classification-extended", Req: "HZ-6.2-9"}
	horizonMemoGraph    = Case{ID: "horizon.6.2.reference-graph", Req: "HZ-6.2-10"}
)

func horizonMemoCases() []Case { return []Case{horizonMemoExtended, horizonMemoGraph} }

func TestHorizonMemo(t *testing.T) {
	check(t, horizonMemoExtended, func(t *testing.T) error { return hzMemoExtended(t) })
	check(t, horizonMemoGraph, func(t *testing.T) error { return hzMemoGraph(t) })
}

// hzMemoOrigin is a pushable origin with a check-in chain written in order.
type hzMemoOrigin struct {
	t    *testing.T
	r    *repo.Repo
	path string
	code string
	when time.Time
	tips []libfossil.FslID
}

func newHZMemoOrigin(t *testing.T, checkins int) (*hzMemoOrigin, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "origin.fossil")
	r, err := repo.Create(path, "u", simio.CryptoRand{}, "")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { r.Close() })
	if err := auth.UpdateCaps(r.DB(), "nobody", "gijoryz"); err != nil {
		return nil, err
	}
	code, err := r.Config("project-code")
	if err != nil {
		return nil, err
	}
	o := &hzMemoOrigin{t: t, r: r, path: path, code: code, when: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	for i := 0; i < checkins; i++ {
		if err := o.commit(); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// commit checks in on the last check-in, changing one file.
func (o *hzMemoOrigin) commit() error {
	o.when = o.when.Add(time.Hour)
	var parent libfossil.FslID
	if len(o.tips) > 0 {
		parent = o.tips[len(o.tips)-1]
	}
	rid, _, err := manifest.Checkin(o.r, manifest.CheckinOpts{
		Files: []manifest.File{{Name: "src/a.c", Content: []byte(o.when.String())},
			{Name: "doc/r.txt", Content: []byte("r\n")}},
		Comment: "c", User: "u", Parent: parent, Time: o.when,
	})
	o.tips = append(o.tips, rid)
	return err
}

func (o *hzMemoOrigin) uuid(rid libfossil.FslID) (string, error) {
	var uuid string
	err := o.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid)
	return uuid, err
}

// pull asks r for c's horizon, announcing nothing and carrying no cursor,
// and returns the names the reply announces and what the request's
// classification read.
func (o *hzMemoOrigin) pull(r *repo.Repo, c *cone.Cone) (map[string]bool, int, error) {
	cards := []xfer.Card{&xfer.PragmaCard{Name: "client-version", Values: []string{"22800"}},
		&xfer.PullCard{ServerCode: "x", ProjectCode: o.code}}
	for _, m := range c.Include() {
		cards = append(cards, &xfer.PragmaCard{Name: "cone", Values: []string{m}})
	}
	h, _ := c.Horizon()
	cards = append(cards, &xfer.PragmaCard{Name: "horizon", Values: []string{h.Hash(), h.Rule().String()}})
	if h.Depth() == cone.DepthStructural {
		cards = append(cards, &xfer.PragmaCard{Name: "horizon-structural"},
			&xfer.PragmaCard{Name: "horizon-seed", Values: []string{"-"}})
	}
	req, err := scSyncWire(&xfer.Message{Cards: cards})
	if err != nil {
		return nil, 0, err
	}
	work := &scHandleWork{}
	resp, err := sync.HandleSyncWithOpts(context.Background(), r, req, sync.HandleOpts{Observer: work})
	if err != nil {
		return nil, 0, err
	}
	return hzIGots(resp), work.classified[0], nil
}

// pullFresh is pull against a second handle on the origin's store, whose
// memo starts empty, and also checks that it read the whole store.
func (o *hzMemoOrigin) pullFresh(c *cone.Cone) (map[string]bool, error) {
	r, err := repo.Open(o.path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	got, read, err := o.pull(r, c)
	if err != nil {
		return nil, err
	}
	if n, err := o.artifacts(); err != nil || read != n {
		return nil, expectf("a fresh handle's classification read %d of %d artifacts (%v)", read, n, err)
	}
	return got, nil
}

func (o *hzMemoOrigin) artifacts() (int, error) {
	var n int
	err := o.r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&n)
	return n, err
}

// expect pulls c and fails unless the classification read want artifacts
// and the reply announces what a fresh classification announces.
func (o *hzMemoOrigin) expect(what string, c *cone.Cone, want int) error {
	got, read, err := o.pull(o.r, c)
	if err != nil {
		return err
	}
	if read != want {
		return expectf("%s: the classification read %d artifacts, want %d", what, read, want)
	}
	fresh, err := o.pullFresh(c)
	if err != nil {
		return err
	}
	return hzSameNames(what, got, fresh)
}

// push delivers artifacts to the origin.
func (o *hzMemoOrigin) push(artifacts ...[]byte) error {
	cards := []xfer.Card{&xfer.PushCard{ServerCode: "x", ProjectCode: o.code}}
	for _, data := range artifacts {
		cards = append(cards, &xfer.FileCard{UUID: hash.SHA1(data), Content: data})
	}
	req, err := scSyncWire(&xfer.Message{Cards: cards})
	if err != nil {
		return err
	}
	resp, err := sync.HandleSync(context.Background(), o.r, req)
	if err != nil {
		return err
	}
	for _, card := range resp.Cards {
		if e, ok := card.(*xfer.ErrorCard); ok {
			return expectf("push refused: %s", e.Message)
		}
	}
	return nil
}

func hzMemoCone(hash string, rule cone.HorizonRule, depth cone.HorizonDepth, include ...string) (*cone.Cone, error) {
	h, err := cone.NewHorizon(hash, rule, depth)
	if err != nil {
		return nil, err
	}
	return cone.NewWithHorizon(include, nil, h)
}

func hzMemoCheckin(comment string, when time.Time, parent string, files map[string]string) ([]byte, error) {
	d := &deck.Deck{Type: deck.Checkin, C: comment, D: when, U: deck.User("u"), P: []string{parent}}
	for _, name := range []string{"doc/r.txt", "src/a.c"} {
		d.F = append(d.F, deck.FileCard{Name: name, UUID: files[name]})
	}
	return d.Marshal()
}

// HZ-6.2-9: the classification is kept by cone and store prefix and
// extended by what is appended: a pull over an unchanged store reads
// nothing, one after a commit reads what it appended, a blob arriving after
// the manifest that names it is classified alone, and a parent arriving
// after the child whose P card cited it sends the classification back to
// a full read. Every reply announces what a fresh classification does.
func hzMemoExtended(t *testing.T) error {
	o, err := newHZMemoOrigin(t, 4)
	if err != nil {
		return err
	}
	c2, err := o.uuid(o.tips[1])
	if err != nil {
		return err
	}
	c, err := hzMemoCone(c2, cone.RuleDescendants, cone.DepthContent)
	if err != nil {
		return err
	}
	n, err := o.artifacts()
	if err != nil {
		return err
	}
	if err := o.expect("first pull", c, n); err != nil {
		return err
	}
	if err := o.expect("unchanged store", c, 0); err != nil {
		return err
	}
	if err := o.commit(); err != nil {
		return err
	}
	appended, err := o.artifacts()
	if err != nil {
		return err
	}
	if err := o.expect("after a commit", c, appended-n); err != nil {
		return err
	}
	tip, err := o.uuid(o.tips[len(o.tips)-1])
	if err != nil {
		return err
	}
	blob := []byte("late blob\n")
	files := map[string]string{"doc/r.txt": hash.SHA1([]byte("r\n")), "src/a.c": hash.SHA1(blob)}
	child, err := hzMemoCheckin("late", o.when.Add(time.Hour), tip, files)
	if err != nil {
		return err
	}
	if err := o.push(child); err != nil {
		return err
	}
	// The manifest, and a phantom for each version it names that the store
	// lacks: its blob's is filled below.
	pushed, err := o.artifacts()
	if err != nil {
		return err
	}
	if err := o.expect("a manifest ahead of its blob", c, pushed-appended); err != nil {
		return err
	}
	if err := o.push(blob); err != nil {
		return err
	}
	if err := o.expect("the late blob", c, 1); err != nil {
		return err
	}
	parent, err := hzMemoCheckin("absent", o.when.Add(2*time.Hour), hash.SHA1(child), files)
	if err != nil {
		return err
	}
	orphan, err := hzMemoCheckin("orphan", o.when.Add(3*time.Hour), hash.SHA1(parent), files)
	if err != nil {
		return err
	}
	if err := o.push(orphan); err != nil {
		return err
	}
	orphaned, err := o.artifacts()
	if err != nil {
		return err
	}
	if err := o.expect("a child ahead of its parent", c, orphaned-pushed); err != nil {
		return err
	}
	if err := o.push(parent); err != nil {
		return err
	}
	all, err := o.artifacts()
	if err != nil {
		return err
	}
	return o.expect("the cited parent's arrival", c, all)
}

// HZ-6.2-10: the references are read once per store, and a cone the origin
// has never classified, at either depth and either rule, with or without
// paths, is classified over them without reading them again. The F cards
// of a horizon's trees are not among them: the first walk to need a tree
// reads it from the store into the memo's bounded tree cache, which later
// horizons reuse (internal/content TestMemoTreesReadOnce), and later
// classifications of one horizon extend its walk.
func hzMemoGraph(t *testing.T) error {
	o, err := newHZMemoOrigin(t, 5)
	if err != nil {
		return err
	}
	c2, err := o.uuid(o.tips[1])
	if err != nil {
		return err
	}
	c3, err := o.uuid(o.tips[2])
	if err != nil {
		return err
	}
	first, err := hzMemoCone(c2, cone.RuleDescendants, cone.DepthContent)
	if err != nil {
		return err
	}
	n, err := o.artifacts()
	if err != nil {
		return err
	}
	if err := o.expect("first cone", first, n); err != nil {
		return err
	}
	for _, spec := range []struct {
		hash  string
		rule  cone.HorizonRule
		depth cone.HorizonDepth
		paths []string
	}{
		{c3, cone.RuleLeaves, cone.DepthContent, nil},
		{c3, cone.RuleDescendants, cone.DepthStructural, nil},
		{c2, cone.RuleLeaves, cone.DepthStructural, []string{"src"}},
	} {
		c, err := hzMemoCone(spec.hash, spec.rule, spec.depth, spec.paths...)
		if err != nil {
			return err
		}
		if err := o.expect("a cone never classified: "+c.String(), c, 0); err != nil {
			return err
		}
	}
	return nil
}
