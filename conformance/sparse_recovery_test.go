package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/simio"
)

func scRecoveryCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.4.2.recovery-max", Req: "SC-4.2-3"},
			func(t *testing.T) error { return scCanonicalRecovery(t, "max", 1) }},
		{Case{ID: "sparsecone.4.2.recovery-final-merge", Req: "SC-4.2-3"},
			func(t *testing.T) error { return scCanonicalRecovery(t, "final", 3) }},
		{Case{ID: "sparsecone.4.2.recovery-changed-k", Req: "SC-4.2-3"},
			func(t *testing.T) error { return scCanonicalRecovery(t, "replace", 2) }},
		{Case{ID: "sparsecone.4.2.recovery-unknown-k", Req: "SC-4.2-3"},
			func(t *testing.T) error { return scCanonicalRecovery(t, "replace", 1) }},
	}
}

// Direct execution never treats a deviation marker as a passing assertion.
func TestSparseRecovery(t *testing.T) {
	for _, c := range scRecoveryCases() {
		t.Run(c.ID, func(t *testing.T) {
			if err := c.run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// These are Appendix A.6's literal names, not a search using the writer's
// boundary function: the 18 names with zbits zero force MAX propagation.
var scRecoveryMAXNames = []string{
	"f0000", "f0001", "f0003", "f0004", "f0005", "f0006", "f0008", "f0010", "f0014",
	"f0015", "f0016", "f0017", "f0018", "f0019", "f0021", "f0022", "f0023", "f0024",
}

var scRecoveryTime = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

type scRecoveryFixture struct {
	peer               *scSyncPeer
	dense, sparse      *repo.Repo
	parentUUID         string
	parentRID          libfossil.FslID
	oldNames, newNames []string
	cone               *cone.Cone
}

// Both clones use the same coverage policy, but only one actually loses S/F
// bytes. Their resulting check-in and every canonical child must be identical.
func scCanonicalRecovery(t *testing.T, mode string, k int) error {
	fixture, err := scRecoveryOrigin(t, mode)
	if err != nil {
		return err
	}
	if err := fixture.configure(t, mode, k); err != nil {
		return err
	}
	if err := fixture.elide(mode); err != nil {
		return err
	}
	if err := fixture.recover(); err != nil {
		return err
	}
	files, cards := fixture.files(mode)
	var output [][]byte
	for _, r := range []*repo.Repo{fixture.dense, fixture.sparse} {
		rid, ok := blob.Exists(r.DB(), fixture.parentUUID)
		if !ok {
			return expectf("parent identity not held")
		}
		newRID, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: files, Parent: rid, User: "u", Comment: "A.6 canonical rewrite",
			Time: scRecoveryTime.Add(time.Hour),
		})
		if err != nil {
			return err
		}
		data, err := content.Expand(r.DB(), newRID)
		if err != nil {
			return err
		}
		output = append(output, data)
	}
	if !bytes.Equal(output[0], output[1]) {
		return expectf("sparse reconstruction differs byte-for-byte from dense canonical commit")
	}
	if err := fixture.checkCanonical(mode, k, cards, output[1]); err != nil {
		return err
	}
	for _, name := range fixture.oldNames {
		if !fixture.cone.Covers(name) {
			if _, held := content.AvailableByUUID(fixture.sparse.DB(), hash.SHA3([]byte(name))); held {
				return expectf("structural recovery fetched outside F payload %s", name)
			}
		}
	}
	return nil
}

func scRecoveryOrigin(t *testing.T, mode string) (*scRecoveryFixture, error) {
	fixture := &scRecoveryFixture{
		oldNames: append([]string{scRecoveryMAXNames[0]}, scRecoveryMAXNames[2:]...),
		newNames: slices.Clone(scRecoveryMAXNames),
	}
	oldK := 1
	if mode == "replace" {
		fixture.newNames = slices.Clone(fixture.oldNames)
	}
	if mode == "final" {
		fixture.oldNames = []string{"g0000", "g0001", "g0005", "g0006"}
		fixture.newNames = []string{"g0000", "g0001", "g0005"}
		oldK = 3
	}
	r, err := repo.Create(filepath.Join(t.TempDir(), "origin.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetHashPolicy("sha3"); err != nil {
		return nil, err
	}
	for key, value := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: fmt.Sprint(oldK),
	} {
		if err := r.SetConfig(key, value); err != nil {
			return nil, err
		}
	}
	files := make([]manifest.File, 0, len(fixture.oldNames))
	for _, name := range fixture.oldNames {
		files = append(files, manifest.File{Name: name, Content: []byte(name)})
	}
	_, fixture.parentUUID, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: files, User: "u", Comment: "A.6 parent", Time: scRecoveryTime,
	})
	if err != nil {
		return nil, err
	}
	fixture.peer = &scSyncPeer{r: r}
	fixture.dense, _ = scSyncClone(t, fixture.peer, sync.CloneOpts{PullPhantoms: true})
	fixture.sparse, _ = scSyncClone(t, fixture.peer, sync.CloneOpts{PullPhantoms: true})
	return fixture, nil
}

func (f *scRecoveryFixture) configure(t *testing.T, mode string, k int) error {
	var ok bool
	f.parentRID, ok = blob.Exists(f.sparse.DB(), f.parentUUID)
	if !ok {
		return expectf("parent identity not cloned")
	}
	parent, err := manifest.GetManifest(f.sparse, f.parentRID)
	if err != nil {
		return err
	}
	if len(parent.S) < 2 {
		return expectf("A.6 parent has no sibling sub-manifest")
	}
	kept := 0
	if mode == "final" {
		kept = 1
	}
	keptRID, ok := content.AvailableByUUID(f.sparse.DB(), parent.S[kept].UUID)
	if !ok {
		return expectf("fixture leaf not held")
	}
	keptBytes, err := content.Expand(f.sparse.DB(), keptRID)
	if err != nil {
		return err
	}
	keptLeaf, err := deck.Parse(keptBytes)
	if err != nil {
		return err
	}
	var include []string
	for _, file := range keptLeaf.F {
		include = append(include, file.Name)
	}
	if mode == "max" {
		include = append(include, "f0001")
	}
	f.cone = scCone(t, include, nil)
	for _, r := range []*repo.Repo{f.dense, f.sparse} {
		if err := r.SetHashPolicy("sha3"); err != nil {
			return err
		}
		if err := content.SetStoreCone(r.DB(), f.cone); err != nil {
			return err
		}
		if err := r.SetConfig(manifest.SettingEnableSubmanifests, "on"); err != nil {
			return err
		}
		if err := r.SetConfig(manifest.SettingSubmanifestK, fmt.Sprint(k)); err != nil {
			return err
		}
	}
	return nil
}

func (f *scRecoveryFixture) elide(mode string) error {
	parent, err := manifest.GetManifest(f.sparse, f.parentRID)
	if err != nil {
		return err
	}
	kept := 0
	if mode == "final" {
		kept = 1
	}
	var elided []string
	for i, s := range parent.S {
		if i != kept {
			elided = append(elided, s.UUID)
		}
	}
	for _, name := range f.oldNames {
		if !f.cone.Covers(name) {
			elided = append(elided, hash.SHA3([]byte(name)))
		}
	}
	for _, uuid := range elided {
		_, err := f.sparse.DB().Exec("UPDATE blob SET size=-1,content=NULL WHERE uuid=?", uuid)
		if err != nil {
			return err
		}
		_, err = f.sparse.DB().Exec(
			"INSERT OR IGNORE INTO phantom(rid) SELECT rid FROM blob WHERE uuid=?", uuid)
		if err != nil {
			return err
		}
		if _, held := content.AvailableByUUID(f.sparse.DB(), uuid); held {
			return expectf("fixture did not actually remove %s", uuid)
		}
	}
	return nil
}

func (f *scRecoveryFixture) recover() error {
	var refused *manifest.RequiredContentError
	err := manifest.RequireParentContent(context.Background(), f.sparse.DB(),
		[]libfossil.FslID{f.parentRID})
	if !errors.As(err, &refused) {
		return expectf("absent S sibling was not explicitly refused: %v", err)
	}
	if refused.Elided == 0 || !strings.Contains(refused.Error(), "no configured peer") {
		return expectf("incorrect elision diagnostic: %v", refused)
	}
	if strings.Contains(refused.Error(), "missing") || strings.Contains(refused.Error(), "corrupt") {
		return expectf("elision described as damage: %v", refused)
	}
	_, err = sync.FetchRequiredArtifacts(context.Background(), f.sparse, f.peer,
		refused.Roots, sync.SyncOpts{})
	if err != nil {
		return err
	}
	got, err := content.StoreCone(f.sparse.DB())
	if err != nil || got == nil || !got.Equal(f.cone) {
		return expectf("recovery changed policy: %v, %v", got, err)
	}
	return nil
}

func (f *scRecoveryFixture) files(mode string) ([]manifest.File, []deck.FileCard) {
	files := make([]manifest.File, 0, len(f.newNames))
	cards := make([]deck.FileCard, 0, len(f.newNames))
	for _, name := range f.newNames {
		data := []byte(name)
		if mode == "replace" && name == "f0000" {
			data = []byte("replacement")
		}
		file := manifest.File{Name: name, UUID: hash.SHA3(data)}
		if f.cone.Covers(name) {
			file.UUID, file.Content = "", data
		}
		files = append(files, file)
		cards = append(cards, deck.FileCard{Name: name, UUID: hash.SHA3(data)})
	}
	return files, cards
}

func (f *scRecoveryFixture) checkCanonical(
	mode string, k int, cards []deck.FileCard, output []byte,
) error {
	canonical, err := manifest.BuildTree(cards, k, func(data []byte) (string, error) {
		return hash.SHA3(data), nil
	})
	if err != nil {
		return err
	}
	result, err := deck.Parse(output)
	if err != nil {
		return err
	}
	if !slices.Equal(result.S, canonical.S) {
		return expectf("rewrite did not use full canonical construction at requested k=%d", k)
	}
	wantSizes := []int{8, 8, 2}
	if mode == "replace" {
		wantSizes = []int{8, 8, 1}
		if k == 2 {
			wantSizes = []int{16, 1}
		}
	}
	if mode == "final" {
		wantSizes = nil
		if len(result.F) != 3 || len(result.S) != 0 {
			return expectf("final-small-group merge did not flatten to exactly three F cards")
		}
	}
	var leafSizes []int
	for _, want := range canonical.Subs {
		leaf, err := deck.Parse(want)
		if err != nil {
			return err
		}
		leafSizes = append(leafSizes, len(leaf.F))
		uuid := hash.SHA3(want)
		rid, held := content.AvailableByUUID(f.sparse.DB(), uuid)
		if !held {
			return expectf("canonical child %s absent", uuid)
		}
		got, err := content.Expand(f.sparse.DB(), rid)
		if err != nil || !bytes.Equal(got, want) {
			return expectf("canonical child bytes differ for %s: %v", uuid, err)
		}
	}
	if !slices.Equal(leafSizes, wantSizes) {
		return expectf("canonical leaf sizes %v, want literal A.6 sizes %v", leafSizes, wantSizes)
	}
	return nil
}
