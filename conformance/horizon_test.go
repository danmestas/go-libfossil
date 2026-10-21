package conformance

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
)

// The horizon suite tracks draft-fossil-horizon-00. So far it covers the
// horizon member of the cone (§3.3): its canonical serialization and the
// version byte that makes a reader predating it refuse the cone rather than
// read it as path members; and the §3.1 held set, computed by
// internal/horizon.

func horizonSuiteCases() []Case {
	return []Case{horizonCanonical, horizonUnknownVersion, horizonHeldSetRules, horizonHeldSetRecomputed}
}

var (
	horizonCanonical         = Case{ID: "horizon.3.3.canonical-member", Req: "HZ-3.3-1"}
	horizonUnknownVersion    = Case{ID: "horizon.3.3.unknown-version", Req: "HZ-3.3-2"}
	horizonHeldSetRules      = Case{ID: "horizon.3.1.held-set-rules", Req: "HZ-3.1-1"}
	horizonHeldSetRecomputed = Case{ID: "horizon.3.1.held-set-recomputed", Req: "HZ-3.1-3"}
)

const horizonHash = "0123456789abcdef0123456789abcdef01234567"

func TestHorizon(t *testing.T) {
	check(t, horizonCanonical, func() error {
		h, err := cone.NewHorizon(horizonHash, cone.RuleLeaves, cone.DepthContent)
		if err != nil {
			return err
		}
		c, err := cone.NewWithHorizon([]string{"src"}, nil, h)
		if err != nil {
			return err
		}
		// Version 3, count 1, rule leaves, depth content, 40 hash digits,
		// then the one inclusion with its flag byte.
		want := append([]byte{0, 3, 1, 1, 0, 40}, horizonHash...)
		want = append(want, 0, 3, 0, 's', 'r', 'c')
		if got := c.Serialize(); !bytes.Equal(got, want) {
			return expectf("serialized % x, want % x", got, want)
		}
		back, err := cone.Parse(c.Serialize())
		if err != nil || !back.Equal(c) || back.Digest() != c.Digest() {
			return expectf("round trip gave %v, %v", back, err)
		}
		// The member is of a distinct kind: the same path members without a
		// horizon, or with another rule, digest differently.
		plain, err := cone.New([]string{"src"}, nil)
		if err != nil {
			return err
		}
		descend, err := cone.NewHorizon(horizonHash, cone.RuleDescendants, cone.DepthContent)
		if err != nil {
			return err
		}
		other, err := cone.NewWithHorizon([]string{"src"}, nil, descend)
		if err != nil {
			return err
		}
		if c.Digest() == plain.Digest() || c.Digest() == other.Digest() {
			return expectf("horizon did not change the digest")
		}
		// Not canonical: an upper-case hash is refused, not normalized.
		upper := bytes.Clone(want)
		upper[6] = 'A'
		if _, err := cone.Parse(upper); err == nil {
			return expectf("upper-case horizon hash accepted")
		}
		return nil
	})

	check(t, horizonUnknownVersion, func() error {
		// A version-2 cone with one inclusion, relabelled as each version
		// above the horizon's: a reader that skipped the version would see
		// "src".
		for v := byte(4); v < 8; v++ {
			forged := []byte{0, v, 1, 0, 3, 0, 's', 'r', 'c'}
			if c, err := cone.Parse(forged); err == nil {
				return expectf("serialized version %d read as %v", v, c)
			}
		}
		// Version 3 is the horizon's: a version-2 body under it is no
		// horizon and is refused, as is a malformed horizon.
		malformed := [][]byte{
			{0, 3, 1, 0, 3, 0, 's', 'r', 'c'},
			append([]byte{0, 3, 0, 2, 0, 40}, horizonHash...),
			append([]byte{0, 3, 0, 0, 2, 40}, horizonHash...),
			append([]byte{0, 3, 0, 0, 0, 39}, horizonHash[:39]...),
		}
		for _, forged := range malformed {
			if c, err := cone.Parse(forged); err == nil {
				return expectf("malformed version 3 % x read as %v", forged, c)
			}
		}
		return nil
	})
	check(t, horizonHeldSetRules, func() error { return hzRules(t) })
	check(t, horizonHeldSetRecomputed, func() error { return hzRecomputed(t) })
}

// hzGraph is trunk c1..c4 with C = c3, a branch feat forked at c1 (below C)
// and merged into c4's child m, and a branch side forked at C itself.
type hzGraph struct {
	f                       *scRepo
	c1, c2, c3, c4, m       libfossil.FslID
	feat, side1, side2, tip libfossil.FslID
}

func hzBranch(name string) []deck.TagCard {
	return []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: name},
		{Type: deck.TagPropagating, Name: "sym-" + name, UUID: "*"},
		{Type: deck.TagCancel, Name: "sym-trunk", UUID: "*"},
	}
}

func hzCommit(t *testing.T, f *scRepo, name string, parents []libfossil.FslID, tags []deck.TagCard) libfossil.FslID {
	t.Helper()
	f.when = f.when.Add(time.Hour)
	rid, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: name, Content: []byte(name + "\n")}},
		Comment: name, User: "u", Parent: parents[0], MergeParents: parents[1:],
		Time: f.when, Tags: tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

func newHZGraph(t *testing.T) *hzGraph {
	t.Helper()
	g := &hzGraph{f: newSCSyncRepo(t, scSyncFiles())}
	g.c1 = g.f.c1
	g.c2 = hzCommit(t, g.f, "c2", []libfossil.FslID{g.c1}, nil)
	g.c3 = hzCommit(t, g.f, "c3", []libfossil.FslID{g.c2}, nil)
	g.c4 = hzCommit(t, g.f, "c4", []libfossil.FslID{g.c3}, nil)
	g.feat = hzCommit(t, g.f, "feat", []libfossil.FslID{g.c1}, hzBranch("feat"))
	g.m = hzCommit(t, g.f, "m", []libfossil.FslID{g.c4, g.feat}, nil)
	g.side1 = hzCommit(t, g.f, "side1", []libfossil.FslID{g.c3}, hzBranch("side"))
	g.side2 = hzCommit(t, g.f, "side2", []libfossil.FslID{g.side1}, nil)
	return g
}

func hzWant(r *repo.Repo, c libfossil.FslID, rule cone.HorizonRule, want ...libfossil.FslID) error {
	got, err := horizon.HeldSet(context.Background(), r.DB(), c, rule)
	if err != nil {
		return err
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return expectf("%v held set = %v, want %v", rule, got, want)
	}
	return nil
}

// hzRules: descendants holds C, its trunk descendants (the merge m among
// them) and the branch forked at C; leaves adds the below-C branch feat,
// still an open leaf after its merge, back to c1, its nearest common
// ancestor with C. c2 is on no leaf's path and stays out under both.
func hzRules(t *testing.T) error {
	g := newHZGraph(t)
	if err := hzWant(g.f.r, g.c3, cone.RuleDescendants, g.c3, g.c4, g.m, g.side1, g.side2); err != nil {
		return err
	}
	return hzWant(g.f.r, g.c3, cone.RuleLeaves, g.c3, g.c4, g.m, g.side1, g.side2, g.feat, g.c1)
}

// hzUUIDs names a held set by artifact hash, so sets computed in two
// repositories compare.
func hzUUIDs(r *repo.Repo, c string, rule cone.HorizonRule) ([]string, error) {
	var rid libfossil.FslID
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", c).Scan(&rid); err != nil {
		return nil, err
	}
	k, err := horizon.HeldSet(context.Background(), r.DB(), rid, rule)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(k))
	for _, rid := range k {
		var uuid string
		if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			return nil, err
		}
		out = append(out, uuid)
	}
	slices.Sort(out)
	return out, nil
}

// hzRecomputed: a clone holding the origin's manifests computes the
// origin's held set from its own tables, and after a new check-in arrives
// both recompute a set that includes it. Nothing is stored between the
// computations; each reads the check-in graph as it then stands.
func hzRecomputed(t *testing.T) error {
	g := newHZGraph(t)
	peer := &scSyncPeer{r: g.f.r}
	clone, _ := scSyncClone(t, peer, sync.CloneOpts{})
	var c string
	if err := g.f.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", g.c3).Scan(&c); err != nil {
		return err
	}
	compare := func(stage string) ([]string, error) {
		var last []string
		for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
			atOrigin, err := hzUUIDs(g.f.r, c, rule)
			if err != nil {
				return nil, err
			}
			atClone, err := hzUUIDs(clone, c, rule)
			if err != nil {
				return nil, err
			}
			if !slices.Equal(atOrigin, atClone) {
				return nil, fmt.Errorf("%s %v: origin %v, clone %v", stage, rule, atOrigin, atClone)
			}
			last = atOrigin
		}
		return last, nil
	}
	before, err := compare("cloned")
	if err != nil {
		return err
	}
	g.tip = hzCommit(t, g.f, "tip", []libfossil.FslID{g.side2}, nil)
	if _, err := sync.Sync(context.Background(), clone, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	after, err := compare("pulled")
	if err != nil {
		return err
	}
	var tip string
	if err := g.f.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", g.tip).Scan(&tip); err != nil {
		return err
	}
	if slices.Contains(before, tip) || !slices.Contains(after, tip) || len(after) != len(before)+1 {
		return expectf("held set %v after the new check-in %s arrived, %v before", after, tip, before)
	}
	return nil
}
