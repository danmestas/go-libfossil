package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The horizon cursor (draft-fossil-horizon-00 §6.1, §6.2.6): go-libfossil's
// handler is the origin, its pull and sync the client. The origin's
// requests are written by hand, so each MUST is read off one reply; the
// client's are read off the requests the peer recorded.

var (
	horizonCursorOrigin   = Case{ID: "horizon.6.2.6.origin-cursor", Req: "HZ-6.2-11"}
	horizonCursorPushOnly = Case{ID: "horizon.6.2.6.push-only-silent", Req: "HZ-6.2-11"}
	horizonCursorRekept   = Case{ID: "horizon.6.2.6.reclassified-once", Req: "HZ-6.2-11"}
	horizonCursorMeeting  = Case{ID: "horizon.6.2.6.meeting-moved", Req: "HZ-6.2-11"}
	horizonCursorClient   = Case{ID: "horizon.6.2.6.client-cursor", Req: "HZ-6.2-12"}
	horizonCursorGimme    = Case{ID: "horizon.6.2.6.gimme-answered", Req: "HZ-6.2-13"}
	horizonCursorLost     = Case{ID: "horizon.6.2.6.lost-cursor", Req: "HZ-6.2-13"}

	horizonCursorCloneReply  = Case{ID: "horizon.6.2.6.clone-reply-cursor", Req: "HZ-6.2-11"}
	horizonCursorCloneRecord = Case{ID: "horizon.6.2.6.clone-records-cursor", Req: "HZ-6.2-12"}
)

func horizonCursorCases() []Case {
	return []Case{horizonCursorOrigin, horizonCursorPushOnly, horizonCursorRekept, horizonCursorMeeting,
		horizonCursorClient, horizonCursorGimme, horizonCursorLost,
		horizonCursorCloneReply, horizonCursorCloneRecord}
}

func TestHorizonCursor(t *testing.T) {
	check(t, horizonCursorOrigin, func(t *testing.T) error { return hzCursorOrigin(t) })
	check(t, horizonCursorPushOnly, func(t *testing.T) error { return hzCursorPushOnly(t) })
	check(t, horizonCursorRekept, func(t *testing.T) error { return hzCursorRekept(t) })
	check(t, horizonCursorMeeting, func(t *testing.T) error { return hzCursorMeeting(t) })
	check(t, horizonCursorClient, func(t *testing.T) error { return hzCursorClient(t) })
	check(t, horizonCursorGimme, func(t *testing.T) error { return hzCursorGimme(t) })
	check(t, horizonCursorLost, func(t *testing.T) error { return hzCursorLost(t) })
	check(t, horizonCursorCloneReply, func(t *testing.T) error { return hzCursorCloneReply(t) })
	check(t, horizonCursorCloneRecord, func(t *testing.T) error { return hzCursorCloneRecord(t) })
}

// hzNoCursor asks hzAsk for a request that carries no cursor.
const hzNoCursor = -1

// hzAsk sends origin one request through its handler and returns the reply
// as the wire delivers it: a pull and a push as asked, the horizon of sc
// when sc is set, pragma horizon-cursor when cursor is not hzNoCursor, then
// extra.
func hzAsk(t *testing.T, origin *repo.Repo, sc *cone.Cone, pull, push bool, cursor int64,
	extra ...xfer.Card) (*xfer.Message, error) {
	t.Helper()
	code, err := origin.Config("project-code")
	if err != nil {
		return nil, err
	}
	cards := []xfer.Card{&xfer.PragmaCard{Name: "client-version", Values: []string{"22800"}}}
	if pull {
		cards = append(cards, &xfer.PullCard{ServerCode: "x", ProjectCode: code})
	}
	if push {
		cards = append(cards, &xfer.PushCard{ServerCode: "x", ProjectCode: code})
	}
	if sc != nil {
		h, _ := sc.Horizon()
		cards = append(cards, &xfer.PragmaCard{Name: "horizon", Values: []string{h.Hash(), h.Rule().String()}})
	}
	if cursor != hzNoCursor {
		cards = append(cards, &xfer.PragmaCard{Name: "horizon-cursor",
			Values: []string{strconv.FormatInt(cursor, 10)}})
	}
	req, err := scSyncWire(&xfer.Message{Cards: append(cards, extra...)})
	if err != nil {
		return nil, err
	}
	resp, err := sync.HandleSync(context.Background(), origin, req)
	if err != nil {
		return nil, err
	}
	return scSyncWire(resp)
}

// hzCursorOf is the one horizon-cursor a message carries, or an error.
func hzCursorOf(m *xfer.Message) (int64, error) {
	got := hzPragmas(m, "horizon-cursor")
	if len(got) != 1 || len(got[0]) != 1 {
		return 0, expectf("message carries horizon-cursor %q, want one value", got)
	}
	return strconv.ParseInt(got[0][0], 10, 64)
}

// hzIGots is every name m announces with igot.
func hzIGots(m *xfer.Message) map[string]bool {
	return hzAnnounced([]*xfer.Message{m})
}

// hzRID is uuid's rid in r.
func hzRID(r *repo.Repo, uuid string) (libfossil.FslID, error) {
	var rid libfossil.FslID
	err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid)
	return rid, err
}

// hzAbove is the names of names whose rid in r exceeds n.
func hzAbove(r *repo.Repo, names map[string]bool, n int64) (map[string]bool, error) {
	out := map[string]bool{}
	for uuid := range names {
		rid, err := hzRID(r, uuid)
		if err != nil {
			return nil, err
		}
		if int64(rid) > n {
			out[uuid] = true
		}
	}
	return out, nil
}

// hzSameNames fails unless got and want name the same artifacts.
func hzSameNames(what string, got, want map[string]bool) error {
	for uuid := range want {
		if !got[uuid] {
			return expectf("%s: %s missing", what, uuid)
		}
	}
	for uuid := range got {
		if !want[uuid] {
			return expectf("%s: %s unexpected", what, uuid)
		}
	}
	return nil
}

// hzMaxRID is the highest rid r holds a row for.
func hzMaxRID(r *repo.Repo) (int64, error) {
	var n int64
	err := r.DB().QueryRow("SELECT coalesce(max(rid),0) FROM blob").Scan(&n)
	return n, err
}

// An origin that classified a pull says, right after horizon-served, the
// position through which it served it; receiving that cursor it announces
// nothing at or below it, and every kept artifact above it: exactly what it
// would announce without a cursor, less what lies at or below. A dense
// pull carrying the cursor is bounded the same way but is not handed one.
func hzCursorOrigin(t *testing.T) error {
	g := newHZSyncGraph(t)
	first, err := hzAsk(t, g.f.r, g.c, true, false, hzNoCursor)
	if err != nil {
		return err
	}
	n, err := hzCursorOf(first)
	if err != nil {
		return err
	}
	if top, err := hzMaxRID(g.f.r); err != nil || n != top {
		return expectf("cursor %d, want the origin's position %d (%v)", n, top, err)
	}
	if err := hzCursorFollowsServed(first); err != nil {
		return err
	}
	if len(hzIGots(first)) == 0 {
		return expectf("a pull without a cursor was announced nothing")
	}
	again, err := hzAsk(t, g.f.r, g.c, true, false, n)
	if err != nil {
		return err
	}
	if got := hzIGots(again); len(got) != 0 {
		return expectf("a pull at the origin's own cursor was announced %d artifacts", len(got))
	}
	hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	hzCommit(t, g.f, "feat2", []libfossil.FslID{g.feat}, nil)
	for _, sc := range []*cone.Cone{g.c, nil} {
		full, err := hzAsk(t, g.f.r, sc, true, false, hzNoCursor)
		if err != nil {
			return err
		}
		bounded, err := hzAsk(t, g.f.r, sc, true, false, n)
		if err != nil {
			return err
		}
		want, err := hzAbove(g.f.r, hzIGots(full), n)
		if err != nil {
			return err
		}
		if len(want) == 0 {
			return expectf("fixture: nothing kept above cursor %d", n)
		}
		if err := hzSameNames(fmt.Sprintf("announced above cursor %d (horizon %v)", n, sc != nil),
			hzIGots(bounded), want); err != nil {
			return err
		}
		if got := len(hzPragmas(bounded, "horizon-cursor")); (sc != nil) != (got == 1) || got > 1 {
			return expectf("a reply (horizon %v) carries %d horizon-cursor pragmas", sc != nil, got)
		}
	}
	return nil
}

// hzCursorFollowsServed fails unless horizon-cursor directly follows
// horizon-served, where zit places it (sync.zig appendHorizonServed).
func hzCursorFollowsServed(m *xfer.Message) error {
	for i, card := range m.Cards {
		p, ok := card.(*xfer.PragmaCard)
		if !ok || p.Name != "horizon-served" {
			continue
		}
		if i+1 < len(m.Cards) {
			if next, ok := m.Cards[i+1].(*xfer.PragmaCard); ok && next.Name == "horizon-cursor" {
				return nil
			}
		}
		return expectf("horizon-cursor does not follow horizon-served")
	}
	return expectf("the reply carries no horizon-served")
}

// An origin announces only in reply to a request that pulls: a push-only
// request's reply announces nothing, with a cursor or without one.
func hzCursorPushOnly(t *testing.T) error {
	g := newHZSyncGraph(t)
	for _, cursor := range []int64{hzNoCursor, 1} {
		reply, err := hzAsk(t, g.f.r, nil, false, true, cursor)
		if err != nil {
			return err
		}
		if got := hzIGots(reply); len(got) != 0 {
			return expectf("a push-only reply (cursor %d) announced %d artifacts", cursor, len(got))
		}
		if got := hzPragmas(reply, "horizon-cursor"); len(got) != 0 {
			return expectf("a push-only reply carries horizon-cursor %q", got)
		}
	}
	return nil
}

// The cursor changes what is announced, never what is answered: a gimme
// for a kept artifact at or below the cursor is answered with its content.
// It is advisory: an origin that cannot place a cursor ignores it.
func hzCursorGimme(t *testing.T) error {
	g := newHZSyncGraph(t)
	first, err := hzAsk(t, g.f.r, g.c, true, false, hzNoCursor)
	if err != nil {
		return err
	}
	n, err := hzCursorOf(first)
	if err != nil {
		return err
	}
	want := hzUUID(t, g.f.r, g.c4)
	reply, err := hzAsk(t, g.f.r, g.c, true, false, n, &xfer.GimmeCard{UUID: want})
	if err != nil {
		return err
	}
	if _, ok := hzSent([]*xfer.Message{reply})[want]; !ok {
		return expectf("a gimme for %s at or below cursor %d went unanswered", want, n)
	}
	// A cursor beyond the origin's position is not one it handed out: it
	// cannot place it, and ignores it.
	foreign, err := hzAsk(t, g.f.r, g.c, true, false, n+1000)
	if err != nil {
		return err
	}
	return hzSameNames(fmt.Sprintf("announced under the foreign cursor %d", n+1000),
		hzIGots(foreign), hzIGots(first))
}

// hzWorkRepo clones origin densely into a repository a test commits in
// without the origin seeing it.
func hzWorkRepo(t *testing.T, origin *repo.Repo) *scRepo {
	t.Helper()
	r, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "work.fossil"),
		&scSyncPeer{r: origin}, sync.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return &scRepo{r: r, when: hzLater}
}

// hzLater is a commit time after every check-in a fixture origin holds.
var hzLater = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

// hzFileCards are full file cards for check-in rid of r and its own files.
func hzFileCards(t *testing.T, r *repo.Repo, rid libfossil.FslID) []xfer.Card {
	t.Helper()
	names := hzFiles(t, r, rid)
	names[hzUUID(t, r, rid)] = true
	var cards []xfer.Card
	for _, uuid := range scSorted(names) {
		src, err := hzRID(r, uuid)
		if err != nil {
			t.Fatal(err)
		}
		data, err := content.Expand(r.DB(), src)
		if err != nil {
			t.Fatal(err)
		}
		cards = append(cards, &xfer.FileCard{UUID: uuid, Content: data})
	}
	return cards
}

// When an artifact arrives that a held check-in already cited, older held
// artifacts can enter the classification; a request carrying a cursor
// from before is answered as if it carried none, once: the cursor of that
// reply is honoured again.
func hzCursorRekept(t *testing.T) error {
	g := newHZSyncGraph(t)
	work := hzWorkRepo(t, g.f.r)
	c4, err := hzRID(work.r, hzUUID(t, g.f.r, g.c4))
	if err != nil {
		return err
	}
	p := hzCommit(t, work, "late-parent", []libfossil.FslID{c4}, nil)
	x := hzCommit(t, work, "early-child", []libfossil.FslID{p}, nil)
	// The child arrives first; its parent is left a phantom it cites.
	if reply, err := hzAsk(t, g.f.r, nil, false, true, hzNoCursor, hzFileCards(t, work.r, x)...); err != nil {
		return err
	} else if errs := hzErrors(reply); len(errs) != 0 {
		return expectf("pushing the child: %q", errs)
	}
	before, err := hzAsk(t, g.f.r, g.c, true, false, hzNoCursor)
	if err != nil {
		return err
	}
	old, err := hzCursorOf(before)
	if err != nil {
		return err
	}
	parent, err := hzRID(g.f.r, hzUUID(t, work.r, p))
	if err != nil {
		return err
	}
	if old >= int64(parent) {
		return expectf("cursor %d covers the phantom parent rid=%d", old, parent)
	}
	if reply, err := hzAsk(t, g.f.r, nil, false, true, hzNoCursor, hzFileCards(t, work.r, p)...); err != nil {
		return err
	} else if errs := hzErrors(reply); len(errs) != 0 {
		return expectf("pushing the parent: %q", errs)
	}
	full, err := hzAsk(t, g.f.r, g.c, true, false, hzNoCursor)
	if err != nil {
		return err
	}
	voided, err := hzAsk(t, g.f.r, g.c, true, false, old)
	if err != nil {
		return err
	}
	if err := hzSameNames(fmt.Sprintf("announced under the void cursor %d", old),
		hzIGots(voided), hzIGots(full)); err != nil {
		return err
	}
	next, err := hzCursorOf(voided)
	if err != nil {
		return err
	}
	after, err := hzAsk(t, g.f.r, g.c, true, false, next)
	if err != nil {
		return err
	}
	if got := hzIGots(after); len(got) != 0 {
		return expectf("the cursor %d past the reclassification was not honoured: %d announced", next, len(got))
	}
	return nil
}

// hzCursorMeeting: under the leaves rule a new childless check-in forked at
// c1, below C, moves the meeting point of its ancestry with C's to c1, and
// c1, c2 and feat, all older than any cursor handed out, join K (§3.1). The
// origin reclassifies, so a cursor from before is answered as if the request
// carried none, once, and the reply's cursor is honoured after (§6.2.6).
func hzCursorMeeting(t *testing.T) error {
	g := newHZSyncGraph(t)
	leaves := hzStoreCone(t, hzUUID(t, g.f.r, g.c3), cone.RuleLeaves, cone.DepthContent)
	before, err := hzAsk(t, g.f.r, leaves, true, false, hzNoCursor)
	if err != nil {
		return err
	}
	old, err := hzCursorOf(before)
	if err != nil {
		return err
	}
	c2Blobs := hzMinus(hzFiles(t, g.f.r, g.c2), g.above)
	if len(c2Blobs) == 0 {
		return expectf("fixture: c2 names no blob of its own")
	}
	for _, uuid := range c2Blobs {
		if hzIGots(before)[uuid] {
			return expectf("fixture: c2's blob %s is above the leaves horizon before the fork", uuid)
		}
	}
	hzCommit(t, g.f, "late", []libfossil.FslID{g.c1}, hzBranch("late"))
	full, err := hzAsk(t, g.f.r, leaves, true, false, hzNoCursor)
	if err != nil {
		return err
	}
	for _, uuid := range c2Blobs {
		if !hzIGots(full)[uuid] {
			return expectf("fixture: c2's blob %s did not join K with the fork at c1", uuid)
		}
	}
	voided, err := hzAsk(t, g.f.r, leaves, true, false, old)
	if err != nil {
		return err
	}
	if err := hzSameNames(fmt.Sprintf("announced under the cursor %d from before the fork", old),
		hzIGots(voided), hzIGots(full)); err != nil {
		return err
	}
	next, err := hzCursorOf(voided)
	if err != nil {
		return err
	}
	after, err := hzAsk(t, g.f.r, leaves, true, false, next)
	if err != nil {
		return err
	}
	if got := hzIGots(after); len(got) != 0 {
		return expectf("the cursor %d past the fork was not honoured: %d announced", next, len(got))
	}
	return nil
}

// hzErrors is the text of every error card m carries.
func hzErrors(m *xfer.Message) []string {
	var out []string
	for _, card := range m.Cards {
		if c, ok := card.(*xfer.ErrorCard); ok {
			out = append(out, c.Message)
		}
	}
	return out
}

// hzRequestIGots counts the igot cards of every request.
func hzRequestIGots(requests []*xfer.Message) map[string]bool {
	return hzAnnounced(requests)
}

// hzRequestCursors fails unless every request carries horizon-cursor want,
// or, when want is hzNoCursor, none does.
func hzRequestCursors(requests []*xfer.Message, want int64) error {
	for i, req := range requests {
		got := hzPragmas(req, "horizon-cursor")
		if want == hzNoCursor {
			if len(got) != 0 {
				return expectf("request %d carries horizon-cursor %q, want none", i, got)
			}
			continue
		}
		n, err := hzCursorOf(req)
		if err != nil {
			return expectf("request %d: %v", i, err)
		}
		if n != want {
			return expectf("request %d carries cursor %d, want %d", i, n, want)
		}
	}
	return nil
}

// hzLastCursor is the cursor of the last reply that carried one.
func hzLastCursor(replies []*xfer.Message) (int64, error) {
	for i := len(replies) - 1; i >= 0; i-- {
		if len(hzPragmas(replies[i], "horizon-cursor")) != 0 {
			return hzCursorOf(replies[i])
		}
	}
	return 0, expectf("no reply of %d carried a horizon-cursor", len(replies))
}

// A client that converged records the last reply's cursor per origin and
// sends it on every later request to that origin. A pull-only request then
// announces nothing; a request that sends announces only what the client
// appended after the recorded count, and a pull does not advance that
// count past an artifact the origin does not hold.
func hzCursorClient(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer, r, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return err
	}
	opts := sync.SyncOpts{Pull: true, Origin: "origin"}
	peer.requests, peer.replies = nil, nil
	if _, err := sync.Sync(context.Background(), r, peer, opts); err != nil {
		return err
	}
	if err := hzRequestCursors(peer.requests, hzNoCursor); err != nil {
		return err
	}
	n, err := hzLastCursor(peer.replies)
	if err != nil {
		return err
	}
	hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	peer.requests, peer.replies = nil, nil
	if _, err := sync.Sync(context.Background(), r, peer, opts); err != nil {
		return err
	}
	if err := hzRequestCursors(peer.requests, n); err != nil {
		return err
	}
	if got := hzRequestIGots(peer.requests); len(got) != 0 {
		return expectf("a pull-only request under a cursor announced %d artifacts", len(got))
	}
	if n, err = hzLastCursor(peer.replies); err != nil {
		return err
	}
	// The first exchange that sends announces the whole holding once; the
	// clone recorded no count.
	both := sync.SyncOpts{Pull: true, Push: true, Origin: "origin"}
	peer.requests, peer.replies = nil, nil
	if _, err := sync.Sync(context.Background(), r, peer, both); err != nil {
		return err
	}
	if err := hzRequestCursors(peer.requests, n); err != nil {
		return err
	}
	if len(hzRequestIGots(peer.requests)) == 0 {
		return expectf("the first sync that sends announced nothing")
	}
	if n, err = hzLastCursor(peer.replies); err != nil {
		return err
	}
	// A local commit; the pull that follows must not count it delivered.
	local := &scRepo{r: r, when: hzLater}
	m, err := hzRID(r, hzUUID(t, g.f.r, g.m))
	if err != nil {
		return err
	}
	mine := hzCommit(t, local, "mine", []libfossil.FslID{m}, nil)
	appended := hzFiles(t, r, mine)
	appended[hzUUID(t, r, mine)] = true
	peer.requests, peer.replies = nil, nil
	if _, err := sync.Sync(context.Background(), r, peer, opts); err != nil {
		return err
	}
	if n, err = hzLastCursor(peer.replies); err != nil {
		return err
	}
	peer.requests, peer.replies = nil, nil
	if _, err := sync.Sync(context.Background(), r, peer, both); err != nil {
		return err
	}
	if err := hzRequestCursors(peer.requests, n); err != nil {
		return err
	}
	if err := hzSameNames("a sync under a cursor announced", hzRequestIGots(peer.requests), appended); err != nil {
		return err
	}
	return hzHeld(g.f.r, appended, true)
}

// The cursor is advisory: a client holding none for the origin it
// addresses announces its whole holding on a request that sends.
func hzCursorLost(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer, r, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return err
	}
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true, Origin: "origin"}); err != nil {
		return err
	}
	recorded, err := hzMaxRID(r)
	if err != nil {
		return err
	}
	peer.requests = nil
	if _, err := sync.Sync(context.Background(), r, peer,
		sync.SyncOpts{Pull: true, Push: true, Origin: "elsewhere"}); err != nil {
		return err
	}
	if err := hzRequestCursors(peer.requests, hzNoCursor); err != nil {
		return err
	}
	older := 0
	for uuid := range hzRequestIGots(peer.requests) {
		rid, err := hzRID(r, uuid)
		if err != nil {
			return err
		}
		if int64(rid) <= recorded {
			older++
		}
	}
	if older == 0 {
		return expectf("a client without a cursor announced none of its older holding")
	}
	return nil
}

// hzSmallBatches makes a clone origin send one artifact per round.
type hzSmallBatches struct{}

func (hzSmallBatches) Check(site string, _ float64) bool {
	return site == "handler.emitCloneBatch.smallBatch"
}

// hzCloneAt clones g's origin under sc as the peer named origin, through
// peer, which records the exchange.
func hzCloneAt(t *testing.T, peer *scSyncPeer, sc *cone.Cone, origin string) (*repo.Repo, error) {
	t.Helper()
	r, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "horizon.fossil"),
		peer, sync.CloneOpts{StoreCone: sc, Origin: origin})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { r.Close() })
	return r, nil
}

// An origin that classified a clone request emits the cursor in every
// reply, right after horizon-served: the position through which that
// reply served the clone sequence, so a round that stops short names a
// position below the sequence it hands on, and the last reply names the
// origin's own position.
func hzCursorCloneReply(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer := &scSyncPeer{r: g.f.r, opts: sync.HandleOpts{Buggify: hzSmallBatches{}}}
	if _, err := hzCloneAt(t, peer, g.c, "origin"); err != nil {
		return err
	}
	top, err := hzMaxRID(g.f.r)
	if err != nil {
		return err
	}
	rounds := 0
	for i, reply := range peer.replies {
		seqno := -1
		for _, card := range reply.Cards {
			if c, ok := card.(*xfer.CloneSeqNoCard); ok {
				seqno = c.SeqNo
			}
		}
		if seqno < 0 {
			continue // the operation-less cleanup round serves nothing
		}
		rounds++
		n, err := hzCursorOf(reply)
		if err != nil {
			return expectf("clone reply %d: %v", i, err)
		}
		if err := hzCursorFollowsServed(reply); err != nil {
			return expectf("clone reply %d: %v", i, err)
		}
		if seqno > 0 && n >= int64(seqno) {
			return expectf("clone reply %d: cursor %d covers the next sequence %d", i, n, seqno)
		}
		if seqno == 0 && n != top {
			return expectf("last clone reply: cursor %d, want the origin's position %d", n, top)
		}
	}
	if rounds < 3 {
		return expectf("fixture: the clone took %d rounds, want several", rounds)
	}
	return nil
}

// A client whose clone converged records the last reply's cursor and its
// own count per origin, so its first pull sends the cursor and announces
// nothing, and its first exchange that sends announces only what it
// appended since.
func hzCursorCloneRecord(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer := &scSyncPeer{r: g.f.r}
	r, err := hzCloneAt(t, peer, g.c, "origin")
	if err != nil {
		return err
	}
	n, err := hzLastCursor(peer.replies)
	if err != nil {
		return err
	}
	peer.requests, peer.replies = nil, nil
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true, Origin: "origin"}); err != nil {
		return err
	}
	if err := hzRequestCursors(peer.requests, n); err != nil {
		return err
	}
	if got := hzRequestIGots(peer.requests); len(got) != 0 {
		return expectf("the first pull after a clone announced %d artifacts", len(got))
	}
	local := &scRepo{r: r, when: hzLater}
	m, err := hzRID(r, hzUUID(t, g.f.r, g.m))
	if err != nil {
		return err
	}
	mine := hzCommit(t, local, "mine", []libfossil.FslID{m}, nil)
	appended := hzFiles(t, r, mine)
	appended[hzUUID(t, r, mine)] = true
	peer.requests, peer.replies = nil, nil
	if _, err := sync.Sync(context.Background(), r, peer,
		sync.SyncOpts{Pull: true, Push: true, Origin: "origin"}); err != nil {
		return err
	}
	if err := hzRequestCursors(peer.requests, n); err != nil {
		return err
	}
	if err := hzSameNames("the first sync that sends after a clone announced",
		hzRequestIGots(peer.requests), appended); err != nil {
		return err
	}
	// A clone addressed to no origin keeps nothing.
	bare := &scSyncPeer{r: g.f.r}
	other, err := hzCloneAt(t, bare, g.c, "")
	if err != nil {
		return err
	}
	bare.requests = nil
	if _, err := sync.Sync(context.Background(), other, bare, sync.SyncOpts{Pull: true, Origin: "origin"}); err != nil {
		return err
	}
	return hzRequestCursors(bare.requests, hzNoCursor)
}
