package manifest

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// These tests drive the writer of draft-fossil-submanifests-00 through
// Checkin, the one place check-in manifests are made: the emission gate
// (§8.1), the tree stored beside its manifest (§6.2), pinning and selecting
// k (§6.3, §8.1) and re-tuning it (§6.4).

// sha3Repo is a test repository that names artifacts as the vectors do.
func sha3Repo(t *testing.T) *repo.Repo {
	t.Helper()
	r := setupTestRepo(t)
	if err := r.SetHashPolicy("sha3"); err != nil {
		t.Fatal(err)
	}
	return r
}

func a1CheckinFiles() []File {
	return []File{
		{Name: "README.md", Content: []byte("# Example\n")},
		{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
		{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
		{Name: "src/util.c", Content: []byte("/* util */\n")},
	}
}

func setSettings(t *testing.T, r *repo.Repo, kv ...string) {
	t.Helper()
	for i := 0; i+1 < len(kv); i += 2 {
		if err := r.SetConfig(kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
}

var writeTime = time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)

func commit(t *testing.T, r *repo.Repo, opts CheckinOpts) (libfossil.FslID, *deck.Deck) {
	t.Helper()
	if opts.User == "" {
		opts.User = "drh"
	}
	if opts.Time.IsZero() {
		opts.Time = writeTime
	}
	rid, _, err := Checkin(r, opts)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	d, err := GetManifest(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	return rid, d
}

func seen(t *testing.T, r *repo.Repo) bool {
	t.Helper()
	ok, err := SeenSubmanifest(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// §8.1: emission is off by default, and the manifest stays flat.
func TestCheckinFlatByDefault(t *testing.T) {
	r := sha3Repo(t)
	_, d := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "flat"})
	if len(d.S) != 0 || len(d.F) != 4 || seen(t, r) {
		t.Fatalf("default commit: %d S cards, %d F cards, seen %v", len(d.S), len(d.F), seen(t, r))
	}
}

// §6.2: with emission on at k = 3 the A.1 files go to the two leaves of
// Appendix A.2, the manifest carries their S cards and no F card, the tree
// walks back to the four files, and the indicator of §8.1 is set.
func TestCheckinEmitsTree(t *testing.T) {
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	rid, d := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "tree"})
	if len(d.F) != 0 || len(d.S) != 2 ||
		d.S[0].UUID != "aa9d404c14fe4dadd2ab398961fb0dfe650a877c061f650d8233a5888e920acf" ||
		d.S[1].UUID != "a50a722b465ed67b42ee0fbe61e4a19d6b1d22940ba5740e99e450921e3f52ea" {
		t.Fatalf("manifest S %+v F %d", d.S, len(d.F))
	}
	files, err := ListFiles(r, rid)
	if err != nil || len(files) != 4 || files[3].Name != "src/util.c" {
		t.Fatalf("ListFiles = %+v, %v", files, err)
	}
	if !seen(t, r) {
		t.Fatal("seen-submanifest not recorded")
	}
	for _, s := range d.S {
		leaf, _ := blob.Exists(r.DB(), s.UUID)
		var n int
		r.DB().QueryRow("SELECT count(*) FROM unsent WHERE rid=?", leaf).Scan(&n)
		if n != 1 {
			t.Fatalf("leaf %s is not marked unsent", s.UUID[:10])
		}
	}
}

// §4.4: a tree check-in carries no B card even when a delta is asked for;
// a flat construction under emission still may be a delta.
func TestCheckinTreeIsNeverDelta(t *testing.T) {
	r := sha3Repo(t)
	parent, _ := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "base"})
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	files := a1CheckinFiles()
	files[1].Content = []byte("int main(void){return 1;}\n")
	_, d := commit(t, r, CheckinOpts{Files: files, Comment: "edit", Parent: parent, Delta: true,
		Time: writeTime.Add(time.Hour)})
	if d.B != "" || len(d.S) != 2 {
		t.Fatalf("B %q with %d S cards", d.B, len(d.S))
	}
}

// §6.3: a stored k outside 1..12 falls back to flat; §7.2: a commit made in
// a session whose server sent avoid-submanifests is flat.
func TestCheckinFlatFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, k string
		avoid   bool
	}{{"k 13", "13", false}, {"k 0", "0", false}, {"k unset", "", false}, {"avoid", "3", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := sha3Repo(t)
			setSettings(t, r, SettingEnableSubmanifests, "on")
			if tc.k != "" {
				setSettings(t, r, SettingSubmanifestK, tc.k)
			}
			_, d := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "c", AvoidSubmanifests: tc.avoid})
			if len(d.S) != 0 || len(d.F) != 4 || seen(t, r) {
				t.Fatalf("%d S cards, %d F cards", len(d.S), len(d.F))
			}
		})
	}
}

// subCount counts the sub-manifests r holds.
func subCount(t *testing.T, r *repo.Repo) int {
	t.Helper()
	subs := 0
	rows, err := r.DB().Query("SELECT uuid FROM blob WHERE size>=0")
	if err != nil {
		t.Fatal(err)
	}
	var uuids []string
	for rows.Next() {
		var u string
		rows.Scan(&u)
		uuids = append(uuids, u)
	}
	rows.Close()
	for _, u := range uuids {
		rid, _ := blob.Exists(r.DB(), u)
		d, err := GetManifest(r, rid)
		if err != nil {
			continue
		}
		if d.Type == deck.Submanifest {
			subs++
		}
	}
	return subs
}

// §6.2: content addressing shares every unchanged sub-manifest, so editing
// one file adds exactly one leaf.
func TestCheckinReusesUnchangedLeaves(t *testing.T) {
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	parent, first := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "one"})
	files := a1CheckinFiles()
	files[3].Content = []byte("/* util 2 */\n")
	_, second := commit(t, r, CheckinOpts{Files: files, Comment: "two", Parent: parent,
		Time: writeTime.Add(time.Hour)})
	if second.S[0] != first.S[0] || second.S[1] == first.S[1] {
		t.Fatalf("S runs %+v then %+v", first.S, second.S)
	}
	if n := subCount(t, r); n != 3 {
		t.Fatalf("%d sub-manifests stored, want 3", n)
	}
}

func settingValue(t *testing.T, r *repo.Repo, name string) string {
	t.Helper()
	v, err := r.Config(name)
	if err != nil {
		return ""
	}
	return v
}

// §8.1: a local enable request pins k by the rules of §6.3, and one that
// cannot be met changes nothing.
func TestEnableSubmanifests(t *testing.T) {
	r := sha3Repo(t)
	one, _ := commit(t, r, CheckinOpts{Files: a1CheckinFiles()[:1], Comment: "one file"})
	four, _ := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "four", Parent: one,
		Time: writeTime.Add(time.Hour)})

	unchanged := func(what string) {
		t.Helper()
		if settingValue(t, r, SettingEnableSubmanifests) != "" || settingValue(t, r, SettingSubmanifestK) != "" {
			t.Fatalf("%s left settings behind", what)
		}
	}
	// Appendix A.5: over one name no k is a candidate, so the request fails.
	if _, err := EnableSubmanifests(r, one, 0); !errors.Is(err, errNoSelection) {
		t.Fatalf("enable over one file: %v", err)
	}
	unchanged("selection without a result")
	if _, err := EnableSubmanifests(r, 0, 0); !errors.Is(err, errNoSelection) {
		t.Fatalf("enable with no current check-in: %v", err)
	}
	unchanged("no current check-in")
	for _, k := range []int{-1, 13} {
		if _, err := EnableSubmanifests(r, four, k); err == nil {
			t.Fatalf("enable --k %d accepted", k)
		}
		unchanged(fmt.Sprintf("k %d", k))
	}
	// Selection over A.1: k = 1, 2 and 3 tie, and the lowest wins.
	if k, err := EnableSubmanifests(r, four, 0); err != nil || k != 1 {
		t.Fatalf("enable = %d, %v", k, err)
	}
	if settingValue(t, r, SettingEnableSubmanifests) != "on" || settingValue(t, r, SettingSubmanifestK) != "1" {
		t.Fatal("settings not written")
	}
	// The pin takes precedence; a different explicit k is a re-tune.
	if k, err := EnableSubmanifests(r, four, 1); err != nil || k != 1 {
		t.Fatalf("re-enable at the pin = %d, %v", k, err)
	}
	if _, err := EnableSubmanifests(r, four, 3); err == nil || !strings.Contains(err.Error(), "re-tune") {
		t.Fatalf("enable --k 3 over pin 1: %v", err)
	}
	// An out-of-range stored k is no valid pin, nor absent: a request with
	// no explicit k is refused naming it, and selection does not run over
	// it; an explicit value replaces it.
	for _, stored := range []string{"13", "0", "x"} {
		setSettings(t, r, SettingEnableSubmanifests, "off", SettingSubmanifestK, stored)
		_, err := EnableSubmanifests(r, four, 0)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", stored)) {
			t.Fatalf("enable over stored k %q: %v", stored, err)
		}
		if settingValue(t, r, SettingEnableSubmanifests) != "off" || settingValue(t, r, SettingSubmanifestK) != stored {
			t.Fatalf("a refused enable over stored k %q changed the settings", stored)
		}
		if k, err := EnableSubmanifests(r, four, 2); err != nil || k != 2 {
			t.Fatalf("enable --k 2 over stored k %q = %d, %v", stored, k, err)
		}
		if settingValue(t, r, SettingSubmanifestK) != "2" {
			t.Fatalf("the explicit k did not replace stored k %q", stored)
		}
	}
}

// §8.1: a persistence failure partway through enabling leaves neither
// setting applied.
func TestEnableSubmanifestsAtomic(t *testing.T) {
	r := sha3Repo(t)
	four, _ := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "four"})
	if _, err := r.DB().Exec(`CREATE TRIGGER fail_enable BEFORE INSERT ON config
		WHEN NEW.name='enable-submanifests' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableSubmanifests(r, four, 3); err == nil {
		t.Fatal("enable succeeded through an injected failure")
	}
	if settingValue(t, r, SettingSubmanifestK) != "" || settingValue(t, r, SettingEnableSubmanifests) != "" {
		t.Fatal("a setting was applied by a failed enable")
	}
}

// §6.4: a manual re-tune governs the next commit; leaves whose bytes are the
// same under both constructions are shared, and history is untouched.
func TestRetuneGovernsNextCommit(t *testing.T) {
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	parent, first := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "one"})
	if err := RetuneSubmanifests(r, 13); err == nil {
		t.Fatal("re-tune to 13 accepted")
	}
	if err := RetuneSubmanifests(r, 4); err != nil {
		t.Fatal(err)
	}
	_, second := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "two", Parent: parent,
		Time: writeTime.Add(time.Hour)})
	if len(second.S) != 0 || len(second.F) != 4 {
		t.Fatalf("k=4 commit has %d S cards; A.1 is flat at k = 4", len(second.S))
	}
	if again, _ := GetManifest(r, parent); len(again.S) != len(first.S) {
		t.Fatal("the parent changed")
	}
	if err := RetuneSubmanifests(r, 2); err != nil {
		t.Fatal(err)
	}
	_, third := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "three", Parent: parent,
		Time: writeTime.Add(2 * time.Hour)})
	if len(third.S) != 2 || third.S[0] != first.S[0] || third.S[1] != first.S[1] {
		t.Fatalf("k=2 shares A.1's partition, so its leaves: %+v vs %+v", third.S, first.S)
	}
}

// retuneFiles is a file set on which k = 1 is far from the optimum.
func retuneFiles() []File {
	var out []File
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("dir%02d/file%03d.txt", i%20, i)
		out = append(out, File{Name: name, Content: []byte(name + "\n")})
	}
	return out
}

// §6.4: only while submanifest-auto-retune is on does a commit re-pin k, and
// then only past the margin; off, the pin stays.
func TestAutoRetune(t *testing.T) {
	var cards []deck.FileCard
	for _, f := range retuneFiles() {
		cards = append(cards, deck.FileCard{Name: f.Name, UUID: strings.Repeat("0", 64)})
	}
	sortCards(cards)
	target, ok := retuneTarget(partitionCosts(cards), 1)
	if !ok {
		t.Fatal("fixture: k = 1 shows no drift")
	}
	for _, auto := range []string{"off", "on"} {
		r := sha3Repo(t)
		setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "1", SettingAutoRetune, auto)
		commit(t, r, CheckinOpts{Files: retuneFiles(), Comment: "c"})
		want := "1"
		if auto == "on" {
			want = fmt.Sprint(target)
		}
		if got := settingValue(t, r, SettingSubmanifestK); got != want {
			t.Fatalf("auto-retune %s: k %s, want %s", auto, got, want)
		}
	}
}

func sortCards(cards []deck.FileCard) {
	for i := 1; i < len(cards); i++ {
		for j := i; j > 0 && cards[j-1].Name > cards[j].Name; j-- {
			cards[j-1], cards[j] = cards[j], cards[j-1]
		}
	}
}

// A duplicate name is refused rather than built into a tree.
func TestCheckinTreeRefusesDuplicateName(t *testing.T) {
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	files := append(a1CheckinFiles(), File{Name: "README.md", Content: []byte("again\n")})
	if _, _, err := Checkin(r, CheckinOpts{Files: files, Comment: "dup", User: "u"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	var n int
	if err := r.WithTx(func(tx *db.Tx) error {
		return tx.QueryRow("SELECT count(*) FROM event").Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("%d events after a refused commit (%v)", n, err)
	}
}

// zit's replay refinement of §6.3: it needs two replayable check-ins, and it
// keeps to the snapshot's k and its neighbours, the lowest on a tie.
func TestReplayK(t *testing.T) {
	r := sha3Repo(t)
	if _, ok, err := replayK(r.DB(), 3); ok || err != nil {
		t.Fatalf("an empty history replayed: %v", err)
	}
	one, _ := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "one"})
	if _, ok, _ := replayK(r.DB(), 3); ok {
		t.Fatal("one check-in replayed")
	}
	files := a1CheckinFiles()
	files[3].Content = []byte("/* util 2 */\n")
	commit(t, r, CheckinOpts{Files: files, Comment: "two", Parent: one, Time: writeTime.Add(time.Hour)})
	// A.1 partitions alike at k = 1, 2 and 3: every arm ties, the lowest wins.
	if k, ok, err := replayK(r.DB(), 2); err != nil || !ok || k != 1 {
		t.Fatalf("replayK(2) = %d, %v, %v", k, ok, err)
	}
	// From k = 4 every arm is flat: no result.
	if k, ok, err := replayK(r.DB(), 6); err != nil || ok {
		t.Fatalf("replayK(6) = %d, %v, %v", k, ok, err)
	}
}

// §8.1: disabling stops trees and keeps the pin; once trees exist it says
// they cannot be retired.
func TestDisableSubmanifests(t *testing.T) {
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	if note, err := DisableSubmanifests(r); err != nil || note != "" {
		t.Fatalf("disable before any tree = %q, %v", note, err)
	}
	setSettings(t, r, SettingEnableSubmanifests, "on")
	parent, _ := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "tree"})
	if note, err := DisableSubmanifests(r); err != nil || note != SubmanifestDisableNote {
		t.Fatalf("disable after a tree = %q, %v", note, err)
	}
	_, d := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "flat", Parent: parent,
		Time: writeTime.Add(time.Hour)})
	if len(d.S) != 0 || settingValue(t, r, SettingSubmanifestK) != "3" {
		t.Fatalf("after disable: %d S cards, k %q", len(d.S), settingValue(t, r, SettingSubmanifestK))
	}
}

// a1Tree is the A.1 tree at k = 3 named by alg: the leaves' bytes and the
// manifest's S run.
func a1Tree(t *testing.T, alg func([]byte) string) Tree {
	t.Helper()
	var cards []deck.FileCard
	for _, f := range a1CheckinFiles() {
		cards = append(cards, deck.FileCard{Name: f.Name, UUID: alg(f.Content)})
	}
	tree, err := BuildTree(cards, 3, func(b []byte) (string, error) { return alg(b), nil })
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func isPrivate(t *testing.T, r *repo.Repo, uuid string) bool {
	t.Helper()
	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatalf("%s not stored", uuid)
	}
	var n int
	r.DB().QueryRow("SELECT count(*) FROM private WHERE rid=?", rid).Scan(&n)
	return n > 0
}

// A public check-in makes every sub-manifest it names public, even one
// already stored privately (a private branch sharing a leaf), so sync sends
// the whole tree.
func TestCheckinTreeMakesSharedLeavesPublic(t *testing.T) {
	r := sha3Repo(t)
	tree := a1Tree(t, hash.SHA3)
	if err := r.WithTx(func(tx *db.Tx) error {
		for _, sub := range tree.Subs {
			rid, _, err := blob.Store(tx, sub)
			if err != nil {
				return err
			}
			if err := content.MakePrivate(tx, int64(rid)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	_, d := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "public"})
	for _, s := range d.S {
		if isPrivate(t, r, s.UUID) {
			t.Fatalf("leaf %s stays private under a public check-in", s.UUID[:10])
		}
		rid, _ := blob.Exists(r.DB(), s.UUID)
		var n int
		r.DB().QueryRow("SELECT count(*) FROM unsent WHERE rid=?", rid).Scan(&n)
		if n != 1 {
			t.Fatalf("leaf %s was made public but not queued for the next push", s.UUID[:10])
		}
	}
}

// §6.2: an S card names its child under the repository's hash policy: a
// sha1-named copy of a leaf in a sha3 repository is not reused, and a sha1
// repository names its tree in sha1.
func TestCheckinTreeNamesByPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		alg                 func([]byte) string
	}{
		{"sha3 over a sha1 copy", "0", "2", hash.SHA3},
		{"sha1", "0", "0", hash.SHA1},
		{"auto with sha1 history", "0", "1", hash.SHA1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupTestRepo(t)
			setSettings(t, r, "hash-policy", tc.before)
			if err := r.WithTx(func(tx *db.Tx) error {
				_, _, err := blob.Store(tx, a1Tree(t, tc.alg).Subs[0])
				return err
			}); err != nil {
				t.Fatal(err)
			}
			setSettings(t, r, "hash-policy", tc.after, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
			_, d := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "c"})
			want := a1Tree(t, tc.alg).S
			if len(d.S) != 2 || d.S[0] != want[0] || d.S[1] != want[1] {
				t.Fatalf("S run %+v, want %+v", d.S, want)
			}
		})
	}
}

// Only sub-manifests the commit wrote are queued for sync; one it shares
// with an earlier, already sent tree is not queued again.
func TestCheckinTreeQueuesOnlyNewSubmanifests(t *testing.T) {
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	parent, first := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "one"})
	if _, err := r.DB().Exec("DELETE FROM unsent"); err != nil {
		t.Fatal(err)
	}
	files := a1CheckinFiles()
	files[3].Content = []byte("/* util 2 */\n")
	_, second := commit(t, r, CheckinOpts{Files: files, Comment: "two", Parent: parent,
		Time: writeTime.Add(time.Hour)})
	unsent := func(uuid string) bool {
		rid, _ := blob.Exists(r.DB(), uuid)
		var n int
		r.DB().QueryRow("SELECT count(*) FROM unsent WHERE rid=?", rid).Scan(&n)
		return n > 0
	}
	if second.S[0] != first.S[0] || unsent(second.S[0].UUID) {
		t.Fatal("the shared leaf was queued again")
	}
	if !unsent(second.S[1].UUID) {
		t.Fatal("the new leaf was not queued")
	}
}

// §7.1 and §7.3: a received tree check-in waiting on a sub-manifest is linked
// as soon as a local commit stores that sub-manifest, not at the next sweep.
func TestCheckinFillingPhantomLinksWaitingTree(t *testing.T) {
	r := sha3Repo(t)
	tree := a1Tree(t, hash.SHA3)
	var waiting string
	if err := r.WithTx(func(tx *db.Tx) error {
		for _, f := range a1CheckinFiles() {
			if _, _, err := blob.Store(tx, f.Content); err != nil {
				return err
			}
		}
		if _, _, err := blob.Store(tx, tree.Subs[0]); err != nil {
			return err
		}
		data, err := (&deck.Deck{C: "received", D: writeTime, S: tree.S, U: deck.User("u"),
			T: []deck.TagCard{{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}}}).Marshal()
		if err != nil {
			return err
		}
		_, waiting, err = blob.Store(tx, data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatal(err)
	}
	waitingRid, _ := blob.Exists(r.DB(), waiting)
	events := func() int {
		var n int
		r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", waitingRid).Scan(&n)
		return n
	}
	if events() != 0 {
		t.Fatal("fixture: the tree was linked without its second leaf")
	}
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "local", Time: writeTime.Add(time.Hour)})
	if events() != 1 {
		t.Fatal("the waiting tree was not linked when its sub-manifest was stored")
	}
}

// §6.4: the automatic re-tune runs after the commit is durable; its failure
// is reported, never the commit's.
func TestAutoRetuneFailureKeepsCommit(t *testing.T) {
	r := sha3Repo(t)
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "1", SettingAutoRetune, "on")
	if _, err := r.DB().Exec(`CREATE TRIGGER fail_retune BEFORE INSERT ON config
		WHEN NEW.name='submanifest-k' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	rid, _, err := Checkin(r, CheckinOpts{Files: retuneFiles(), Comment: "c", User: "u", Time: writeTime})
	if err != nil || rid <= 0 {
		t.Fatalf("a failed re-tune failed the commit: %v", err)
	}
	if got := settingValue(t, r, SettingSubmanifestK); got != "1" {
		t.Fatalf("k %s after a failed re-tune", got)
	}
}

// Selection runs outside the write transaction -- here it writes a pin
// itself, as a concurrent writer might -- and a pin that appears meanwhile
// takes §8.1's precedence over the value selected.
func TestEnableSelectsOutsideTheWrite(t *testing.T) {
	r := sha3Repo(t)
	k, err := enableWith(r, 0, func() (int, error) {
		if err := r.SetConfig(SettingSubmanifestK, "5"); err != nil {
			return 0, err
		}
		return 2, nil
	})
	if err != nil || k != 5 || settingValue(t, r, SettingSubmanifestK) != "5" ||
		settingValue(t, r, SettingEnableSubmanifests) != "on" {
		t.Fatalf("enable = %d, %v; want the pin that appeared, 5", k, err)
	}
}

// The post-commit re-tune re-reads the settings: a manual re-tune, or the
// automatic one switched off, since the commit is never overwritten.
func TestAutoRetuneRereadsSettings(t *testing.T) {
	var cards []deck.FileCard
	for _, f := range retuneFiles() {
		cards = append(cards, deck.FileCard{Name: f.Name, UUID: strings.Repeat("0", 64)})
	}
	sortCards(cards)
	ct := checkinTree{settings: submanifestSettings{enabled: true, k: 1, autoRetune: true}, k: 1, files: cards}
	target, ok := retuneTarget(partitionCosts(cards), 1)
	if !ok {
		t.Fatal("fixture: k = 1 shows no drift")
	}
	manual := strconv.Itoa(target%MaxPartitionK + 1) // neither 1 nor the target
	for _, tc := range []struct{ k, auto, want string }{
		{manual, "on", manual}, // re-tuned by hand meanwhile
		{"1", "off", "1"},      // automatic re-tune switched off meanwhile
	} {
		r := sha3Repo(t)
		setSettings(t, r, SettingSubmanifestK, tc.k, SettingAutoRetune, tc.auto)
		if err := r.WithTx(ct.retune); err != nil {
			t.Fatal(err)
		}
		if got := settingValue(t, r, SettingSubmanifestK); got != tc.want {
			t.Fatalf("k %s, auto %s: re-tune left k %s, want %s", tc.k, tc.auto, got, tc.want)
		}
	}
}
