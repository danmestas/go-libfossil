package conformance

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// scOneArtifactRounds shrinks every clone batch to one artifact, so each
// check-in and each artifact its walk reaches lands in a round of its own.
type scOneArtifactRounds struct{}

func (scOneArtifactRounds) Check(site string, _ float64) bool {
	return site == "handler.emitCloneBatch.smallBatch"
}

func scProtocolParentDiffFlat(t *testing.T) error {
	return scProtocolParentDiff(t, newSCSyncRepo(t, scSyncFiles()), scSyncFiles())
}

func scProtocolParentDiffTree(t *testing.T) error {
	return scProtocolParentDiff(t, newSCSyncTreeRepo(t), scSyncTreeFiles())
}

// A check-in sends only what its primary parent's tree does not hold: an
// unchanged in-cone file crosses the wire once however many check-ins name
// it, while a check-in whose walk outgrows a round resumes in the next.
func scProtocolParentDiff(t *testing.T, origin *scRepo, base map[string]string) error {
	parent := origin.c1
	for i := 0; i < 3; i++ {
		base[fmt.Sprintf("src/v%d", i)] = fmt.Sprintf("version %d\n", i)
		base[fmt.Sprintf("zzz/v%d", i)] = fmt.Sprintf("outside %d\n", i)
		parent = origin.commit(t, base, parent)
	}
	peer := &scSyncPeer{r: origin.r, opts: sync.HandleOpts{Buggify: scOneArtifactRounds{}}}
	r, _ := scSyncClone(t, peer, sync.CloneOpts{StoreCone: scCone(t, []string{"src"}, nil)})
	if err := scNoTransit(r); err != nil {
		return err
	}
	unchanged := scTextUUID(origin.r, "inside\n")
	sent, resumed := 0, false
	for _, resp := range peer.replies {
		if _, ok := scSyncPayloads(resp)[unchanged]; ok {
			sent++
		}
		for _, card := range resp.Cards {
			if c, ok := card.(*xfer.CookieCard); ok && strings.Count(c.Value, ":") == 3 {
				resumed = true
			}
		}
	}
	if sent != 1 || !resumed {
		return fmt.Errorf("unchanged in-cone file sent %d times, walk resumed=%v; want once, true", sent, resumed)
	}
	for text, want := range map[string]bool{"inside\n": true, "version 2\n": true, "outside 2\n": false} {
		_, held, err := content.CheckAvailableByUUID(r.DB(), scTextUUID(origin.r, text))
		if err != nil || held != want {
			return fmt.Errorf("holding %q = %v, want %v (error %v)", text, held, want, err)
		}
	}
	return nil
}

// scTextUUID names text as origin stored it, under either hash.
func scTextUUID(origin *repo.Repo, text string) string {
	uuid := hash.SHA3([]byte(text))
	if _, ok := content.AvailableByUUID(origin.DB(), uuid); ok {
		return uuid
	}
	return hash.SHA1([]byte(text))
}

// Committing deltifies each manifest against its successor, so the first
// check-in's chain puts every later manifest on the wire ahead of its row.
// Each still walks its tree: without pulling phantoms afterwards, the clone
// holds every check-in's in-cone files.
func scProtocolDeltaManifestFlat(t *testing.T) error {
	return scProtocolDeltaManifestWalk(t, newSCSyncRepo(t, scSyncFiles()), scSyncFiles())
}

func scProtocolDeltaManifestTree(t *testing.T) error {
	return scProtocolDeltaManifestWalk(t, newSCSyncTreeRepo(t), scSyncTreeFiles())
}

func scProtocolDeltaManifestWalk(t *testing.T, origin *scRepo, files map[string]string) error {
	parent := origin.c1
	for i := 0; i < 3; i++ {
		files["src/a"] = fmt.Sprintf("inside v%d\n", i)
		parent = origin.commit(t, files, parent)
	}
	peer := &scSyncPeer{r: origin.r}
	r, _ := scSyncClone(t, peer, sync.CloneOpts{StoreCone: scCone(t, []string{"src"}, nil)})
	if err := scNoTransit(r); err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		text := fmt.Sprintf("inside v%d\n", i)
		if _, held, err := content.CheckAvailableByUUID(r.DB(), scTextUUID(origin.r, text)); err != nil || !held {
			return fmt.Errorf("holding %q = %v (error %v), want held", text, held, err)
		}
	}
	return nil
}

// scNoTransit holds a clone made without pulling phantoms to the walk alone:
// whatever the cone needs arrived in the clone rounds themselves.
func scNoTransit(r *repo.Repo) error {
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		return err
	}
	if n := a.Count(content.Transit); n != 0 {
		return fmt.Errorf("clone rounds left %d transit phantoms", n)
	}
	return nil
}
