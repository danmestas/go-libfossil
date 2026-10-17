package conformance

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

// The requirements of the artifact draft that are not about parsing one
// artifact: naming, the comparator, the writer, the R card, the hash policy
// and delta manifests. Each case calls the package that implements the
// requirement. fossil(1) is not consulted; where a case encodes fossil's
// behavior rather than plain draft text, its comment says so.

// apiCase is a check of one requirement through a go-libfossil API.
type apiCase struct {
	Case
	// run returns nil when go-libfossil conforms. t is for temporary
	// repositories only.
	run func(t *testing.T) error
}

func TestArtifactAPI(t *testing.T) {
	for _, c := range apiCases() {
		check(t, c.Case, func() error { return c.run(t) })
	}
}

func apiCases() []apiCase {
	var all []apiCase
	for _, table := range [][]apiCase{
		namingCases, writerCases, rCardCases, policyCases(), deltaManifestCases(),
	} {
		all = append(all, table...)
	}
	for i := range all {
		all[i].Gap = artifactGaps[all[i].ID]
	}
	return all
}

// The complete examples of Appendix A.1, byte for byte.
const (
	exampleSHA1 = "C initial\\sempty\\sbaseline\nD 2007-07-21T14:09:59\nP\n" +
		"R d41d8cd98f00b204e9800998ecf8427e\nU drh\nZ c62e3619f7c41ee77b33f00df8ec26f4\n"
	exampleSHA3 = "C Test\\stech-note\nD 2017-12-06T15:36:24.287\n" +
		"E 2013-08-26T07:35:00 bfdf42305400f6caa441fa816cb89231e90f8c82\n" +
		"P 94694585cd2f85f75b4316515bbb09aa7e9acaa5\nN text/x-fossil-wiki\n" +
		"T +bgcolor * #c0f0ff\nU drh\nW 415\nThis technote is only for test-purposes.\n\n" +
		"The original version of this technote had an empty C-card demonstrating that " +
		"the C-card in event artifacts are optional. Even when doing a \"fossil rebuild\" " +
		"using fossil version 1.22, this events displays fine. Of course, making the " +
		"Timeline Comment non-empty would be better. But nothing in Fossil prevents " +
		"leaving this line empty, so the C-card could as well be left out.\n\n" +
		"Z a7789c6da4c12b8feb6e290f3083f5f0\n"
)

var namingCases = []apiCase{
	{Case{ID: "artifact.3.1.name-covers-wrapper", Req: "AF-3.1-1"}, func(t *testing.T) error {
		r := newPolicyRepo(t, "2")
		signed := []byte(pgpHead + signedControl + pgpSig)
		_, uuid, err := blob.Store(r.DB(), signed)
		if err != nil {
			return err
		}
		return want("stored name", uuid, hash.SHA3(signed))
	}},
	{Case{ID: "artifact.3.2.sha1-example", Req: "AF-3.2-1"}, func(*testing.T) error {
		return want("SHA1(A.1.1)", hash.SHA1([]byte(exampleSHA1)),
			"a28c83647dfa805f05f3204a7e146eb1f0d90505")
	}},
	{Case{ID: "artifact.3.2.sha3-example", Req: "AF-3.2-1"}, func(*testing.T) error {
		return want("SHA3(A.1.2)", hash.SHA3([]byte(exampleSHA3)),
			"2fa494a18a766efe9a8ba706d669b7157d5329e1fbcc2a6d6c15a386d113218d")
	}},
	// FIPS 202 SHA3-256 of nothing; Keccak-256 would give c5d2460186f7...
	{Case{ID: "artifact.3.2.sha3-not-keccak", Req: "AF-3.2-1"}, func(*testing.T) error {
		return want("SHA3(empty)", hash.SHA3([]byte{}),
			"a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a")
	}},
	{Case{ID: "artifact.3.2.name-lengths", Req: "AF-3.2-2"}, func(*testing.T) error {
		for _, n := range []int{1, 39, 41, 63, 65} {
			if hash.IsValidHash(strings.Repeat("a", n)) {
				return expectf("a %d-digit name is valid", n)
			}
		}
		if !hash.IsValidHash(h1) || !hash.IsValidHash(h64) {
			return expectf("a 40- or 64-digit name is invalid")
		}
		return nil
	}},
	{Case{ID: "artifact.3.3.lowercase-names", Req: "AF-3.3-1"}, func(*testing.T) error {
		lower := regexp.MustCompile(`^[0-9a-f]+$`)
		for _, name := range []string{hash.SHA1([]byte("x")), hash.SHA3([]byte("x"))} {
			if !lower.MatchString(name) {
				return expectf("computed name %q is not lowercase hex", name)
			}
		}
		return nil
	}},
	{Case{ID: "artifact.3.4.verify-match", Req: "AF-3.4-1"}, verifyCase(hash.SHA3, true)},
	{Case{ID: "artifact.3.4.verify-uppercase", Req: "AF-3.4-1"}, verifyCase(
		func(b []byte) string { return strings.ToUpper(hash.SHA1(b)) }, false)},
	{Case{ID: "artifact.3.4.verify-length", Req: "AF-3.4-1"}, verifyCase(
		func(b []byte) string { return hash.SHA3(b)[:50] }, false)},
	{Case{ID: "artifact.3.4.verify-mismatch", Req: "AF-3.4-1"}, verifyCase(
		func([]byte) string { return hash.SHA1([]byte("other")) }, false)},
	{Case{ID: "artifact.4.5.3.comparator", Req: "AF-4.5.3-1"}, func(*testing.T) error {
		// Each pair is in ascending order under the canonical comparator.
		for _, pair := range [][2]string{
			{"", "a"}, {"a", "ab"}, {"B", "a"}, {"a.b", "a/b"}, {"a/b", "a0"},
			{"\x7f", "\x80"}, {"e\u0301", "\u00e9"}, {"Z", "\xff"},
		} {
			if deck.Compare(pair[0], pair[1]) >= 0 || deck.Compare(pair[1], pair[0]) <= 0 {
				return expectf("Compare does not order %q before %q", pair[0], pair[1])
			}
		}
		return nil
	}},
}

// verifyZAccepts checks that deck.VerifyZ lets data through: it has no Z
// card the verifier could find mismatched.
func verifyZAccepts(data string) func(*testing.T) error {
	return func(*testing.T) error {
		if err := deck.VerifyZ([]byte(data)); err != nil {
			return expectf("deck.VerifyZ rejected %q: %v", data, err)
		}
		return nil
	}
}

// verifyCase stores "content" under the name name gives it and checks
// whether the content then verifies, as Section 3.4 decides.
func verifyCase(name func([]byte) string, verifies bool) func(*testing.T) error {
	return func(t *testing.T) error {
		r := newPolicyRepo(t, "2")
		data := []byte("content")
		compressed, err := blob.Compress(data)
		if err != nil {
			return err
		}
		result, err := r.DB().Exec("INSERT INTO blob(uuid, size, content, rcvid) VALUES(?,?,?,1)",
			name(data), len(data), compressed)
		if err != nil {
			return err
		}
		rid, err := result.LastInsertId()
		if err != nil {
			return err
		}
		err = content.Verify(r.DB(), libfossil.FslID(rid))
		if verifies && err != nil {
			return expectf("content under its own name does not verify: %v", err)
		}
		if !verifies && err == nil {
			return expectf("content verifies under the name %q", name(data))
		}
		return nil
	}
}

var writerCases = []apiCase{
	{Case{ID: "artifact.4.6.1.writer-backslash", Req: "AF-4.6.1-3"}, writerRefuses("a\\b.txt")},
	{Case{ID: "artifact.4.6.1.writer-invalid-utf8", Req: "AF-4.6.1-3"}, writerRefuses("a\xff.txt")},
	{Case{ID: "artifact.4.6.1.writer-surrogate", Req: "AF-4.6.1-3"},
		writerRefuses("a\xed\xa0\x80.txt")},
	{Case{ID: "artifact.4.6.4.writer-form", Req: "AF-4.6.4-1"}, func(*testing.T) error {
		at := time.Date(2026, 1, 2, 3, 4, 5, 500e6, time.UTC)
		d := &deck.Deck{D: at, E: &deck.EventCard{Date: at, UUID: h1},
			T: []deck.TagCard{{Type: deck.TagSingleton, Name: "x", UUID: "*"}},
			U: deck.User("u"), W: []byte("w")}
		data, err := d.Marshal()
		if err != nil {
			return err
		}
		form := regexp.MustCompile(`(?m)^[DE] \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?( |$)`)
		if n := len(form.FindAll(data, -1)); n != 2 {
			return expectf("%d of the D and E timestamps in %q have the writer form", n, data)
		}
		return nil
	}},
	// Section 4.7.9: a parser may accept an uppercase K, but a writer emits it
	// lowercase, so a re-written ticket artifact carries a lowercase ID.
	{Case{ID: "artifact.4.7.9.writer-lowercase", Req: "AF-4.7.9-1"},
		writerLowercases(baseTicket, "K "+h1+"\n", "K "+strings.ToUpper(h1)+"\n")},
	{Case{ID: "artifact.4.7.6.writer-permission", Req: "AF-4.7.6-3"}, func(*testing.T) error {
		d := &deck.Deck{D: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			F: []deck.FileCard{{Name: "new.txt", UUID: h1, OldName: "old.txt"}}}
		data, err := d.Marshal()
		if err != nil {
			return err
		}
		if line := "F new.txt " + h1 + " w old.txt\n"; !strings.Contains(string(data), line) {
			return expectf("Marshal wrote %q, want the line %q", data, line)
		}
		return nil
	}},
	{Case{ID: "artifact.6.2.verify-missing-z", Req: "AF-6.2-5"}, verifyZAccepts(baseControl)},
	{Case{ID: "artifact.6.2.verify-too-short", Req: "AF-6.2-5"}, verifyZAccepts("D x\n")},
	// Section 6.2: the state is read from the bytes at n-35 and n-34, not
	// from where card lines begin, so a U card ending in "Z " and 32 bytes
	// is in the mismatch state although no Z card is present.
	{Case{ID: "artifact.6.2.verify-by-position", Req: "AF-6.2-7"}, func(*testing.T) error {
		data := edit(baseControl, "U alice\n", "U aliceZ "+strings.Repeat("0", 32)+"\n")
		if err := deck.VerifyZ([]byte(data)); err == nil {
			return expectf("deck.VerifyZ accepted %q, want the mismatch state", data)
		}
		return nil
	}},
	// The last byte is not LF; Z verification must not look at it.
	{Case{ID: "artifact.6.2.verify-last-byte", Req: "AF-6.2-6"},
		verifyZAccepts(strings.TrimSuffix(signedControl, "\n") + "x")},
	{Case{ID: "artifact.6.2.writer-z", Req: "AF-6.2-4"}, func(*testing.T) error {
		d, err := deck.Parse(withZ(baseControl))
		if err != nil {
			return err
		}
		data, err := d.Marshal()
		if err != nil {
			return err
		}
		// zCard renders the digest in lowercase, as Section 6.2 requires.
		return want("Z card", string(data[len(data)-35:]), zCard(string(data[:len(data)-35])))
	}},
}

// writerLowercases parses base with the line lower replaced by upper, as a
// reader may accept it, and checks that Marshal writes the line lower back.
func writerLowercases(base, lower, upper string) func(*testing.T) error {
	return func(*testing.T) error {
		d, err := deck.Parse(withZ(edit(base, lower, upper)))
		if err != nil {
			return expectf("deck.Parse rejects the uppercase form: %v", err)
		}
		data, err := d.Marshal()
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), lower) {
			return expectf("Marshal wrote %q, want the line %q",
				data, strings.TrimSuffix(lower, "\n"))
		}
		return nil
	}
}

// writerRefuses checks that a check-in refuses to create the filename.
func writerRefuses(name string) func(*testing.T) error {
	return func(t *testing.T) error {
		r := newPolicyRepo(t, "2")
		_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: name, Content: []byte("x\n")}},
			Comment: "c", User: "u", Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		})
		if err == nil {
			return expectf("Checkin created the filename %q", name)
		}
		return nil
	}
}

// rCard is the R value of files, given as name and content pairs already in
// comparator order, computed straight from Section 6.3.2.
func rCard(files ...string) string {
	sum := md5.New()
	for i := 0; i < len(files); i += 2 {
		fmt.Fprintf(sum, "%s %d\n%s", files[i], len(files[i+1]), files[i+1])
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// computeR runs deck.ComputeR over files given as name and content pairs.
func computeR(files ...string) (string, error) {
	d := &deck.Deck{}
	byName := map[string]string{}
	for i := 0; i < len(files); i += 2 {
		name := hash.SHA1([]byte(files[i+1]))
		d.F = append(d.F, deck.FileCard{Name: files[i], UUID: name})
		byName[name] = files[i+1]
	}
	return d.ComputeR(func(uuid string) ([]byte, error) { return []byte(byName[uuid]), nil })
}

var rCardCases = []apiCase{
	{Case{ID: "artifact.6.3.2.example", Req: "AF-6.3-2"}, func(*testing.T) error {
		got, err := computeR("alpha.txt", "hello world\n", "beta.txt", "fossil artifact format\n",
			"gamma.txt", "R card test\n")
		if err != nil {
			return err
		}
		return want("R of Appendix A.3", got, "95feb799536dc053e7968efb7cdd3ec3")
	}},
	{Case{ID: "artifact.6.3.2.empty-tree", Req: "AF-6.3-2"}, func(*testing.T) error {
		got, err := computeR()
		if err != nil {
			return err
		}
		return want("R of no files", got, "d41d8cd98f00b204e9800998ecf8427e")
	}},
	// Given out of order: "a.b" sorts before "a/b" because '.' < '/'.
	{Case{ID: "artifact.6.3.2.byte-order", Req: "AF-6.3-2"}, func(*testing.T) error {
		got, err := computeR("a/b", "two\n", "a.b", "one\n")
		if err != nil {
			return err
		}
		return want("R", got, rCard("a.b", "one\n", "a/b", "two\n"))
	}},
	// Permissions are not part of the R input.
	{Case{ID: "artifact.6.3.2.permission-only", Req: "AF-6.3-2"}, func(*testing.T) error {
		d := &deck.Deck{F: []deck.FileCard{{Name: "run.sh", UUID: h1, Perm: "x"},
			{Name: "link", UUID: h2, Perm: "l"}}}
		got, err := d.ComputeR(func(uuid string) ([]byte, error) { return []byte(uuid[:4]), nil })
		if err != nil {
			return err
		}
		return want("R", got, rCard("link", h2[:4], "run.sh", h1[:4]))
	}},
	{Case{ID: "artifact.6.3.4.writer-r", Req: "AF-6.3-4"}, func(t *testing.T) error {
		d, err := deltaCheckin(newPolicyRepo(t, "2"))
		if err != nil {
			return err
		}
		data, err := d.Marshal()
		if err != nil {
			return err
		}
		if !regexp.MustCompile(`(?m)^R [0-9a-f]{32}$`).Match(data) {
			return expectf("no R card of the writer form in %q", data)
		}
		return nil
	}},
	// The commit-time check, driven through manifest.VerifyRCard, the seam
	// Checkin calls after storing the manifest: manifest.Checkin builds R and
	// the F cards from one file list, so no caller input makes them differ.
	{Case{ID: "artifact.6.3.5.commit-r-lowercase-ok", Req: "AF-6.3-5"}, func(t *testing.T) error {
		return checkStoredR(t, func(r string) string { return r }, false)
	}},
	{Case{ID: "artifact.6.3.5.commit-r-uppercase", Req: "AF-6.3-5"}, func(t *testing.T) error {
		return checkStoredR(t, strings.ToUpper, true)
	}},
	{Case{ID: "artifact.6.3.5.commit-r-wrong", Req: "AF-6.3-5"}, func(t *testing.T) error {
		return checkStoredR(t, func(string) string { return strings.Repeat("0", 32) }, true)
	}},
	{Case{ID: "artifact.6.3.3.delta-r", Req: "AF-6.3-3"}, func(t *testing.T) error {
		r := newPolicyRepo(t, "2")
		d, err := deltaCheckin(r)
		if err != nil {
			return err
		}
		if d.B == "" {
			return expectf("the second check-in is not a delta manifest")
		}
		return want("delta R", d.R, rCard("a.txt", "a2\n", "b.txt", "b\n", "c.txt", "c\n"))
	}},
}

// deltaCheckin commits three files, then a delta check-in that changes one,
// and returns the second manifest.
func deltaCheckin(r *repo.Repo) (*deck.Deck, error) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	files := []manifest.File{{Name: "a.txt", Content: []byte("a\n")},
		{Name: "b.txt", Content: []byte("b\n")}, {Name: "c.txt", Content: []byte("c\n")}}
	first, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: files, Comment: "one", User: "u", Time: at})
	if err != nil {
		return nil, err
	}
	files[0].Content = []byte("a2\n")
	second, _, err := manifest.Checkin(r, manifest.CheckinOpts{Files: files, Comment: "two",
		User: "u", Time: at.Add(time.Minute), Parent: first, Delta: true})
	if err != nil {
		return nil, err
	}
	return manifest.GetManifest(r, second)
}

// newPolicyRepo creates a repository with the given hash-policy value, or
// with no policy row at all when policy is empty.
func newPolicyRepo(t *testing.T, policy string) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conformance.fossil")
	r, err := repo.Create(path, "tester", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	setPolicy(t, r, policy)
	return r
}

func setPolicy(t *testing.T, r *repo.Repo, policy string) {
	t.Helper()
	var err error
	if policy == "" {
		_, err = r.DB().Exec("DELETE FROM config WHERE name='hash-policy'")
	} else {
		err = r.SetConfig("hash-policy", policy)
	}
	if err != nil {
		t.Fatalf("setting hash-policy %q: %v", policy, err)
	}
}

// storeUnder stores data in a repository under policy, then moves the
// repository to next ("" removes the setting), and returns data's name.
func storeUnder(t *testing.T, policy, next string,
	data []byte) (*repo.Repo, libfossil.FslID, string) {
	t.Helper()
	r := newPolicyRepo(t, policy)
	rid, uuid, err := blob.Store(r.DB(), data)
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	setPolicy(t, r, next)
	return r, rid, uuid
}

// namesWith checks the length of the name blob.Store gives new content.
func namesWith(r *repo.Repo, length int) error {
	_, uuid, err := blob.Store(r.DB(), []byte("new content"))
	if err != nil {
		return err
	}
	return want("length of the new name", len(uuid), length)
}

func policyCases() []apiCase {
	var cases []apiCase
	// Policy values 0-4 are sha1, auto, sha3, sha3-only and shun-sha1.
	for policy, length := range []int{40, 40, 64, 64, 64} {
		cases = append(cases, apiCase{
			Case{ID: fmt.Sprintf("artifact.7.1.policy-%d-primary", policy), Req: "AF-7.1-1"},
			func(t *testing.T) error {
				return namesWith(newPolicyRepo(t, fmt.Sprint(policy)), length)
			},
		})
	}
	// The setting holds the ordinal (7.1), so an unrecognized value is a stored
	// one that is not 0 through 4: a policy name, in any case, included.
	// Divergence from fossil: 2.28 reads a non-integer as 0 (sha1) and treats
	// a negative stored value (db.c:2773) as unset to be re-derived under 7.2,
	// and its behavior above 4 is undefined; -01 is followed here, so any
	// stored value outside 0-4 is fatal.
	for _, value := range []string{"banana", "SHA3", "sha3", "5", "-1", ""} {
		cases = append(cases, apiCase{
			Case{ID: "artifact.7.1.unrecognized-" + policyCaseSlug(value), Req: "AF-7.1-2"},
			func(t *testing.T) error {
				_, _, err := blob.Store(newPolicyRepoRaw(t, value).DB(), []byte("x"))
				if err == nil {
					return expectf("blob.Store named content under hash-policy %q", value)
				}
				return nil
			},
		})
	}
	cases = append(cases, policyNameCases()...)
	return append(cases, policyStateCases...)
}

// policyNameCases drive AF-7.1-2 through the value-string surface,
// Repo.SetHashPolicy, which plays fossil's hash-policy command: each of
// Table 7's names is accepted exactly, and any other string is refused,
// leaving the stored policy alone.
func policyNameCases() []apiCase {
	var cases []apiCase
	for ordinal, name := range []string{"sha1", "auto", "sha3", "sha3-only", "shun-sha1"} {
		cases = append(cases, apiCase{
			Case{ID: "artifact.7.1.name-" + name, Req: "AF-7.1-2"},
			func(t *testing.T) error {
				r := newPolicyRepo(t, "")
				if err := r.SetHashPolicy(name); err != nil {
					return expectf("SetHashPolicy(%q): %v", name, err)
				}
				return wantStoredPolicy(r, fmt.Sprint(ordinal))
			},
		})
	}
	refused := map[string][]string{
		"wrong-case": {"SHA1", "Auto", "SHA3", "Sha3-Only", "SHUN-SHA1"},
		"unknown":    {"banana", "sha256", "sha3_only", "sha3 ", " sha1", "sha1\x00", "2"},
		"empty":      {""},
	}
	for _, kind := range []string{"wrong-case", "unknown", "empty"} {
		names := refused[kind]
		cases = append(cases, apiCase{
			Case{ID: "artifact.7.1.name-" + kind, Req: "AF-7.1-2"},
			func(t *testing.T) error { return refusesPolicyNames(t, names) },
		})
	}
	return cases
}

// refusesPolicyNames checks SetHashPolicy fails for every name and leaves
// the stored sha3-only policy as it was.
func refusesPolicyNames(t *testing.T, names []string) error {
	r := newPolicyRepo(t, "3")
	for _, name := range names {
		if err := r.SetHashPolicy(name); err == nil {
			return expectf("SetHashPolicy accepted %q", name)
		}
	}
	return wantStoredPolicy(r, "3")
}

func wantStoredPolicy(r *repo.Repo, ordinal string) error {
	got, err := r.Config("hash-policy")
	if err != nil {
		return err
	}
	return want("stored hash-policy", got, ordinal)
}

// policyCaseSlug makes a stored policy value usable in a case ID.
func policyCaseSlug(value string) string {
	switch value {
	case "":
		return "empty"
	case "-1":
		return "negative"
	}
	return value
}

// newPolicyRepoRaw is newPolicyRepo that stores value even when it is empty,
// where newPolicyRepo reads empty as "no row".
func newPolicyRepoRaw(t *testing.T, value string) *repo.Repo {
	t.Helper()
	r := newPolicyRepo(t, "2")
	// Written below SetConfig, which refuses these: the row stands in for one
	// another tool wrote, which the reader must still reject.
	if _, err := r.DB().Exec("INSERT OR REPLACE INTO config(name, value, mtime) "+
		"VALUES('hash-policy', ?, julianday('now'))", value); err != nil {
		t.Fatalf("setting hash-policy %q: %v", value, err)
	}
	return r
}

var policyStateCases = []apiCase{
	// The write side of AF-7.1-2: a value the reader would reject is refused
	// where it comes in, as fossil's hash-policy command does, and the stored
	// row is left alone.
	{Case{ID: "artifact.7.1.set-unrecognized", Req: "AF-7.1-2"}, func(t *testing.T) error {
		r := newPolicyRepo(t, "2")
		for _, v := range []string{"sha3", "SHA3", "5", "-1", ""} {
			if err := r.SetConfig("hash-policy", v); err == nil {
				return expectf("SetConfig accepted hash-policy %q", v)
			}
		}
		got, err := r.Config("hash-policy")
		if err != nil {
			return err
		}
		return want("stored hash-policy", got, "2")
	}},
	{Case{ID: "artifact.7.2.default-empty", Req: "AF-7.2-1"}, func(t *testing.T) error {
		return namesWith(newPolicyRepo(t, ""), 64)
	}},
	{Case{ID: "artifact.7.2.default-sha1-repo", Req: "AF-7.2-1"}, func(t *testing.T) error {
		r, _, _ := storeUnder(t, "0", "", []byte("old"))
		return namesWith(r, 40)
	}},
	{Case{ID: "artifact.7.2.default-sha3-repo", Req: "AF-7.2-1"}, func(t *testing.T) error {
		r, _, _ := storeUnder(t, "2", "", []byte("old"))
		return namesWith(r, 64)
	}},
	{Case{ID: "artifact.7.4.ratchet-naming", Req: "AF-7.4-1"}, func(t *testing.T) error {
		r, err := ratchet(t)
		if err != nil {
			return err
		}
		return namesWith(r, 64)
	}},
	{Case{ID: "artifact.7.4.ratchet-persisted", Req: "AF-7.4-1"}, func(t *testing.T) error {
		r, err := ratchet(t)
		if err != nil {
			return err
		}
		value, err := r.Config("hash-policy")
		if err != nil {
			return err
		}
		return want("hash-policy after the ratchet", value, "2")
	}},
	{Case{ID: "artifact.7.5.sha3-keeps-sha1-name", Req: "AF-7.5-1"}, keepsName("0", "2")},
	{Case{ID: "artifact.7.5.sha1-keeps-sha3-name", Req: "AF-7.5-1"}, keepsName("2", "0")},
	{Case{ID: "artifact.7.5.sha3-only-separate-name", Req: "AF-7.5-2"}, separateName("3")},
	{Case{ID: "artifact.7.5.shun-sha1-separate-name", Req: "AF-7.5-2"}, separateName("4")},
	{Case{ID: "artifact.7.6.no-rename", Req: "AF-7.6-1"}, func(t *testing.T) error {
		r, rid, uuid := storeUnder(t, "0", "2", []byte("old"))
		got, err := r.UUIDByRID(int64(rid))
		if err != nil {
			return err
		}
		return want("name after the policy change", got, uuid)
	}},
}

// ratchet stores, under auto, an artifact whose supplied name is a SHA3
// one, the way a sync receive does.
func ratchet(t *testing.T) (*repo.Repo, error) {
	r := newPolicyRepo(t, "1")
	source := []byte("source content\n")
	srcRid, _, err := blob.Store(r.DB(), source)
	if err != nil {
		return nil, err
	}
	target := []byte("source content\nand more\n")
	d, err := delta.Create(source, target)
	if err != nil {
		return nil, err
	}
	if _, err := blob.StoreDeltaRaw(r.DB(), hash.SHA3(target), d, srcRid, nil); err != nil {
		return nil, err
	}
	return r, nil
}

// keepsName stores content under one policy, then again under another that
// may consult the first's algorithm, and wants the first name back.
func keepsName(first, second string) func(*testing.T) error {
	return func(t *testing.T) error {
		data := []byte("shared content")
		r, rid, uuid := storeUnder(t, first, second, data)
		again, name, err := blob.Store(r.DB(), data)
		if err != nil {
			return err
		}
		return want("second store", fmt.Sprint(again, " ", name), fmt.Sprint(rid, " ", uuid))
	}
}

// separateName stores content under sha1, then again under a policy that
// disallows the alternative hash, and wants a second, SHA3 name.
func separateName(strict string) func(*testing.T) error {
	return func(t *testing.T) error {
		data := []byte("shared content")
		r, _, _ := storeUnder(t, "0", strict, data)
		_, name, err := blob.Store(r.DB(), data)
		if err != nil {
			return err
		}
		return want("second name", name, hash.SHA3(data))
	}
}

// checkStoredR checks in a manifest, stores a copy whose R card is mutate(R)
// under a fresh Z card (so it still parses), and runs the commit-time R check
// on the copy. wantFail says whether the check must reject it.
func checkStoredR(t *testing.T, mutate func(string) string, wantFail bool) error {
	r := newPolicyRepo(t, "2")
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{{Name: "a.txt", Content: []byte("one\n")}},
		User:  "u", Comment: "c", Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	if err != nil {
		return err
	}
	raw, err := content.Expand(r.DB(), rid)
	if err != nil {
		return err
	}
	d, err := deck.Parse(raw)
	if err != nil {
		return err
	}
	bad := strings.Replace(string(raw), "R "+d.R, "R "+mutate(d.R), 1)
	bad = bad[:strings.LastIndex(bad, "Z ")]
	sum := md5.Sum([]byte(bad))
	bad += "Z " + hex.EncodeToString(sum[:]) + "\n"
	var verr error
	err = r.WithTx(func(tx *db.Tx) error {
		brid, _, err := blob.Store(tx, []byte(bad))
		if err != nil {
			return err
		}
		verr = manifest.VerifyRCard(tx, content.NewCache(1<<20), brid)
		return nil
	})
	if err != nil {
		return err
	}
	if wantFail && verr == nil {
		return expectf("the commit-time R check accepted R card %q", mutate(d.R))
	}
	if !wantFail && verr != nil {
		return expectf("the commit-time R check rejected a correct R card: %v", verr)
	}
	return nil
}
