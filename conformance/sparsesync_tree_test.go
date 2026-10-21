package conformance

import (
	"context"
	"errors"
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
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

func scSyncTreeAware(t *testing.T) error   { return scSyncTreeClonePull(t, false) }
func scSyncTreeIgnored(t *testing.T) error { return scSyncTreeClonePull(t, true) }

// Tree histories put out-of-cone leaves under a spine node and under an
// out-of-cone interior. The expected holdings come from the origin's decks:
// an aware server sends spines, in-cone nodes and in-cone blobs (6.1); against
// an ignorant one the requester elides exactly the blobs every F card names
// out of the cone and keeps every structural artifact, as zit does (6.3).
func scSyncTreeClonePull(t *testing.T, ignore bool) error {
	origin := newSCSyncTreeRepo(t)
	peer := &scSyncPeer{r: origin.r, ignore: ignore}
	c := scCone(t, []string{"src"}, nil)
	r, result := scSyncClone(t, peer, sync.CloneOpts{StoreCone: c, PullPhantoms: true})
	if err := scSyncTreeHoldings(t, origin.r, r, peer, c, ignore, result.Elided); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	files := scSyncTreeFiles()
	files["src/new"], files["zzz/new-shared"] = "new shared\n", "new shared\n"
	files["zzz/new"], files["yyy/e/g"] = "new outside\n", "new deep\n"
	origin.commit(t, files, origin.c1)
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if len(pulled.Errors) != 0 {
		return fmt.Errorf("pull errors: %q", pulled.Errors)
	}
	if err := scSyncTreeHoldings(t, origin.r, r, peer, c, ignore, pulled.Elided); err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	if _, err := scVerify(r); err != nil {
		return err
	}
	return nil
}

func scSyncTreeFiles() map[string]string {
	return map[string]string{
		"src/a": "inside\n", "src/shared": "shared\n", "zzz/shared": "shared\n",
		"zzz/b": "outside\n", "zzz/deep/c": "deep\n", "yyy/d": "y\n", "yyy/e/f": "yf\n",
	}
}

func newSCSyncTreeRepo(t *testing.T) *scRepo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "origin.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	for k, v := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
	} {
		if err := r.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	f := &scRepo{r: r, when: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	f.c1 = f.commit(t, scSyncTreeFiles(), 0)
	return f
}

// scSyncTreeOrigin is the origin's decks by uuid; plain blobs map to nil.
func scSyncTreeOrigin(t *testing.T, origin *repo.Repo) map[string]*deck.Deck {
	t.Helper()
	rows, err := origin.DB().Query("SELECT rid, uuid FROM blob WHERE size>=0")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[libfossil.FslID]string{}
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			t.Fatal(err)
		}
		ids[rid] = uuid
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	out := map[string]*deck.Deck{}
	for rid, uuid := range ids {
		data, err := content.Expand(origin.DB(), rid)
		if err != nil {
			t.Fatal(err)
		}
		d, err := deck.Parse(data)
		if err != nil {
			d = nil
		}
		out[uuid] = d
	}
	return out
}

// scSyncTreeExpect returns the holdings and the elided blobs each path must
// produce. A node is in-cone when a covered file lies beneath it.
func scSyncTreeExpect(all map[string]*deck.Deck, c *cone.Cone, ignore bool) (held, elided map[string]bool) {
	inside, outside := map[string]bool{}, map[string]bool{}
	for _, d := range all {
		if d == nil {
			continue
		}
		for _, f := range d.F {
			if c.Covers(f.Name) {
				inside[f.UUID] = true
			} else if f.UUID != "" {
				outside[f.UUID] = true
			}
		}
	}
	elided = map[string]bool{}
	for uuid := range outside {
		if !inside[uuid] {
			elided[uuid] = true
		}
	}
	held = map[string]bool{}
	for uuid, d := range all {
		switch {
		case ignore:
			held[uuid] = !elided[uuid]
		case d == nil:
			held[uuid] = inside[uuid]
		case d.Type == deck.Checkin:
			held[uuid] = true
		case d.Type == deck.Submanifest:
			held[uuid] = scSyncTreeCovers(all, d, c, 0)
		}
	}
	maps.DeleteFunc(held, func(_ string, v bool) bool { return !v })
	return held, elided
}

func scSyncTreeCovers(all map[string]*deck.Deck, d *deck.Deck, c *cone.Cone, depth int) bool {
	if d == nil || depth > 16 {
		return false
	}
	for _, f := range d.F {
		if c.Covers(f.Name) {
			return true
		}
	}
	for _, s := range d.S {
		if scSyncTreeCovers(all, all[s.UUID], c, depth+1) {
			return true
		}
	}
	return false
}

func scSyncTreeHoldings(t *testing.T, origin, r *repo.Repo, peer *scSyncPeer,
	c *cone.Cone, ignore bool, reported int,
) error {
	t.Helper()
	held, elided := scSyncTreeExpect(scSyncTreeOrigin(t, origin), c, ignore)
	var got []string
	if err := scSyncTreeQuery(r, "SELECT uuid FROM blob WHERE size>=0", &got); err != nil {
		return err
	}
	if err := scSameList("held artifacts", got, scSorted(held)); err != nil {
		return err
	}
	classes := scAbsences(t, r)
	if n := len(scClassed(classes, content.Elided)); n != len(classes) || n != reported {
		return fmt.Errorf("phantoms %v; reported elided %d", classes, reported)
	}
	if ignore {
		return scSameList("elided phantoms", scClassed(classes, content.Elided), scSorted(elided))
	}
	for uuid := range classes {
		if held[uuid] {
			return fmt.Errorf("in-cone artifact %s is a phantom", uuid)
		}
	}
	for _, m := range peer.requests {
		for uuid := range classes {
			if err := scSyncNoGimme([]*xfer.Message{m}, uuid); err != nil {
				return err
			}
		}
	}
	for _, m := range peer.replies {
		for uuid := range scSyncPayloads(m) {
			if !held[uuid] {
				return fmt.Errorf("aware server sent out-of-cone artifact %s", uuid)
			}
		}
	}
	return nil
}

func scSyncTreeQuery(r *repo.Repo, query string, out *[]string) error {
	rows, err := r.DB().Query(query)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return errors.Join(err, rows.Close())
		}
		*out = append(*out, s)
	}
	slices.Sort(*out)
	return errors.Join(rows.Err(), rows.Close())
}
