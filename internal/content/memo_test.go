package content_test

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// memoRepo is a store that grows by check-ins written in order, one file
// changing per check-in, in one of the manifest forms hzMode names.
type memoRepo struct {
	t    *testing.T
	r    *repo.Repo
	mode hzMode
	when time.Time
	tips []libfossil.FslID
	n    int
	// wide, when set, holds the files of a wide tree that every check-in
	// renames, deletes or adds one of, and changes another.
	wide map[string]string
}

func newMemoRepo(t *testing.T, mode hzMode) *memoRepo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "memo.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if mode == hzTree {
		for k, v := range map[string]string{
			manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
		} {
			if err := r.SetConfig(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &memoRepo{t: t, r: r, mode: mode, when: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

// commit checks in on parent (0 for a root) a tree of a fixed file set,
// one of whose files changes.
func (m *memoRepo) commit(parent libfossil.FslID) libfossil.FslID {
	m.t.Helper()
	m.n++
	m.when = m.when.Add(time.Hour)
	files := []manifest.File{
		{Name: "lib/x.c", Content: []byte("x\n")}, {Name: "lib/y.c", Content: []byte("y\n")},
		{Name: "src/a.c", Content: []byte("a\n")}, {Name: "src/b.c", Content: []byte("b\n")},
	}
	files[m.n%len(files)].Content = []byte(fmt.Sprintf("v%d\n", m.n))
	if m.wide != nil {
		files = m.widen()
	}
	rid, _, err := manifest.Checkin(m.r, manifest.CheckinOpts{
		Files: files, Comment: fmt.Sprintf("c%d", m.n), User: "u", Parent: parent, Time: m.when,
		Delta: m.mode == hzDelta && parent != 0,
	})
	if err != nil {
		m.t.Fatal(err)
	}
	m.tips = append(m.tips, rid)
	return rid
}

// widen evolves the wide tree by one check-in and returns its files.
func (m *memoRepo) widen() []manifest.File {
	names := slices.Sorted(maps.Keys(m.wide))
	pick := names[(m.n*7)%len(names)]
	switch m.n % 4 {
	case 0:
		m.wide[fmt.Sprintf("lib/r%d.c", m.n)] = m.wide[pick]
		delete(m.wide, pick)
	case 1:
		if len(m.wide) > 24 {
			delete(m.wide, pick)
		}
	case 2:
		m.wide[fmt.Sprintf("src/n%d.c", m.n)] = fmt.Sprintf("n%d\n", m.n)
	}
	names = slices.Sorted(maps.Keys(m.wide))
	m.wide[names[(m.n*3)%len(names)]] = fmt.Sprintf("v%d\n", m.n)
	files := make([]manifest.File, 0, len(m.wide))
	for _, name := range slices.Sorted(maps.Keys(m.wide)) {
		files = append(files, manifest.File{Name: name, Content: []byte(m.wide[name])})
	}
	return files
}

func (m *memoRepo) uuid(rid libfossil.FslID) string {
	m.t.Helper()
	var uuid string
	if err := m.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		m.t.Fatal(err)
	}
	return uuid
}

// memoCones are the cones the tests classify: a path cone, and C's
// horizons under both rules and both depths, with and without paths.
func (m *memoRepo) memoCones(c libfossil.FslID) []*cone.Cone {
	m.t.Helper()
	paths, err := cone.New([]string{"src"}, nil)
	if err != nil {
		m.t.Fatal(err)
	}
	out := []*cone.Cone{paths}
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		for _, depth := range []cone.HorizonDepth{cone.DepthContent, cone.DepthStructural} {
			h, err := cone.NewHorizon(m.uuid(c), rule, depth)
			if err != nil {
				m.t.Fatal(err)
			}
			for _, include := range [][]string{nil, {"src"}} {
				hc, err := cone.NewWithHorizon(include, nil, h)
				if err != nil {
					m.t.Fatal(err)
				}
				out = append(out, hc)
			}
		}
	}
	return out
}

// sameClassification fails unless the view classifies c as a fresh read
// of the store does: every artifact's Outside, Required and below answer,
// and the held set and reach of its horizon.
func sameClassification(t *testing.T, r *repo.Repo, v *content.MemoView, c *cone.Cone) {
	t.Helper()
	ctx := context.Background()
	q := r.DB()
	fresh, err := content.LoadConeReferences(ctx, q, c)
	if err != nil {
		t.Fatal(err)
	}
	memo, err := v.References(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var freshBelow, memoBelow func(libfossil.FslID) bool
	if _, ok := c.Horizon(); ok {
		if freshBelow, err = fresh.HorizonBelow(ctx, q, c); err != nil {
			t.Fatal(err)
		}
		if memoBelow, err = v.HorizonBelow(ctx, q, memo, c); err != nil {
			t.Fatal(err)
		}
		if (freshBelow == nil) != (memoBelow == nil) {
			t.Fatalf("%s: placed fresh=%v memo=%v", c, freshBelow != nil, memoBelow != nil)
		}
		h, _ := c.Horizon()
		fk, freach, fplaced, err := content.HorizonReach(ctx, q, h)
		if err != nil {
			t.Fatal(err)
		}
		mk, mreach, mplaced, err := v.HorizonReach(ctx, q, h)
		if err != nil {
			t.Fatal(err)
		}
		if fplaced != mplaced || !slices.Equal(fk, mk) || !maps.Equal(freach, mreach) {
			t.Fatalf("%s: reach fresh K=%v placed=%v, memo K=%v placed=%v", c, fk, fplaced, mk, mplaced)
		}
	}
	rows, err := q.Query("SELECT rid FROM blob ORDER BY rid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			t.Fatal(err)
		}
		if fresh.Outside(rid) != memo.Outside(rid) || fresh.Required(rid) != memo.Required(rid) {
			t.Fatalf("%s rid=%d: outside %v/%v required %v/%v (fresh/memo)", c, rid,
				fresh.Outside(rid), memo.Outside(rid), fresh.Required(rid), memo.Required(rid))
		}
		if freshBelow != nil && freshBelow(rid) != memoBelow(rid) {
			t.Fatalf("%s rid=%d: below fresh=%v memo=%v", c, rid, freshBelow(rid), memoBelow(rid))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// draft-fossil-horizon-00 §6.2: a memo extended by each appended check-in
// classifies every cone as a fresh read of the store does, in every manifest
// form, and reads only what was appended.
func TestMemoExtendsExactly(t *testing.T) {
	for _, mode := range []hzMode{hzFull, hzDelta, hzTree} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			m := newMemoRepo(t, mode)
			root := m.commit(0)
			c := m.commit(root)
			memo := content.NewMemo()
			v, err := memo.View(ctx, m.r.DB())
			if err != nil {
				t.Fatal(err)
			}
			if v.Read() == 0 {
				t.Fatal("the first view read nothing")
			}
			for _, sc := range m.memoCones(c) {
				sameClassification(t, m.r, v, sc)
			}
			v.Publish()
			fork := root
			for i := 0; i < 4; i++ {
				before := maxRID(t, m.r)
				tip := m.commit(m.tips[len(m.tips)-1])
				if i == 2 {
					fork = m.commit(fork) // a branch from below C
				}
				v, err := memo.View(ctx, m.r.DB())
				if err != nil {
					t.Fatal(err)
				}
				if appended := int(maxRID(t, m.r) - before); v.Read() != appended {
					t.Fatalf("after check-in %d the view read %d artifacts, want the %d appended", tip, v.Read(), appended)
				}
				for _, sc := range m.memoCones(c) {
					sameClassification(t, m.r, v, sc)
				}
				v.Publish()
			}
			again, err := memo.View(ctx, m.r.DB())
			if err != nil {
				t.Fatal(err)
			}
			if again.Read() != 0 {
				t.Fatalf("an unchanged store read %d artifacts", again.Read())
			}
		})
	}
}

// A view that is not published leaves the memo as it was: what an origin
// classified inside a request that rolls back is never reused.
func TestMemoUnpublishedViewDiscarded(t *testing.T) {
	ctx := context.Background()
	m := newMemoRepo(t, hzFull)
	c := m.commit(0)
	memo := content.NewMemo()
	v, err := memo.View(ctx, m.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	v.Publish()
	before := maxRID(t, m.r)
	m.commit(c)
	if _, err := memo.View(ctx, m.r.DB()); err != nil {
		t.Fatal(err)
	}
	v, err = memo.View(ctx, m.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	if appended := int(maxRID(t, m.r) - before); v.Read() != appended {
		t.Fatalf("view after an unpublished one read %d, want %d", v.Read(), appended)
	}
}

// draft-fossil-horizon-00 §6.2: the memo keeps the trees its horizons'
// walks read, so a second horizon over the same store, under another C or
// rule, reads from the store only the trees the first did not, and
// classifies as a fresh read does. Past its tree limit the memo reads
// trees again, and classifies alike.
func TestMemoTreesReadOnce(t *testing.T) {
	for _, mode := range []hzMode{hzFull, hzDelta, hzTree} {
		for _, limited := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/limited=%v", mode, limited), func(t *testing.T) {
				ctx := context.Background()
				m := newMemoRepo(t, mode)
				tip := m.commit(0)
				for i := 0; i < 5; i++ {
					tip = m.commit(tip)
				}
				memo := content.NewMemo()
				if limited {
					memo.LimitTrees(400)
				}
				seen := map[string]bool{}
				again := 0
				for _, c := range []libfossil.FslID{m.tips[3], m.tips[1], m.tips[0], m.tips[3]} {
					v, err := memo.View(ctx, m.r.DB())
					if err != nil {
						t.Fatal(err)
					}
					for _, sc := range m.memoCones(c) {
						sameClassification(t, m.r, v, sc)
					}
					names, reads := v.TreesRead()
					if reads != len(names) {
						t.Fatalf("horizon at %d: %d tree reads of %d trees", c, reads, len(names))
					}
					if len(seen) == 0 && len(names) == 0 {
						t.Fatalf("the first horizon, at %d, read no tree", c)
					}
					for _, name := range names {
						if seen[name] {
							again++
						}
						seen[name] = true
					}
					v.Publish()
					if limited && memo.TreeBytes() > 400 {
						t.Fatalf("the memo keeps %d tree bytes past its limit", memo.TreeBytes())
					}
				}
				if !limited && again > 0 {
					t.Fatalf("%d trees read again", again)
				}
				if limited && again == 0 {
					t.Fatal("no tree read again past the limit")
				}
			})
		}
	}
}

// draft-fossil-horizon-00 §6.2: over wide trees that rename, delete and
// add files, in every manifest form, the memo's trees (kept as differences
// along chains of the longest length, or past a small limit started again
// on most publishes) classify every cone as a fresh read does, before and
// after the store gains check-ins. A branch's newer trees head the chains
// that trunk trees continue, so a trunk horizon decodes trees whose chain
// starts outside its held set.
func TestMemoWideTreesMatchFresh(t *testing.T) {
	for _, mode := range []hzMode{hzFull, hzDelta, hzTree} {
		for _, limit := range []int64{0, 6000} {
			t.Run(fmt.Sprintf("%s/limit=%d", mode, limit), func(t *testing.T) {
				ctx := context.Background()
				m := newMemoRepo(t, mode)
				m.wide = map[string]string{}
				for i := 0; i < 30; i++ {
					m.wide[fmt.Sprintf("src/f%02d.c", i)] = fmt.Sprintf("f%d\n", i)
				}
				tip := m.commit(0)
				for i := 0; i < 19; i++ {
					tip = m.commit(tip)
				}
				branch := m.tips[5]
				for i := 0; i < 6; i++ {
					branch = m.commit(branch)
				}
				memo := content.NewMemo()
				if limit > 0 {
					memo.LimitTrees(limit)
				}
				classify := func(cs ...libfossil.FslID) {
					t.Helper()
					for _, c := range cs {
						v, err := memo.View(ctx, m.r.DB())
						if err != nil {
							t.Fatal(err)
						}
						for _, sc := range m.memoCones(c) {
							sameClassification(t, m.r, v, sc)
						}
						v.Publish()
						if limit > 0 && memo.TreeBytes() > limit {
							t.Fatalf("the memo keeps %d tree bytes past %d", memo.TreeBytes(), limit)
						}
					}
				}
				classify(m.tips[0], m.tips[12], m.tips[6], m.tips[22])
				if depth := memo.TreeChainDepth(); limit == 0 && mode == hzFull && depth != 15 {
					t.Fatalf("longest chain of differences %d, want 15", depth)
				}
				for i := 0; i < 4; i++ {
					tip, branch = m.commit(tip), m.commit(branch)
				}
				classify(m.tips[0], m.tips[14], m.tips[23], m.tips[3])
			})
		}
	}
}

func maxRID(t *testing.T, r *repo.Repo) libfossil.FslID {
	t.Helper()
	var n libfossil.FslID
	if err := r.DB().QueryRow("SELECT max(rid) FROM blob").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A store with no non-artifact record proves file versions by the tables an
// artifact's crosslink writes, and those change when a sweep links an
// artifact late: a checked-in file whose bytes are a cluster stays file
// content until a sweep claims it. A memo extended past that sweep must
// classify as a fresh read does.
func TestMemoFossilWrittenStoreFollowsLateClaims(t *testing.T) {
	ctx := context.Background()
	m := newMemoRepo(t, hzFull)
	root := m.commit(0)
	cluster, err := (&deck.Deck{Type: deck.Cluster, M: []string{m.uuid(root)}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m.when = m.when.Add(time.Hour)
	tip, _, err := manifest.Checkin(m.r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "lib/cluster.txt", Content: cluster}, {Name: "src/a.c", Content: []byte("a\n")}},
		Comment: "a file that parses as a cluster", User: "u", Parent: root, Time: m.when,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.tips = append(m.tips, tip)
	if _, err := m.r.DB().Exec("DROP TABLE IF EXISTS crosslink_nonartifact_v2"); err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	memo := content.NewMemo()
	v, err := memo.View(ctx, m.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	sameClassification(t, m.r, v, c)
	v.Publish()
	if _, err := manifest.Crosslink(m.r); err != nil {
		t.Fatal(err)
	}
	for _, grow := range []bool{false, true} {
		if grow {
			m.commit(tip)
		}
		v, err := memo.View(ctx, m.r.DB())
		if err != nil {
			t.Fatal(err)
		}
		sameClassification(t, m.r, v, c)
		v.Publish()
	}
}

// A store with no non-artifact record takes a version mlink names and no
// claim table names as the file it is, read or not: a checked-in file whose
// bytes parse as a cluster, which no sweep linked as one, is elided outside
// the cone like any other file (loadFiles), not kept as a cluster.
func TestFossilWrittenStoreTakesUnlinkedArtifactBytesAsFile(t *testing.T) {
	m := newMemoRepo(t, hzFull)
	root := m.commit(0)
	cluster, err := (&deck.Deck{Type: deck.Cluster, M: []string{m.uuid(root)}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manifest.Checkin(m.r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "lib/cluster.txt", Content: cluster}, {Name: "src/a.c", Content: []byte("a\n")}},
		Comment: "a file that parses as a cluster", User: "u", Parent: root, Time: m.when.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.r.DB().Exec("DROP TABLE IF EXISTS crosslink_nonartifact_v2"); err != nil {
		t.Fatal(err)
	}
	var rid libfossil.FslID
	if err := m.r.DB().QueryRow(`SELECT fid FROM mlink JOIN filename USING(fnid)
		WHERE name='lib/cluster.txt'`).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := content.LoadConeReferences(context.Background(), m.r.DB(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !refs.Outside(rid) || refs.Required(rid) {
		t.Fatalf("lib/cluster.txt: outside=%v required=%v, want an elided file", refs.Outside(rid), refs.Required(rid))
	}
}
