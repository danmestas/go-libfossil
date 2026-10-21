package manifest

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestArrivalOrderMatchesRebuild delivers a random history's blobs to a fresh
// repository in a random order and in random rounds, crosslinking after each
// round as a sync does, and compares the tag and leaf tables with a rebuild of
// the same blobs (issue #432). It runs only when LIBFOSSIL_ARRIVAL_PROBE names
// a seed count, as it takes minutes:
//
//	LIBFOSSIL_ARRIVAL_PROBE=200 go test ./internal/manifest -run ArrivalOrder -v
//
// Without reparents every seed must match. With them it only reports the
// seeds that differ: a reparent never removes the tags inherited from the old
// parent and schedules no leaf recheck, as in fossil, whose own rebuild is
// order-dependent there (see ReparentCheckins).
func TestArrivalOrderMatchesRebuild(t *testing.T) {
	seeds, err := strconv.Atoi(os.Getenv("LIBFOSSIL_ARRIVAL_PROBE"))
	if err != nil || seeds <= 0 {
		t.Skip("set LIBFOSSIL_ARRIVAL_PROBE to a seed count to run")
	}
	for _, reparents := range []bool{false, true} {
		var tagBad, leafBad []int64
		for seed := int64(0); seed < int64(seeds); seed++ {
			t.Run(fmt.Sprintf("reparents=%v/seed=%d", reparents, seed), func(t *testing.T) {
				tags, leaves := arrivalOrderDiffers(t, seed, reparents)
				if tags {
					tagBad = append(tagBad, seed)
				}
				if leaves {
					leafBad = append(leafBad, seed)
				}
			})
		}
		t.Logf("reparents=%v seeds=%d: tags differ in %d %v, leaves differ in %d %v",
			reparents, seeds, len(tagBad), tagBad, len(leafBad), leafBad)
		if !reparents && (len(tagBad) > 0 || len(leafBad) > 0) {
			t.Errorf("without reparents, arrival order changed the result: tags %v, leaves %v",
				tagBad, leafBad)
		}
	}
}

// arrivalOrderDiffers builds seed's random history in a source repository:
// 8 to 13 check-ins with branches and merges, one or two propagating tags set
// by control artifacts and, when reparents is set, one or two parent tags. It
// delivers the blobs to a fresh repository as TestArrivalOrderMatchesRebuild
// describes and reports whether its tags and leaves differ from a rebuild.
func arrivalOrderDiffers(t *testing.T, seed int64, reparents bool) (tags, leaves bool) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	src := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	n := 8 + rng.Intn(6)
	uuids := make([]string, n)
	for i := range n {
		var cards []deck.TagCard
		if i == 0 || rng.Intn(4) == 0 {
			branch := fmt.Sprintf("b%d", i)
			if i == 0 {
				branch = "trunk"
			}
			cards = append(cards,
				deck.TagCard{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: branch},
				deck.TagCard{Type: deck.TagSingleton, Name: "sym-" + branch, UUID: "*"})
		}
		var parents []string
		if i > 0 {
			parents = append(parents, uuids[rng.Intn(i)])
			if i > 1 && rng.Intn(5) == 0 {
				if merged := uuids[rng.Intn(i)]; merged != parents[0] {
					parents = append(parents, merged)
				}
			}
		}
		body, uuid := buildCheckin(t, src, fmt.Sprintf("c%d-%d", seed, i),
			base.Add(time.Duration(i)*time.Minute), cards, parents...)
		if _, _, err := blob.Store(src.DB(), body); err != nil {
			t.Fatalf("store c%d: %v", i, err)
		}
		uuids[i] = uuid
	}
	for k := range 1 + rng.Intn(2) {
		storeTagControl(t, src, uuids[rng.Intn(n)], fmt.Sprintf("color%d", k),
			base.Add(time.Hour+time.Duration(k)*time.Minute))
	}
	if reparents {
		for k := range 1 + rng.Intn(2) {
			child := 2 + rng.Intn(n-2)
			storeParentTag(t, src, uuids[child], []string{uuids[rng.Intn(child)]},
				base.Add(2*time.Hour+time.Duration(k)*time.Minute))
		}
	}

	blobs := expandAll(t, src)
	rng.Shuffle(len(blobs), func(i, j int) { blobs[i], blobs[j] = blobs[j], blobs[i] })
	dst := setupTestRepo(t)
	for len(blobs) > 0 {
		k := 1 + rng.Intn(len(blobs))
		for _, b := range blobs[:k] {
			if _, _, err := blob.Store(dst.DB(), b); err != nil {
				t.Fatalf("deliver: %v", err)
			}
		}
		blobs = blobs[k:]
		if _, err := Crosslink(dst); err != nil {
			t.Fatalf("Crosslink: %v", err)
		}
	}
	gotTags, gotLeaves := tagLeafState(t, dst)
	wantTags, wantLeaves := rebuildFromScratch(t, dst)
	return gotTags != wantTags, gotLeaves != wantLeaves
}

// storeTagControl stores a control artifact that sets propagating tag name on
// target.
func storeTagControl(t *testing.T, r *repo.Repo, target, name string, at time.Time) {
	t.Helper()
	d := &deck.Deck{
		Type: deck.Control,
		D:    at,
		T:    []deck.TagCard{{Type: deck.TagPropagating, Name: name, UUID: target, Value: "red"}},
		U:    deck.User("testuser"),
	}
	data, err := d.Marshal()
	if err != nil {
		t.Fatalf("marshal %s control: %v", name, err)
	}
	if _, _, err := blob.Store(r.DB(), data); err != nil {
		t.Fatalf("store %s control: %v", name, err)
	}
}

// expandAll returns the full content of every non-phantom blob in r.
func expandAll(t *testing.T, r *repo.Repo) [][]byte {
	t.Helper()
	rows, err := r.DB().Query("SELECT rid FROM blob WHERE size >= 0 ORDER BY rid")
	if err != nil {
		t.Fatalf("list blobs: %v", err)
	}
	var rids []libfossil.FslID
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			t.Fatalf("scan blob: %v", err)
		}
		rids = append(rids, libfossil.FslID(rid))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list blobs: %v", err)
	}
	out := make([][]byte, 0, len(rids))
	for _, rid := range rids {
		b, err := content.Expand(r.DB(), rid)
		if err != nil {
			t.Fatalf("expand rid=%d: %v", rid, err)
		}
		out = append(out, b)
	}
	return out
}
