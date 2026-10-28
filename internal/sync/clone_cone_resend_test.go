package sync

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
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
	if want := 2 * versions; len(tr.sent) != want {
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
	if want := (cloneRunRef{head: 1000, n: 3, sum: runSum([]int{1000, 7, 3})}); got[0] != want {
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
