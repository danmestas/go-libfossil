package libfossil

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

type compactFixture struct {
	source, target *Repo
	parent         int64
	peer           Transport
	files          map[string]string
}

// The tree has out-of-cone leaves under both a spine node and an out-of-cone
// interior, and one blob named both inside and outside the cone.
func newCompactFixture(t *testing.T) compactFixture {
	t.Helper()
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	for key, value := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
	} {
		if err := source.inner.SetConfig(key, value); err != nil {
			t.Fatal(err)
		}
	}
	f := compactFixture{source: source, files: map[string]string{
		"src/a": "inside\n", "src/shared": "shared\n", "zzz/shared": "shared\n",
		"zzz/b": "outside\n", "zzz/deep/c": "deep\n", "yyy/d": "y\n", "yyy/e/f": "yf\n",
	}}
	var files []FileToCommit
	for _, name := range slices.Sorted(maps.Keys(f.files)) {
		files = append(files, FileToCommit{Name: name, Content: []byte(f.files[name])})
	}
	rid, _, err := source.Commit(CommitOpts{Files: files, User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f.peer = TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return source.HandleSync(ctx, payload)
	})
	f.target, _, err = Clone(context.Background(), filepath.Join(t.TempDir(), "target.fossil"),
		f.peer, CloneOpts{PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.target.Close() })
	f.parent = rid
	return f
}

// compactHeld lists every artifact holding as uuid:size, in rid order.
func compactHeld(t *testing.T, r *Repo) []string {
	t.Helper()
	rows, err := r.DB().Query("SELECT uuid, size FROM blob ORDER BY rid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uuid string
		var size int64
		if err := rows.Scan(&uuid, &size); err != nil {
			t.Fatal(err)
		}
		if size >= 0 {
			out = append(out, uuid)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// compactZitDrops is zit's narrow-then-compact rule computed from the held
// artifacts, not from the compactor: every structural artifact stays, and a
// blob goes only when every F card naming it is out of the cone.
func compactZitDrops(t *testing.T, r *Repo, c *Cone) map[string]bool {
	t.Helper()
	inside, outside := map[string]bool{}, map[string]bool{}
	for _, uuid := range compactHeld(t, r) {
		rid, ok := content.AvailableByUUID(r.DB(), uuid)
		if !ok {
			t.Fatalf("held artifact %s is unavailable", uuid)
		}
		data, err := content.Expand(r.DB(), rid)
		if err != nil {
			t.Fatal(err)
		}
		d, err := deck.Parse(data)
		if err != nil {
			continue
		}
		for _, card := range d.F {
			if c.Covers(card.Name) {
				inside[card.UUID] = true
			} else if card.UUID != "" {
				outside[card.UUID] = true
			}
		}
	}
	for uuid := range inside {
		delete(outside, uuid)
	}
	return outside
}

func TestRepoCompactStore(t *testing.T) {
	f := newCompactFixture(t)
	ctx := context.Background()
	dense := compactHeld(t, f.target)
	if _, err := f.target.CompactStore(ctx); !errors.Is(err, ErrNoStoreCone) {
		t.Fatalf("dense compaction = %v, want ErrNoStoreCone", err)
	}
	narrow, err := NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.target.SetStoreCone(narrow); err != nil {
		t.Fatal(err)
	}
	if got := compactHeld(t, f.target); !slices.Equal(got, dense) {
		t.Fatal("narrowing changed holdings")
	}
	drops := compactZitDrops(t, f.target, narrow)
	if len(drops) != 4 {
		t.Fatalf("fixture has %d out-of-cone-only blobs, want 4", len(drops))
	}
	n, err := f.target.CompactStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, uuid := range dense {
		if !drops[uuid] {
			want = append(want, uuid)
		}
	}
	if got := compactHeld(t, f.target); n != len(drops) || !slices.Equal(got, want) {
		t.Fatalf("compacted %d; held %v, want %v", n, got, want)
	}
	compactElided(t, f.target, drops)
	if err := f.target.Verify(); err != nil {
		t.Fatalf("verify after compaction: %v", err)
	}
	if n, err := f.target.CompactStore(ctx); err != nil || n != 0 {
		t.Fatalf("repeat compaction = %d, %v", n, err)
	}
	compactWiden(t, f, narrow)
}

func compactElided(t *testing.T, r *Repo, drops map[string]bool) {
	t.Helper()
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	if a.Count(content.Elided) != len(drops) || a.Count(content.Transit) != 0 {
		t.Fatalf("absences elided=%d transit=%d, want %d and 0",
			a.Count(content.Elided), a.Count(content.Transit), len(drops))
	}
	for uuid := range drops {
		rid, ok := content.AvailableByUUID(r.DB(), uuid)
		if ok {
			t.Fatalf("dropped artifact %s is still available", uuid)
		}
		var phantom fsltype.FslID
		if err := r.DB().QueryRow("SELECT b.rid FROM blob b JOIN phantom p ON p.rid=b.rid WHERE b.uuid=?",
			uuid).Scan(&phantom); err != nil || a.Class(phantom) != content.Elided {
			t.Fatalf("dropped artifact %s (%d) is not an elided phantom: %v", uuid, rid, err)
		}
	}
}

// Widening through the public checkout recovers every compacted payload.
func compactWiden(t *testing.T, f compactFixture, narrow *Cone) {
	t.Helper()
	co, err := f.target.CreateCheckout(t.TempDir(), CheckoutCreateOpts{Cone: narrow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(f.parent, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.SetConeContext(context.Background(), nil, RequiredContentOpts{Peer: f.peer}); err != nil {
		t.Fatal(err)
	}
	for name, text := range f.files {
		data, err := os.ReadFile(filepath.Join(co.Dir(), name))
		if err != nil || string(data) != text {
			t.Fatalf("widened %s = %q, %v", name, data, err)
		}
	}
	if err := f.target.Verify(); err != nil {
		t.Fatalf("verify after widening: %v", err)
	}
	a, err := content.LoadAbsences(f.target.DB())
	if err != nil {
		t.Fatal(err)
	}
	if a.Count(content.Elided) != 0 || a.Count(content.Transit) != 0 {
		t.Fatalf("after widening elided=%d transit=%d", a.Count(content.Elided), a.Count(content.Transit))
	}
}
