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

func maxRID(t *testing.T, r *repo.Repo) libfossil.FslID {
	t.Helper()
	var n libfossil.FslID
	if err := r.DB().QueryRow("SELECT max(rid) FROM blob").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
