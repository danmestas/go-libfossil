package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
)

// An origin's seed label is its own: it appears on the seeds it mints and on
// no artifact it writes for sharing (horizon §5.2.2). The case drives an
// origin and a structural clone of it through every exchange a seed takes
// part in, and through the check-ins and tags each side writes, then reads
// every artifact either store holds.

func horizonSeedLabelCases() []Case { return []Case{horizonSeedLabelOwn} }

var horizonSeedLabelOwn = Case{ID: "horizon.5.2.2.seed-label-exclusive", Req: "HZ-5.2-2"}

func TestHorizonSeedLabel(t *testing.T) {
	check(t, horizonSeedLabelOwn, func(t *testing.T) error { return hzSeedLabelOwn(t) })
}

// hzLabelled is every artifact r stores whose U card is go-libfossil's seed
// label.
func hzLabelled(r *repo.Repo) ([]string, error) {
	rows, err := r.DB().Query("SELECT rid, uuid FROM blob WHERE size>=0 ORDER BY rid")
	if err != nil {
		return nil, err
	}
	type row struct {
		rid  libfossil.FslID
		uuid string
	}
	var all []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.rid, &x.uuid); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, x)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []string
	for _, x := range all {
		data, err := content.Expand(r.DB(), x.rid)
		if err != nil {
			return nil, err
		}
		if d, err := deck.Parse(data); err == nil && d.U != nil && *d.U == seedcheck.Label {
			out = append(out, x.uuid)
		}
	}
	return out, nil
}

// hzSeedLabelOwn: the clone receives a seed, the origin moves a tag and the
// clone pulls the new one, then the clone commits and tags and syncs both
// ways. The seeds the clone holds carry the label; nothing either store
// holds does, though each holds what the other wrote.
func hzSeedLabelOwn(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	r, result, err := x.clone(t, peer)
	if err != nil {
		return err
	}
	if result.StructuralServed == 0 {
		return expectf("the structural clone was not served a seed: %+v", result)
	}
	first := string(mustHeld(t, r))
	x.s.tag(t, x.s.c3, "sym-v3", tag.TagSingleton, "")
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	held := mustHeld(t, r)
	if string(held) == first {
		return expectf("the pull after a tag change did not carry the new seed")
	}
	if d, err := deck.Parse(held); err != nil || d.U == nil || *d.U != seedcheck.Label {
		return expectf("the held seed's U card %v (%v), want the label %q", d, err, seedcheck.Label)
	}
	ci, err := hzSeedLabelWrites(t, x, r)
	if err != nil {
		return err
	}
	both := sync.SyncOpts{Push: true, Pull: true}
	if _, err := sync.Sync(context.Background(), r, peer, both); err != nil {
		return err
	}
	if _, ok := blob.Exists(x.s.f.r.DB(), ci); !ok {
		return expectf("the origin did not receive the clone's check-in")
	}
	for _, store := range []struct {
		what string
		r    *repo.Repo
	}{{"the origin", x.s.f.r}, {"the clone", r}} {
		got, err := hzLabelled(store.r)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			return expectf("%s stores %d artifacts under the seed label: %.10s",
				store.what, len(got), got)
		}
	}
	return nil
}

// hzSeedLabelWrites makes the clone commit a check-in on c4 and tag it, and
// the origin tag c4, so each store writes control artifacts and a manifest of
// its own. It returns the clone's check-in.
func hzSeedLabelWrites(t *testing.T, x *hzStruct, r *repo.Repo) (string, error) {
	c4, ok := blob.Exists(r.DB(), x.c4)
	if !ok {
		return "", expectf("the clone does not hold c4")
	}
	when := x.s.f.when.Add(time.Hour)
	rid, err := hzFileCheckin(r, c4, []byte("w\n"), when)
	if err != nil {
		return "", err
	}
	if _, err := tag.AddTag(r, tag.TagOpts{TargetRID: rid, TagName: "sym-w",
		TagType: tag.TagSingleton, User: "u", Time: when.Add(time.Minute)}); err != nil {
		return "", err
	}
	x.s.tag(t, x.s.c4, "reviewed", tag.TagSingleton, "")
	return hzUUID(t, r, rid), nil
}
