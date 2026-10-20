package conformance

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// Appendix A.6 specifies inputs rather than pre-authored artifact files.
// Each case reaches the checkout writer and compares its complete closure
// with full construction, or checks an absent-node refusal is atomic.
func smSparseCanonicalCases() []smCase {
	var cases []smCase
	for _, vector := range []string{"max", "final-merge", "different-k", "unknown-k1", "unknown-k2"} {
		for _, absent := range []bool{false, true} {
			mode := "dense"
			if absent {
				mode = "absent-node"
			}
			cases = append(cases, smWriterCase("submanifest.a6."+vector+"."+mode, "SM-A-5", func(t *testing.T) error {
				return smSparseCanonical(t, vector, absent)
			}))
		}
	}
	cases = append(cases,
		smWriterCase("submanifest.6.2.sparse-structural-range", "SM-6.2-8", func(t *testing.T) error {
			return smSparseCanonical(t, "max", true)
		}),
		smWriterCase("submanifest.6.2.sparse-partition-evidence", "SM-6.2-9", func(t *testing.T) error {
			return smSparseCanonical(t, "unknown-k2", true)
		}),
	)
	return cases
}

func scCanonicalCases() []scCase {
	var cases []scCase
	for _, c := range smSparseCanonicalCases() {
		cases = append(cases, scCase{Case{ID: "sparsecone.4.2." + c.ID[len("submanifest."):], Req: "SC-4.2-3"}, c.run})
	}
	cases = append(cases,
		scCase{Case{ID: "sparsecone.5.3.canonical-absent-node", Req: "SC-5.3-1"}, func(t *testing.T) error {
			return smSparseCanonical(t, "max", true)
		}},
		scCase{Case{ID: "sparsecone.5.1.retained-outside-payloads", Req: "SC-5.1-1"}, scRetainedOutsidePayloads},
	)
	return cases
}

func scRetainedOutsidePayloads(t *testing.T) error {
	f := newSCRepo(t, true)
	c := scDefault(t)
	if err := content.SetStoreCone(f.r.DB(), c); err != nil {
		return err
	}
	d, err := manifest.GetManifest(f.r, f.c1)
	if err != nil {
		return err
	}
	held := 0
	for i, card := range d.S {
		limit := ""
		if i+1 < len(d.S) {
			limit = d.S[i+1].Name
		}
		if !c.MayCover(card.Name, limit) {
			if _, ok := content.AvailableByUUID(f.r.DB(), card.UUID); !ok {
				return fmt.Errorf("already-held outside node %s was discarded", card.UUID)
			}
			held++
		}
	}
	if held == 0 {
		return fmt.Errorf("retention fixture has no wholly outside held node")
	}
	_, err = scVerify(f.r)
	return err
}

var smMaxNames = []string{
	"f0000", "f0001", "f0003", "f0004", "f0005", "f0006", "f0008", "f0010", "f0014",
	"f0015", "f0016", "f0017", "f0018", "f0019", "f0021", "f0022", "f0023", "f0024",
}

func smSparseCanonical(t *testing.T, vector string, absent bool) error {
	old := map[string]string{}
	for _, name := range smMaxNames {
		if smZbits(name) != 0 {
			return fmt.Errorf("A.6 name %s has zbits %d, want zero", name, smZbits(name))
		}
		if name != "f0001" {
			old[name] = name
		}
	}
	parentK, requestedK := 1, 1
	if vector == "final-merge" {
		old = map[string]string{"g0000": "g0000", "g0001": "g0001", "g0005": "g0005", "g0006": "g0006"}
		parentK, requestedK = 3, 3
	}
	if vector == "different-k" || vector == "unknown-k2" {
		requestedK = 2
	}
	r, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, strconv.Itoa(parentK))
	if err != nil {
		return err
	}
	parent, d, err := smCommit(r, old, 0, smTime, false)
	if err != nil {
		return err
	}
	wantSizes := []int{8, 8, 1}
	if vector == "final-merge" {
		wantSizes = []int{2, 2}
	}
	if err := smCanonicalLeafSizes(r, d.S, wantSizes); err != nil {
		return err
	}
	if vector == "unknown-k1" || vector == "unknown-k2" {
		// Reauthor the immutable root outside the writer: its S closure is
		// valid, but local derivation evidence for the old identity cannot
		// establish the construction basis of this imported identity.
		d.C = "externally authored parent"
		data, err := d.Marshal()
		if err != nil {
			return err
		}
		if err := r.WithTx(func(tx *db.Tx) error {
			parent, _, err = blob.Store(tx, data)
			return err
		}); err != nil {
			return err
		}
		if _, err := manifest.Crosslink(r); err != nil {
			return err
		}
	}
	if err := r.SetConfig(manifest.SettingSubmanifestK, strconv.Itoa(requestedK)); err != nil {
		return err
	}
	var c *cone.Cone
	if absent {
		include := []string{"f0000"}
		if vector == "max" {
			include = smMaxNames[:9]
		} else if vector == "final-merge" {
			include = []string{"g0005", "g0006"}
		}
		c = scCone(t, include, nil)
		if err := content.SetStoreCone(r.DB(), c); err != nil {
			return err
		}
	}
	f := &scRepo{r: r, c1: parent, when: smTime}
	co := f.open(t, c, parent)
	if absent {
		cards := d.S[1:]
		if vector == "final-merge" {
			cards = d.S[:1]
		}
		for _, card := range cards {
			if err := smElideCanonicalNode(r, card.UUID); err != nil {
				return err
			}
		}
	}
	next := map[string]string{}
	for name, text := range old {
		next[name] = text
	}
	switch vector {
	case "max":
		scWrite(t, co.Dir(), "f0001", "f0001")
		if _, err := co.Manage(checkout.ManageOpts{Paths: []string{"f0001"}}); err != nil {
			return err
		}
		next["f0001"] = "f0001"
	case "final-merge":
		if err := co.Unmanage(checkout.UnmanageOpts{Paths: []string{"g0006"}}); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(co.Dir(), "g0006")); err != nil {
			return err
		}
		delete(next, "g0006")
	default:
		scWrite(t, co.Dir(), "f0000", "replacement")
		next["f0000"] = "replacement"
	}
	before, err := smCanonicalState(r, co)
	if err != nil {
		return err
	}
	rid, uuid, err := co.Commit(checkout.CommitOpts{Message: "A.6", User: "u", Time: smTime.Add(time.Hour)})
	if err != nil {
		if !absent {
			return fmt.Errorf("dense A.6 %s refused: %w", vector, err)
		}
		if _, missing := manifest.MissingSubmanifest(err); !missing {
			return fmt.Errorf("absent-node refusal does not identify its missing sub-manifest: %w", err)
		}
		after, stateErr := smCanonicalState(r, co)
		if stateErr != nil {
			return stateErr
		}
		if rid != 0 || uuid != "" || before != after {
			return fmt.Errorf("refusal published state: rid=%d uuid=%q before=%s after=%s", rid, uuid, before, after)
		}
		return nil
	}
	return smCanonicalClosure(r, rid, next, requestedK)
}

func smElideCanonicalNode(r *repo.Repo, uuid string) error {
	if err := r.WithTx(func(tx *db.Tx) error {
		for _, sql := range []string{
			"DELETE FROM delta WHERE rid=(SELECT rid FROM blob WHERE uuid=?)",
			"UPDATE blob SET size=-1, content=NULL WHERE uuid=?",
			"INSERT OR IGNORE INTO phantom(rid) SELECT rid FROM blob WHERE uuid=?",
		} {
			if _, err := tx.Exec(sql, uuid); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if _, held := content.AvailableByUUID(r.DB(), uuid); held {
		return fmt.Errorf("A.6 requires absent S payload, but %s remains held", uuid)
	}
	return nil
}

func smCanonicalState(r *repo.Repo, co *checkout.Checkout) (string, error) {
	var blobs, events, links int
	if err := r.DB().QueryRow(`SELECT (SELECT count(*) FROM blob),
		(SELECT count(*) FROM event), (SELECT count(*) FROM plink)`).Scan(&blobs, &events, &links); err != nil {
		return "", err
	}
	rid, uuid, err := co.Version()
	if err != nil {
		return "", err
	}
	return fmt.Sprint(blobs, "/", events, "/", links, "/", rid, "/", uuid), nil
}

func smCanonicalLeafSizes(r *repo.Repo, cards []deck.SubmanifestCard, wantSizes []int) error {
	var sizes []int
	for _, card := range cards {
		rid, held := content.AvailableByUUID(r.DB(), card.UUID)
		if !held {
			return fmt.Errorf("parent node %s absent before elision", card.UUID)
		}
		data, err := content.Expand(r.DB(), rid)
		if err != nil {
			return err
		}
		d, err := deck.Parse(data)
		if err != nil {
			return err
		}
		sizes = append(sizes, len(d.F))
	}
	if !slices.Equal(sizes, wantSizes) {
		return fmt.Errorf("A.6 parent leaves %v, want %v", sizes, wantSizes)
	}
	return nil
}

func smCanonicalClosure(r *repo.Repo, rid libfossil.FslID, files map[string]string, k int) error {
	tree, err := manifest.BuildTree(smCards(files), k, smSha3Name)
	if err != nil {
		return err
	}
	d, err := manifest.GetManifest(r, rid)
	if err != nil {
		return err
	}
	if !slices.Equal(d.S, tree.S) {
		return fmt.Errorf("A.6 S run %v, want full construction %v", d.S, tree.S)
	}
	if len(tree.S) == 0 {
		if !slices.Equal(d.F, smCards(files)) {
			return fmt.Errorf("A.6 flat result differs from full construction")
		}
	} else if len(d.F) != 0 {
		return fmt.Errorf("A.6 tree result also carries F cards")
	}
	// BuildTree returns every transitively referenced node, not just leaves.
	for _, expected := range tree.Subs {
		uuid := hash.SHA3(expected)
		node, held := content.AvailableByUUID(r.DB(), uuid)
		if !held {
			return fmt.Errorf("successful A.6 result lacks canonical node %s", uuid)
		}
		actual, err := content.Expand(r.DB(), node)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, expected) || hash.SHA3(actual) != uuid {
			return fmt.Errorf("A.6 node %s differs from full construction", uuid)
		}
	}
	return nil
}
