package conformance

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// The writer half of draft-fossil-submanifests-00 (§6, §8.1, and the writer
// vectors of A.5): construction checked against the binding Appendix A
// artifacts and against zit's recorded output, and the settings that govern
// it checked through manifest.Checkin, the one place check-ins are made.

// smSha3Name names an artifact under the sha3 policy, storing nothing.
func smSha3Name(data []byte) (string, error) { return hash.SHA3(data), nil }

// smCards is a vector set's files as sorted, hashed entries.
func smCards(files map[string]string) []deck.FileCard {
	var cards []deck.FileCard
	for name, text := range files {
		cards = append(cards, deck.FileCard{Name: name, UUID: hash.SHA3([]byte(text))})
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Name < cards[j].Name })
	return cards
}

// smZbits is §6.1's zbits(name), computed here from the hex digest so the
// suite does not lean on the writer's own predicate.
func smZbits(name string) int {
	n := 0
	for _, c := range hash.SHA3([]byte(name)) {
		v, _ := strconv.ParseUint(string(c), 16, 8)
		if v != 0 {
			return n + bits.LeadingZeros8(uint8(v)) - 4
		}
		n += 4
	}
	return n
}

// smReproduces builds set at k and requires the vector's S run and every
// sub-manifest it lists, byte for byte.
func smReproduces(set *smVectorSet, k int) error {
	tree, err := manifest.BuildTree(smCards(set.files), k, smSha3Name)
	if err != nil {
		return err
	}
	data, err := smVector(set.manifest)
	if err != nil {
		return err
	}
	vector, err := deck.Parse(data)
	if err != nil {
		return err
	}
	if err := want("S run", tree.S, vector.S); err != nil {
		return err
	}
	built := map[string]bool{}
	for _, sub := range tree.Subs {
		built[string(sub)] = true
	}
	for _, name := range set.subs {
		sub, err := smVector(name)
		if err != nil {
			return err
		}
		if !built[string(sub)] {
			return fmt.Errorf("%s is not among the %d sub-manifests built at k=%d", name, len(tree.Subs), k)
		}
	}
	if len(tree.Subs) != len(set.subs) {
		return fmt.Errorf("built %d sub-manifests, the vector has %d", len(tree.Subs), len(set.subs))
	}
	return nil
}

// smFlatAt requires cards to build flat at k.
func smFlatAt(cards []deck.FileCard, k int) error {
	tree, err := manifest.BuildTree(cards, k, smSha3Name)
	if err != nil {
		return err
	}
	if len(tree.S) != 0 || len(tree.Subs) != 0 {
		return fmt.Errorf("k=%d built %d S cards over %d names, want flat", k, len(tree.S), len(cards))
	}
	return nil
}

// smNamesBelow returns n names whose zbits are all below k.
func smNamesBelow(n, k int) []deck.FileCard {
	var cards []deck.FileCard
	for i := 0; len(cards) < n; i++ {
		name := fmt.Sprintf("n%05d.c", i)
		if smZbits(name) < k {
			cards = append(cards, deck.FileCard{Name: name, UUID: hash.SHA3([]byte(name))})
		}
	}
	return cards
}

// smWriterRepo is a sha3 repository with the given settings.
func smWriterRepo(t *testing.T, kv ...string) (*repo.Repo, error) {
	r, err := repo.Create(filepath.Join(t.TempDir(), "w.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetHashPolicy("sha3"); err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if err := r.SetConfig(kv[i], kv[i+1]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// smCommit checks in files and returns the stored manifest.
func smCommit(r *repo.Repo, files map[string]string, parent libfossil.FslID, at time.Time, avoid bool) (libfossil.FslID, *deck.Deck, error) {
	var list []manifest.File
	for name, text := range files {
		list = append(list, manifest.File{Name: name, Content: []byte(text)})
	}
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{Files: list, Comment: "c", User: "u",
		Time: at, Parent: parent, Delta: parent > 0, AvoidSubmanifests: avoid})
	if err != nil {
		return 0, nil, err
	}
	d, err := manifest.GetManifest(r, rid)
	return rid, d, err
}

func smWriterCase(id, req string, run func(t *testing.T) error) smCase {
	return smCase{Case{ID: id, Req: req}, run}
}

func smWriterCases() []smCase {
	sets := smVectorSets()
	vector := func(id, req, key string, k int) smCase {
		return smWriterCase(id, req, func(*testing.T) error { return smReproduces(sets[key], k) })
	}
	cases := []smCase{
		vector("submanifest.6.2.a3", "SM-6.2-1", "a3", 3),
		vector("submanifest.6.2.a4", "SM-6.2-1", "a4", 3),
		vector("submanifest.6.2.a5-three-level", "SM-6.2-1", "three-level", 2),
		vector("submanifest.6.2.a5-encoding", "SM-6.2-6", "encoding", 3),
		vector("submanifest.6.2.height-0-merge", "SM-6.2-4", "merge", 3),
		vector("submanifest.6.2.interior-merge", "SM-6.2-4", "a4", 3),
		vector("submanifest.a.5.writer-a4", "SM-A-4", "a4", 3),
		vector("submanifest.a.5.writer-height-0-merge", "SM-A-4", "merge", 3),
		vector("submanifest.a.5.writer-three-level", "SM-A-4", "three-level", 2),
		smWriterCase("submanifest.6.2.zit-oracle", "SM-6.2-1", func(t *testing.T) error {
			return smZitBundle(t)
		}),
	}
	cases = append(cases, smPartitionCases()...)
	cases = append(cases, smSettingCases()...)
	return append(cases, smEnableCases()...)
}

// smZitBundle replays zit's recorded tree check-ins (zit_writer_test.go):
// every one must come out byte for byte.
func smZitBundle(t *testing.T) error {
	for _, sc := range zwScenarios() {
		var manifests [][]byte
		for i := range sc.commits {
			m, err := os.ReadFile(filepath.Join(zitWriterDir, sc.name, fmt.Sprintf("c%d.manifest", i+1)))
			if err != nil {
				return err
			}
			manifests = append(manifests, m)
		}
		zwReplay(t, filepath.Join(t.TempDir(), sc.name+".fossil"), sc, manifests)
	}
	return nil
}

func smPartitionCases() []smCase {
	a1 := smCards(smA1)
	return []smCase{
		smWriterCase("submanifest.6.1.predicate", "SM-6.1-1", func(*testing.T) error {
			// Tables 3 and 4, and the boundary each implies at k = 3.
			for name, z := range map[string]int{"src/node11.c": 8, "f1523.c": 3, "f2614.c": 9, "README.md": 1} {
				if got := smZbits(name); got != z {
					return fmt.Errorf("zbits(%s) = %d, want %d", name, got, z)
				}
			}
			return smReproduces(smVectorSets()["a3"], 3)
		}),
		smWriterCase("submanifest.6.1.names-only", "SM-6.1-1", func(*testing.T) error {
			other := smCards(smA4Files(smA4Names...))
			for i := range other {
				other[i].UUID = hash.SHA1([]byte(other[i].Name))
				other[i].Perm, other[i].OldName = "x", "old/"+other[i].Name
			}
			ta, err := manifest.BuildTree(smCards(smA4Files(smA4Names...)), 3, smSha3Name)
			if err != nil {
				return err
			}
			tb, err := manifest.BuildTree(other, 3, smSha3Name)
			if err != nil {
				return err
			}
			var ka, kb []string
			for i := range ta.S {
				ka = append(ka, ta.S[i].Name)
			}
			for i := range tb.S {
				kb = append(kb, tb.S[i].Name)
			}
			return want("partition keys", kb, ka)
		}),
		smWriterCase("submanifest.6.2.flat-n0-n1", "SM-6.2-2", func(*testing.T) error {
			for k := 1; k <= 12; k++ {
				if err := smFlatAt(nil, k); err != nil {
					return err
				}
				if err := smFlatAt(a1[:1], k); err != nil {
					return err
				}
			}
			return nil
		}),
		smWriterCase("submanifest.6.2.flat-min-suppressed", "SM-6.2-2", func(*testing.T) error {
			return smFlatAt(a1, 4)
		}),
		smWriterCase("submanifest.6.2.flat-no-boundary", "SM-6.2-2", func(*testing.T) error {
			return smFlatAt(smNamesBelow(32, 3), 3)
		}),
		smWriterCase("submanifest.6.2.forced-at-max", "SM-6.2-3", func(*testing.T) error {
			tree, err := manifest.BuildTree(smNamesBelow(70, 3), 3, smSha3Name)
			if err != nil {
				return err
			}
			if len(tree.S) != 3 || strings.Count(string(tree.Subs[0]), "\nF ")+1 != 32 {
				return fmt.Errorf("70 names with no boundary built %d leaves", len(tree.S))
			}
			return nil
		}),
		smWriterCase("submanifest.6.2.three-level", "SM-6.2-5", func(*testing.T) error {
			var cards []deck.FileCard
			for i := 0; i < 400; i++ {
				name := fmt.Sprintf("d%02d/f%03d.c", i%17, i)
				cards = append(cards, deck.FileCard{Name: name, UUID: hash.SHA3([]byte(name))})
			}
			sort.Slice(cards, func(i, j int) bool { return cards[i].Name < cards[j].Name })
			tree, err := manifest.BuildTree(cards, 1, smSha3Name)
			if err != nil {
				return err
			}
			for _, sub := range tree.Subs {
				if sub[0] == 'S' {
					return nil
				}
			}
			return fmt.Errorf("no interior sub-manifest over 400 names at k=1")
		}),
		smWriterCase("submanifest.a.5.writer-flat", "SM-A-4", func(*testing.T) error {
			if err := smFlatAt(a1, 4); err != nil {
				return err
			}
			return smFlatAt(a1[:1], 3)
		}),
	}
}

func smSettingCases() []smCase {
	return []smCase{
		smWriterCase("submanifest.8.1.off-by-default", "SM-8.1-1", func(t *testing.T) error {
			r, err := smWriterRepo(t)
			if err != nil {
				return err
			}
			_, d, err := smCommit(r, smA1, 0, smTime, false)
			if err != nil {
				return err
			}
			return want("S cards with emission off", len(d.S), 0)
		}),
		smWriterCase("submanifest.8.1.on-emits", "SM-8.1-1", func(t *testing.T) error {
			r, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, "3")
			if err != nil {
				return err
			}
			_, d, err := smCommit(r, smA1, 0, smTime, false)
			if err != nil {
				return err
			}
			if seen, _ := manifest.SeenSubmanifest(r.DB()); !seen {
				return fmt.Errorf("seen-submanifest not recorded")
			}
			return want("S run", d.S, smMustParse("a3_tree_manifest").S)
		}),
		smWriterCase("submanifest.8.1.disable-stops", "SM-8.1-4", func(t *testing.T) error {
			r, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, "3")
			if err != nil {
				return err
			}
			first, _, err := smCommit(r, smA1, 0, smTime, false)
			if err != nil {
				return err
			}
			if err := r.SetConfig(manifest.SettingEnableSubmanifests, "off"); err != nil {
				return err
			}
			_, d, err := smCommit(r, smA1, first, smTime.Add(time.Hour), false)
			if err != nil {
				return err
			}
			if len(d.S) != 0 {
				return fmt.Errorf("a tree after disabling")
			}
			older, err := manifest.GetManifest(r, first)
			if err != nil {
				return err
			}
			return want("the earlier tree's S cards", len(older.S), 2)
		}),
		smWriterCase("submanifest.6.3.out-of-range-flat", "SM-6.3-6", func(t *testing.T) error {
			for _, k := range []string{"0", "13", "x"} {
				r, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, k)
				if err != nil {
					return err
				}
				_, d, err := smCommit(r, smA1, 0, smTime, false)
				if err != nil {
					return err
				}
				if len(d.S) != 0 || len(d.F) != 4 {
					return fmt.Errorf("k %q: %d S cards", k, len(d.S))
				}
			}
			return nil
		}),
		smWriterCase("submanifest.6.3.pinned-not-derived", "SM-6.3-5", func(t *testing.T) error {
			// A.4's optimum is not k = 1, yet a pin of 1 governs the commit.
			r, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, "1")
			if err != nil {
				return err
			}
			files := smA4Files(smA4Names...)
			if k, _ := manifest.SelectPartitionK(smCards(files)); k == 1 {
				return fmt.Errorf("fixture: k = 1 is A.4's optimum")
			}
			_, d, err := smCommit(r, files, 0, smTime, false)
			if err != nil {
				return err
			}
			tree, err := manifest.BuildTree(smCards(files), 1, smSha3Name)
			if err != nil {
				return err
			}
			return want("S run", d.S, tree.S)
		}),
		smWriterCase("submanifest.6.3.every-k", "SM-6.3-5", func(t *testing.T) error {
			// Every value from 1 to 12 is supported: each builds the
			// construction for its own k.
			var cards []deck.FileCard
			for i := 0; i < 5000; i++ {
				name := fmt.Sprintf("p%d/f%05d", i%9, i)
				cards = append(cards, deck.FileCard{Name: name, UUID: hash.SHA3([]byte(name))})
			}
			sort.Slice(cards, func(i, j int) bool { return cards[i].Name < cards[j].Name })
			for k := 1; k <= 12; k++ {
				if _, err := manifest.BuildTree(cards, k, smSha3Name); err != nil {
					return fmt.Errorf("k=%d: %v", k, err)
				}
			}
			return nil
		}),
		smWriterCase("submanifest.4.4.tree-never-delta", "SM-4.4-1", func(t *testing.T) error {
			r, err := smWriterRepo(t)
			if err != nil {
				return err
			}
			base, _, err := smCommit(r, smA1, 0, smTime, false)
			if err != nil {
				return err
			}
			for _, kv := range [][2]string{{manifest.SettingEnableSubmanifests, "on"}, {manifest.SettingSubmanifestK, "3"}} {
				if err := r.SetConfig(kv[0], kv[1]); err != nil {
					return err
				}
			}
			edited := map[string]string{}
			for k, v := range smA1 {
				edited[k] = v
			}
			edited["src/util.c"] = "/* util 2 */\n"
			_, d, err := smCommit(r, edited, base, smTime.Add(time.Hour), false)
			if err != nil {
				return err
			}
			if d.B != "" || len(d.S) == 0 {
				return fmt.Errorf("B %q with %d S cards", d.B, len(d.S))
			}
			return nil
		}),
		smWriterCase("submanifest.6.2.reuse", "SM-6.2-7", func(t *testing.T) error {
			r, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, "3")
			if err != nil {
				return err
			}
			first, d1, err := smCommit(r, smA1, 0, smTime, false)
			if err != nil {
				return err
			}
			edited := map[string]string{}
			for k, v := range smA1 {
				edited[k] = v
			}
			edited["src/util.c"] = "/* util 2 */\n"
			_, d2, err := smCommit(r, edited, first, smTime.Add(time.Hour), false)
			if err != nil {
				return err
			}
			if d2.S[0] != d1.S[0] || d2.S[1] == d1.S[1] {
				return fmt.Errorf("S runs %v then %v: want the first leaf shared", d1.S, d2.S)
			}
			return nil
		}),
		smWriterCase("submanifest.7.2.avoid-in-session", "SM-7.2-3", smAvoidInSession),
	}
}

// smAvoidInSession: a sync session whose server has emission off reports
// avoid-submanifests, and the commit made in it is flat although the
// client has emission on.
func smAvoidInSession(t *testing.T) error {
	server, err := smWriterRepo(t)
	if err != nil {
		return err
	}
	client, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, "3")
	if err != nil {
		return err
	}
	pc, err := server.Config("project-code")
	if err != nil {
		return err
	}
	if err := client.SetConfig("project-code", pc); err != nil {
		return err
	}
	transport := &sync.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := sync.HandleSync(context.Background(), server, req)
		if err != nil {
			t.Errorf("HandleSync: %v", err)
		}
		return resp
	}}
	res, err := sync.Sync(context.Background(), client, transport, sync.SyncOpts{Push: true, Pull: true,
		CkinLock: &sync.CkinLockReq{ParentUUID: pc, ClientID: "client-1"}})
	if err != nil {
		return err
	}
	if !res.AvoidSubmanifests {
		return fmt.Errorf("the session did not report avoid-submanifests")
	}
	_, d, err := smCommit(client, smA1, 0, smTime, res.AvoidSubmanifests)
	if err != nil {
		return err
	}
	return want("S cards in the session's commit", len(d.S), 0)
}

func smMustParse(name string) *deck.Deck {
	data, err := smVector(name)
	if err != nil {
		panic(err)
	}
	d, err := deck.Parse(data)
	if err != nil {
		panic(err)
	}
	return d
}

func smEnableCases() []smCase {
	settings := func(r *repo.Repo) string {
		enable, _ := r.Config(manifest.SettingEnableSubmanifests)
		k, _ := r.Config(manifest.SettingSubmanifestK)
		return enable + "/" + k
	}
	withCheckins := func(t *testing.T) (*repo.Repo, libfossil.FslID, libfossil.FslID, error) {
		r, err := smWriterRepo(t)
		if err != nil {
			return nil, 0, 0, err
		}
		one, _, err := smCommit(r, map[string]string{"README.md": "# Example\n"}, 0, smTime, false)
		if err != nil {
			return nil, 0, 0, err
		}
		four, _, err := smCommit(r, smA1, one, smTime.Add(time.Hour), false)
		return r, one, four, err
	}
	return []smCase{
		smWriterCase("submanifest.6.3.selection", "SM-6.3-7", func(t *testing.T) error {
			if k, ok := manifest.SelectPartitionK(smCards(smA1)); !ok || k != 1 {
				return fmt.Errorf("selection over A.1 = %d, %v; k 1 to 3 tie and the lowest wins", k, ok)
			}
			if k, ok := manifest.SelectPartitionK(smCards(smA1)[:1]); ok {
				return fmt.Errorf("selection over one name = %d", k)
			}
			return smZitSelections()
		}),
		smWriterCase("submanifest.6.3.enable-matches-zit", "SM-6.3-7", zwCheckEnables),
		smWriterCase("submanifest.8.1.enable-rules", "SM-8.1-3", func(t *testing.T) error {
			r, _, four, err := withCheckins(t)
			if err != nil {
				return err
			}
			for _, k := range []int{0, 13} {
				if _, err := manifest.EnableSubmanifests(r, 0, k); err == nil {
					return fmt.Errorf("enable with k %d and no current check-in accepted", k)
				}
			}
			if s := settings(r); s != "/" {
				return fmt.Errorf("a refused request left %q", s)
			}
			if k, err := manifest.EnableSubmanifests(r, four, 0); err != nil || k != 1 {
				return fmt.Errorf("selected %d, %v", k, err)
			}
			if _, err := manifest.EnableSubmanifests(r, four, 2); err == nil {
				return fmt.Errorf("a different explicit k over a pin was accepted")
			}
			return want("settings", settings(r), "on/1")
		}),
		smWriterCase("submanifest.a.5.no-candidate", "SM-A-4", func(t *testing.T) error {
			r, one, _, err := withCheckins(t)
			if err != nil {
				return err
			}
			if _, err := manifest.EnableSubmanifests(r, one, 0); err == nil {
				return fmt.Errorf("enable over one name succeeded")
			}
			return want("settings", settings(r), "/")
		}),
		smWriterCase("submanifest.a.5.injected-failure", "SM-A-4", func(t *testing.T) error {
			r, _, four, err := withCheckins(t)
			if err != nil {
				return err
			}
			if _, err := r.DB().Exec(`CREATE TRIGGER fail_enable BEFORE INSERT ON config
				WHEN NEW.name='enable-submanifests' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
				return err
			}
			if _, err := manifest.EnableSubmanifests(r, four, 0); err == nil {
				return errors.New("enable succeeded through an injected failure")
			}
			return want("settings", settings(r), "/")
		}),
		smWriterCase("submanifest.6.4.manual-retune", "SM-6.4-1", func(t *testing.T) error {
			r, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, "3")
			if err != nil {
				return err
			}
			first, d1, err := smCommit(r, smA1, 0, smTime, false)
			if err != nil {
				return err
			}
			if err := manifest.RetuneSubmanifests(r, 2); err != nil {
				return err
			}
			_, d2, err := smCommit(r, smA1, first, smTime.Add(time.Hour), false)
			if err != nil {
				return err
			}
			// A.1 partitions alike at k = 2 and 3: every leaf is shared.
			if err := want("S run after the re-tune", d2.S, d1.S); err != nil {
				return err
			}
			if err := manifest.RetuneSubmanifests(r, 13); err == nil {
				return fmt.Errorf("re-tune to 13 accepted")
			}
			return want("pin", settings(r), "on/2")
		}),
		smWriterCase("submanifest.6.4.auto-retune", "SM-6.4-2", smAutoRetune),
	}
}

// smAutoRetune: with the pin at 1 on a set whose optimum is far better, a
// commit re-pins only while submanifest-auto-retune is on.
func smAutoRetune(t *testing.T) error {
	files := map[string]string{}
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("dir%02d/file%03d.txt", i%20, i)
		files[name] = name + "\n"
	}
	best, ok := manifest.SelectPartitionK(smCards(files))
	if !ok || best == 1 {
		return fmt.Errorf("fixture: optimum %d", best)
	}
	for _, auto := range []string{"off", "on"} {
		r, err := smWriterRepo(t, manifest.SettingEnableSubmanifests, "on", manifest.SettingSubmanifestK, "1",
			manifest.SettingAutoRetune, auto)
		if err != nil {
			return err
		}
		if _, _, err := smCommit(r, files, 0, smTime, false); err != nil {
			return err
		}
		got, _ := r.Config(manifest.SettingSubmanifestK)
		wantK := "1"
		if auto == "on" {
			wantK = strconv.Itoa(best)
		}
		if got != wantK {
			return fmt.Errorf("auto-retune %s: k %s, want %s", auto, got, wantK)
		}
	}
	return nil
}

// smZitSelections compares §6.3's selection with the optimum zit's analysis
// recorded for each scenario file set.
func smZitSelections() error {
	data, err := os.ReadFile(filepath.Join(zitWriterDir, "selection.txt"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		set, kText, _ := strings.Cut(line, " ")
		zk, _ := strconv.Atoi(kText)
		if gk := zwSelected(zwSet(set)); gk != zk {
			return fmt.Errorf("%s: selected k=%d, zit's analysis k=%d", set, gk, zk)
		}
	}
	return nil
}
