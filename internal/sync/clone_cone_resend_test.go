package sync

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestConeCloneSendsAForwardedBaseOnce has an origin hold an in-cone file's
// history as fossil does, each version a delta of the next and each check-in
// a delta of its child, with out-of-cone rows between the check-ins so the
// cone clone takes several rounds. A check-in's walk sends its version with
// the next version ahead of it as the delta's base, and the next check-in
// sends its parent's manifest the same way; when a later round reaches that
// base at its own row or walk it must not send it again. Sending it again
// made a cone clone of the Fossil repository receive about 84,400 blobs to
// hold 56,600 (issue #465).
func TestConeCloneSendsAForwardedBaseOnce(t *testing.T) {
	origin, files, checkins := coneChainFixture(t)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	tr := &cfileCounter{r: origin, sent: map[string]int{}}
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"),
		tr, CloneOpts{StoreCone: c})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	if tr.rounds < 3 {
		t.Fatalf("clone took %d rounds; the fixture needs several", tr.rounds)
	}
	repeats := 0
	for uuid, n := range tr.sent {
		if n != 1 {
			repeats += n - 1
			t.Errorf("%s went out %d times", uuid[:10], n)
		}
	}
	t.Logf("%d rounds, %d artifacts, %d repeated sends", tr.rounds, len(tr.sent), repeats)
	if want := len(files) + len(checkins); len(tr.sent) != want {
		t.Errorf("%d artifacts went out, want %d", len(tr.sent), want)
	}
	for _, rid := range append(files, checkins...) {
		var uuid string
		if err := origin.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			t.Fatal(err)
		}
		cr, ok := blob.Exists(clone.DB(), uuid)
		if !ok {
			t.Fatalf("clone lacks %s", uuid[:10])
		}
		if _, err := content.Expand(clone.DB(), cr); err != nil {
			t.Errorf("clone's %s: %v", uuid[:10], err)
		}
	}
}

// A walk's cookie names its runs compactly and bounded; one it cannot read
// back carries nothing, which only sends those bases again.
func TestCloneRunsCookie(t *testing.T) {
	runs := make([][]int, 0, maxCloneRuns+3)
	for i := range maxCloneRuns + 3 {
		runs = append(runs, []int{1000 + i, 7, 3})
	}
	value := formatCloneRuns(runs)
	got := parseCloneRuns(value)
	if len(got) != maxCloneRuns {
		t.Fatalf("round trip kept %d runs, want %d", len(got), maxCloneRuns)
	}
	// The newest runs are kept: an old one may never be reached.
	if want := (cloneRunRef{head: 1003, n: 3, sum: runSum([]int{1003, 7, 3})}); got[0] != want {
		t.Fatalf("first run = %+v, want %+v", got[0], want)
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "0.1.a", "1.0.a", "x.1.a", "1.1.!", "1.5000.a",
		"1.1.a,,2.1.a", value + ",9.1.a"} {
		if refs := parseCloneRuns(bad); refs != nil {
			t.Errorf("parseCloneRuns(%.20q) = %v, want nil", bad, refs)
		}
	}
	resume := cloneResumeFromCards([]xfer.Card{&xfer.CookieCard{Value: cloneCapPrefix + "90:12:15/3.2.a,40.1.b"}})
	if !resume.aware || !resume.set || resume.row != 12 || resume.last != 15 || len(resume.runs) != 2 ||
		resume.runs[1] != (cloneRunRef{head: 40, n: 1, sum: 11}) {
		t.Fatalf("resume = %+v", resume)
	}
	if snap, ok := parseCloneCap(cloneCapPrefix + "90:12/3.2.a"); !ok || snap != 90 {
		t.Fatalf("parseCloneCap = %d, %v; want 90", snap, ok)
	}
}

// A run is rebuilt from the store's delta chain and kept only while that
// chain is the one the cookie's checksum names: a store re-deltified since
// the run was sent must not pass off a base the client never received.
func TestCloneRunRebuild(t *testing.T) {
	r := setupSyncTestRepo(t)
	var rids []libfossil.FslID
	text := bytes.Repeat([]byte("a file whose versions chain\n"), 64)
	for i := range 3 {
		text = append(text, fmt.Sprintf("version %d\n", i)...)
		rid, _, err := blob.Store(r.DB(), text)
		if err != nil {
			t.Fatal(err)
		}
		rids = append(rids, rid)
	}
	deltify := func(pairs ...[2]int) {
		t.Helper()
		err := r.WithTx(func(tx *db.Tx) error {
			for _, p := range pairs {
				if _, err := content.Deltify(tx, rids[p[0]], rids[p[1]]); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	deltify([2]int{0, 1}, [2]int{1, 2})
	h := &handler{repo: r, cloneSnapMax: int(rids[2])}
	chain := []int{int(rids[0]), int(rids[1]), int(rids[2])}
	ref := cloneRunRef{head: chain[0], n: 3, sum: runSum(chain)}
	if run, err := h.cloneRun(ref); err != nil || fmt.Sprint(run) != fmt.Sprint(chain) {
		t.Fatalf("cloneRun = %v, %v; want %v", run, err, chain)
	}
	for _, stale := range []struct {
		ref  cloneRunRef
		snap int
	}{
		{cloneRunRef{head: chain[0], n: 3, sum: ref.sum + 1}, chain[2]},              // another chain
		{cloneRunRef{head: chain[0], n: 4, sum: runSum(append(chain, 0))}, chain[2]}, // past its tip
		{cloneRunRef{head: chain[1], n: 2, sum: runSum(chain[1:])}, chain[1]},        // past the snapshot
	} {
		h.cloneSnapMax = stale.snap
		if run, err := h.cloneRun(stale.ref); err != nil || run != nil {
			t.Errorf("cloneRun(%+v) = %v, %v; want nothing", stale.ref, run, err)
		}
	}
	h.cloneSnapMax = int(rids[2])
	if err := r.WithTx(func(tx *db.Tx) error { return content.Undelta(tx, rids[0]) }); err != nil {
		t.Fatal(err)
	}
	deltify([2]int{0, 2})
	if src, err := content.DeltaSource(r.DB(), rids[0]); err != nil || src != rids[2] {
		t.Fatalf("re-deltified source = %d, %v; want %d", src, err, rids[2])
	}
	moved := cloneRunRef{head: chain[0], n: 2, sum: runSum(chain[:2])}
	for _, stale := range []cloneRunRef{ref, moved} {
		if run, err := h.cloneRun(stale); err != nil || run != nil {
			t.Errorf("re-deltified chain rebuilt as %v, %v; want nothing", run, err)
		}
	}
}

// A cookie no larger than a clone's own carries its runs; a hostile one is
// refused before it is split, so its size costs no allocation per byte.
func TestCloneCookieRefusedBeforeSplit(t *testing.T) {
	hostile := []string{
		cloneCapPrefix + strings.Repeat(":", 8<<20),
		cloneCapPrefix + "9:1/" + strings.Repeat(".", 8<<20),
		cloneCapPrefix + "9:1/" + strings.Repeat("1.1.a,", maxCloneRuns) + strings.Repeat(".", 8<<20),
	}
	for _, v := range hostile {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		resume := cloneResumeFromCards([]xfer.Card{&xfer.CookieCard{Value: v}})
		snap, ok := parseCloneCap(v)
		runtime.ReadMemStats(&after)
		if len(resume.runs) != 0 || ok || snap != 0 {
			t.Errorf("hostile cookie of %d bytes parsed: %d runs, snap %d, %v", len(v), len(resume.runs), snap, ok)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Errorf("parsing a %d-byte cookie allocated %d bytes", len(v), grew)
		}
	}
}

// What a send withholds is not on the wire, so it is neither a run's rid nor
// a chain's source already sent: a peer that cannot take SHA3 names gets the
// refusal in its place.
func TestCloneSendRecordsOnlyWhatItSends(t *testing.T) {
	r := setupSyncTestRepo(t)
	text := bytes.Repeat([]byte("a version of a file stored as a delta\n"), 64)
	older, olderUUID, err := blob.Store(r.DB(), append(bytes.Clone(text), "older\n"...))
	if err != nil {
		t.Fatal(err)
	}
	newer, _, err := blob.Store(r.DB(), append(bytes.Clone(text), "newer\n"...))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WithTx(func(tx *db.Tx) error { _, err := content.Deltify(tx, older, newer); return err }); err != nil {
		t.Fatal(err)
	}
	if len(olderUUID) <= sha1NameLen {
		t.Fatalf("fixture names %s with SHA1; it needs SHA3 names", olderUUID)
	}
	h := &handler{repo: r, cloneSnapMax: int(newer), peerVersion: sha3Version - 1}
	cards, rids, _, err := h.buildCloneArtifact(int(older), olderUUID, len(text)+6, map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(findCards[*xfer.ErrorCard](&xfer.Message{Cards: cards})) == 0 {
		t.Fatal("no refusal for a SHA3 artifact to a peer before 2.0")
	}
	if len(rids) != 0 {
		t.Fatalf("withheld rids recorded as sent: %v", rids)
	}
}

// A cookie names each chain once and the server rebuilds at most
// maxCloneRunRIDs rids from one: a forged cookie naming the same deep chain
// maxCloneRuns times, or many deep chains, costs no more.
func TestCloneRunsExpansionBounded(t *testing.T) {
	var same, deep []cloneRunRef
	for i := range maxCloneRuns {
		same = append(same, cloneRunRef{head: 7, n: maxCloneChainWalk, sum: 1})
		deep = append(deep, cloneRunRef{head: 100 + i, n: maxCloneChainWalk, sum: 1})
	}
	if got := expandable(same); len(got) != 1 {
		t.Fatalf("a head named %d times expands %d times, want once", len(same), len(got))
	}
	total := 0
	for _, ref := range expandable(deep) {
		total += ref.n
	}
	if total > maxCloneRunRIDs || total == 0 {
		t.Fatalf("a forged cookie expands %d rids, want at most %d", total, maxCloneRunRIDs)
	}
}

// A store re-deltified mid-clone changes the chains a cookie carries. Here a
// version the first round forwarded is stored again as a delta of another
// in-cone file the clone has not reached, so the chain from the cookie's
// head now runs into a blob never sent: the server must see the chain is
// not the one it sent, and the clone must end with every artifact whole.
func TestConeCloneSurvivesRedeltify(t *testing.T) {
	origin, files, checkins := coneChainFixture(t)
	uuidOf := func(rid libfossil.FslID) string {
		t.Helper()
		var uuid string
		if err := origin.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			t.Fatal(err)
		}
		return uuid
	}
	last := files[len(files)-1]
	lastText, err := content.Expand(origin.DB(), last)
	if err != nil {
		t.Fatal(err)
	}
	other, otherUUID, err := blob.Store(origin.DB(), append(lastText, "and another file's line\n"...))
	if err != nil {
		t.Fatal(err)
	}
	data, err := (&deck.Deck{Type: deck.Checkin, C: "another file", D: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		U: deck.User("u"), P: []string{uuidOf(checkins[len(checkins)-1])},
		F: []deck.FileCard{{Name: "src/f", UUID: uuidOf(last)}, {Name: "src/g", UUID: otherUUID}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	top, _ := blob.Exists(origin.DB(), storeAndLink(t, origin, data))
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &redeltify{cfileCounter: cfileCounter{r: origin, sent: map[string]int{}}, rid: files[len(files)/2], src: other}
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"),
		tr, CloneOpts{StoreCone: c})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	if !tr.done {
		t.Fatal("the store was never re-deltified; the clone took one round")
	}
	for _, rid := range append(append(files, checkins...), other, top) {
		cr, ok := blob.Exists(clone.DB(), uuidOf(rid))
		if !ok {
			t.Fatalf("clone lacks rid %d", rid)
		}
		if _, err := content.Expand(clone.DB(), cr); err != nil {
			t.Errorf("clone's rid %d: %v", rid, err)
		}
	}
}

// redeltify, after the first round, stores rid again as a delta of src.
type redeltify struct {
	cfileCounter
	rid, src libfossil.FslID
	done     bool
}

func (r *redeltify) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := r.cfileCounter.Exchange(ctx, req)
	if err != nil || r.done {
		return resp, err
	}
	r.done = true
	err = r.r.WithTx(func(tx *db.Tx) error {
		if err := content.Undelta(tx, r.rid); err != nil {
			return err
		}
		if _, err := content.Deltify(tx, r.rid, r.src); err != nil {
			return err
		}
		src, err := content.DeltaSource(tx, r.rid)
		if err == nil && src != r.src {
			err = fmt.Errorf("rid %d re-deltified against %d, want %d", r.rid, src, r.src)
		}
		return err
	})
	return resp, err
}

// coneChainFixture holds an in-cone file's history as fossil does, each
// version a delta of the next and each check-in a delta of its child, with
// out-of-cone rows between the check-ins so a cone clone takes several
// rounds. It returns the origin and the versions' and check-ins' rids.
func coneChainFixture(t *testing.T) (*repo.Repo, []libfossil.FslID, []libfossil.FslID) {
	t.Helper()
	origin := setupSyncTestRepo(t)
	rng := rand.New(rand.NewSource(1))
	const versions = 8
	var files, checkins []libfossil.FslID
	var parent string
	text := []byte("an in-cone file whose history is one chain of reverse deltas\n")
	when := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range versions {
		text = append(text, fmt.Sprintf("line %d of the history\n", i)...)
		frid, fuuid, err := blob.Store(origin.DB(), text)
		if err != nil {
			t.Fatal(err)
		}
		d := &deck.Deck{Type: deck.Checkin, C: fmt.Sprintf("version %d", i), D: when.Add(time.Duration(i) * time.Hour),
			U: deck.User("u"), F: []deck.FileCard{{Name: "src/f", UUID: fuuid}}}
		if parent != "" {
			d.P = []string{parent}
		}
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		parent = storeAndLink(t, origin, data)
		crid, ok := blob.Exists(origin.DB(), parent)
		if !ok {
			t.Fatalf("check-in %d not stored", i)
		}
		files, checkins = append(files, frid), append(checkins, crid)
		filler := make([]byte, DefaultCloneBatchBytes/2)
		rng.Read(filler)
		if _, _, err := blob.Store(origin.DB(), filler); err != nil {
			t.Fatal(err)
		}
	}
	err := origin.WithTx(func(tx *db.Tx) error {
		for i := 0; i+1 < versions; i++ {
			if _, err := content.Deltify(tx, files[i], files[i+1]); err != nil {
				return err
			}
			if _, err := content.Deltify(tx, checkins[i], checkins[i+1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return origin, files, checkins
}
