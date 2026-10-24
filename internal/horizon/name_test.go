package horizon_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
)

func (d *dag) uuid(rid libfossil.FslID) string {
	d.t.Helper()
	var uuid string
	if err := d.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		d.t.Fatal(err)
	}
	return uuid
}

// TestResolveName resolves names as draft-fossil-horizon-00 §6.2.7 does: tip
// is the most recent check-in, a full hash names its check-in, and any other
// name is the most recent check-in carrying sym-NAME; depth then steps back
// along primary parents, stopping at a root.
func TestResolveName(t *testing.T) {
	d := newDAG(t)
	trunk := d.chain("c1", "c2", "c3", "c4")
	feat := d.commit("feat", p(trunk[1]), branchTags("feat")...)
	m := d.commit("m", p(trunk[3], feat))
	rel := d.commit("rel", p(m), deck.TagCard{Type: deck.TagSingleton, Name: "sym-v1", UUID: "*"})
	side := d.commit("side", p(trunk[2]), branchTags("side")...)

	for _, tc := range []struct {
		name  string
		depth uint32
		want  libfossil.FslID
		steps uint32
	}{
		{"tip", 0, side, 0},
		{"trunk", 0, rel, 0},
		{"trunk", 3, trunk[2], 3}, // rel -> m -> c4 -> c3: m's primary parent is c4
		{"feat", 0, feat, 0},
		{"v1", 1, m, 1},
		{"side", 0, side, 0},
		{d.uuid(trunk[3]), 2, trunk[1], 2},
		{strings.ToUpper(d.uuid(trunk[3])), 0, trunk[3], 0},
		{"side", 10, trunk[0], 3}, // side -> c3 -> c2 -> c1, the root
	} {
		got, err := horizon.ResolveName(d.r.DB(), tc.name, tc.depth)
		if err != nil {
			t.Fatalf("ResolveName(%.12s, %d): %v", tc.name, tc.depth, err)
		}
		if got.Hash != d.uuid(tc.want) || got.Steps != tc.steps {
			t.Errorf("ResolveName(%.12s, %d) = %.12s after %d steps, want %.12s after %d",
				tc.name, tc.depth, got.Hash, got.Steps, d.uuid(tc.want), tc.steps)
		}
	}
	// A file's hash names no check-in, and nothing carries sym-nope.
	var file string
	if err := d.r.DB().QueryRow(
		"SELECT uuid FROM blob WHERE rid NOT IN (SELECT objid FROM event) LIMIT 1").Scan(&file); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nope", file, ""} {
		if _, err := horizon.ResolveName(d.r.DB(), name, 0); !errors.Is(err, horizon.ErrNameNotFound) {
			t.Errorf("ResolveName(%q) = %v, want ErrNameNotFound", name, err)
		}
	}
}

// TestResolveNameCancelled: a cancelled branch tag no longer names the
// check-ins below the cancellation.
func TestResolveNameCancelled(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2")
	d.commit("x", p(c[1]), branchTags("x")...)
	got, err := horizon.ResolveName(d.r.DB(), "trunk", 0)
	if err != nil || got.Hash != d.uuid(c[1]) {
		t.Fatalf("trunk = %.12s, %v; want c2 (x cancels sym-trunk)", got.Hash, err)
	}
}
