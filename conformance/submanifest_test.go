package conformance

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// The submanifest suite checks go-libfossil's reader against
// draft-fossil-submanifests-00. Card-level cases are byte strings given to
// deck.Parse, as in the artifact suite; tree cases store artifacts in a
// repository and read them back through the same entry points every consumer
// uses. fossil(1) reads no S card, so nothing here is checked against it;
// the binding byte vectors of Appendix A, as authored for zit, stand in.

// smCase is one submanifest case. run reports nil when go-libfossil does what
// the draft requires.
type smCase struct {
	Case
	run func(t *testing.T) error
}

func submanifestSuiteCases() []Case {
	var all []Case
	for _, c := range submanifestCases() {
		all = append(all, c.Case)
	}
	return all
}

func TestSubmanifest(t *testing.T) {
	for _, c := range submanifestCases() {
		check(t, c.Case, func() error { return c.run(t) })
	}
}

func submanifestCases() []smCase {
	var all []smCase
	for _, group := range [][]smCase{
		smCardCases(), smTypeCases(), smWalkCases(), smStateCases(),
		smTransferCases(), smVectorCases(),
	} {
		all = append(all, group...)
	}
	return all
}

// Hashes for card-level cases, where only their shape matters.
const (
	smH40 = "0123456789abcdef0123456789abcdef01234567"
	smH64 = "aa9d404c14fe4dadd2ab398961fb0dfe650a877c061f650d8233a5888e920acf"
	smTS  = "2026-07-26T00:00:00"
)

func smAccepts(id, req, body string, want func(*deck.Deck) error) smCase {
	return smCase{Case{ID: id, Req: req}, func(*testing.T) error {
		d, err := deck.Parse(withZ(body))
		if err != nil {
			return fmt.Errorf("rejected: %v", err)
		}
		if want != nil {
			return want(d)
		}
		return nil
	}}
}

func smRejects(id, req, body string) smCase {
	return smCase{Case{ID: id, Req: req}, func(*testing.T) error {
		if _, err := deck.Parse(withZ(body)); err == nil {
			return fmt.Errorf("accepted")
		}
		return nil
	}}
}

func smType(want deck.ArtifactType) func(*deck.Deck) error {
	return func(d *deck.Deck) error {
		if d.Type != want {
			return fmt.Errorf("type %d, want %d", d.Type, want)
		}
		return nil
	}
}

func smCardCases() []smCase {
	return []smCase{
		smAccepts("submanifest.3.1.s-defined", "SM-3.1-1", "S a "+smH64+"\n", nil),
		smRejects("submanifest.3.1.o-undefined", "SM-3.1-1", "D "+smTS+"\nO x\n"),
		smRejects("submanifest.3.1.v-undefined", "SM-3.1-1", "D "+smTS+"\nV x\n"),
		smRejects("submanifest.3.1.x-undefined", "SM-3.1-1", "D "+smTS+"\nX x\n"),
		smRejects("submanifest.3.1.y-undefined", "SM-3.1-1", "D "+smTS+"\nY x\n"),
		smRejects("submanifest.3.1.outside-a-z", "SM-3.1-1", "D "+smTS+"\na x\n"),
		smRejects("submanifest.3.2.third-token", "SM-3.2-1", "S a "+smH64+" x\n"),
		smRejects("submanifest.3.2.no-hash", "SM-3.2-1", "S a\n"),
		smAccepts("submanifest.3.3.filename-decoded", "SM-3.3-1", `S a\sb `+smH64+"\n",
			func(d *deck.Deck) error { return want("S[0].Name", d.S[0].Name, "a b") }),
		smRejects("submanifest.3.3.hash-not-decoded", "SM-3.3-1",
			`S a `+smH64[:62]+`\s`+"\n"),
		smAccepts("submanifest.3.4.ascending", "SM-3.4-1",
			"S a "+smH64+"\nS b "+smH40+"\n",
			func(d *deck.Deck) error { return want("len(S)", len(d.S), 2) }),
		smRejects("submanifest.3.4.equal", "SM-3.4-1", "S a "+smH64+"\nS a "+smH40+"\n"),
		smRejects("submanifest.3.4.descending", "SM-3.4-1", "S b "+smH64+"\nS a "+smH40+"\n"),
		// The comparator orders decoded names: "a b" (0x20) before "a\b" (0x5C).
		smAccepts("submanifest.3.4.decoded-order", "SM-3.4-1",
			`S a\sb `+smH64+"\n"+`S a\\b `+smH40+"\n", nil),
		smAccepts("submanifest.3.4.r-s-t", "SM-3.4-2",
			"D "+smTS+"\nR "+md5Token+"\nS a "+smH64+"\nT +x *\nU u\n", smType(deck.Checkin)),
		smRejects("submanifest.3.4.s-after-t", "SM-3.4-2",
			"D "+smTS+"\nT +x *\nS a "+smH64+"\nU u\n"),
		smAccepts("submanifest.3.5.sha1", "SM-3.5-1", "S a "+smH40+"\n", nil),
		smAccepts("submanifest.3.5.upper-case", "SM-3.5-1", "S a "+strings.ToUpper(smH64)+"\n", nil),
		smRejects("submanifest.3.5.short", "SM-3.5-1", "S a "+smH40[:39]+"\n"),
		smRejects("submanifest.3.5.non-hex", "SM-3.5-1", "S a "+smH40[:39]+"g\n"),
		smRejects("submanifest.3.6.dot-dot", "SM-3.6-1", "S ../a "+smH64+"\n"),
		smRejects("submanifest.3.6.absolute", "SM-3.6-1", "S /a "+smH64+"\n"),
	}
}

func smTypeCases() []smCase {
	tree := "C t\nD " + smTS + "\nN text/plain\nP " + smH40 + "\nQ +" + smH40 + "\nR " + md5Token +
		"\nS a " + smH64 + "\nT +x *\nU u\n"
	bases := map[string]deck.ArtifactType{
		baseManifest: deck.Checkin, baseDelta: deck.Checkin, baseCluster: deck.Cluster,
		baseControl: deck.Control, baseWiki: deck.Wiki, baseTicket: deck.Ticket,
		baseAttachment: deck.Attachment, baseEvent: deck.Event, baseForum: deck.ForumPost,
	}
	return []smCase{
		smAccepts("submanifest.4.2.tree-cards", "SM-4.2-1", tree, smType(deck.Checkin)),
		smRejects("submanifest.4.2.tree-without-z", "SM-4.2-1", "D "+smTS+"\nS a "+smH64+"\n"+
			"Z "+md5Token+"x\n"),
		smAccepts("submanifest.4.2.leaf", "SM-4.2-2", "F a "+smH64+"\n", smType(deck.Submanifest)),
		smRejects("submanifest.4.2.leaf-with-u", "SM-4.2-2", "F a "+smH64+"\nU u\n"),
		smRejects("submanifest.4.2.leaf-with-r", "SM-4.2-2", "F a "+smH64+"\nR "+md5Token+"\n"),
		smAccepts("submanifest.4.3.leaf-reassigned", "SM-4.3-1", "F a "+smH64+"\n",
			smType(deck.Submanifest)),
		smAccepts("submanifest.4.3.interior-reassigned", "SM-4.3-1", "S a "+smH64+"\n",
			smType(deck.Submanifest)),
		smAccepts("submanifest.4.3.s-sets-manifest", "SM-4.3-1", "D "+smTS+"\nS a "+smH64+"\n",
			smType(deck.Checkin)),
		smRejects("submanifest.4.3.d-less-comment", "SM-4.3-1", "C x\nF a "+smH64+"\n"),
		{Case{ID: "submanifest.4.3.base-types-unchanged", Req: "SM-4.3-2"}, func(*testing.T) error {
			for body, typ := range bases {
				d, err := deck.Parse(withZ(body))
				if err != nil {
					return fmt.Errorf("a base artifact is rejected: %v", err)
				}
				if d.Type != typ {
					return fmt.Errorf("type %d, want %d, for %.20q", d.Type, typ, body)
				}
			}
			return nil
		}},
		smRejects("submanifest.4.4.b-and-s", "SM-4.4-1", "B "+smH40+"\nD "+smTS+"\nS a "+smH64+"\n"),
		smRejects("submanifest.4.4.manifest-f-and-s", "SM-4.4-2",
			"D "+smTS+"\nF a "+smH40+"\nS b "+smH64+"\n"),
		smRejects("submanifest.4.4.sub-f-and-s", "SM-4.4-3", "F a "+smH40+"\nS b "+smH64+"\n"),
		smRejects("submanifest.4.4.sub-neither", "SM-4.4-3", "Z "+md5Token+"\n"),
		smRejects("submanifest.4.4.sub-delete-marker", "SM-4.4-4", "F a\n"),
		smAccepts("submanifest.4.5.interior-first-card-s", "SM-4.5-1", "S a "+smH64+"\n", nil),
		// F sets MANIFEST as it is read; only the post-pass, once every card
		// is in, finds no D card and makes the artifact a sub-manifest, and
		// the required-card check then asks for no D card.
		smAccepts("submanifest.9.1.type-after-cards", "SM-9.1-1",
			"F a "+smH64+"\nF b "+smH40+"\n", smType(deck.Submanifest)),
	}
}

// smFixture is one repository under the sha3 hash policy, the policy the
// Appendix A artifacts are named under.
type smFixture struct {
	r       *repo.Repo
	content map[string][]byte // file name to content
}

func newSMFixture(t *testing.T) (*smFixture, error) {
	r, err := repo.Create(filepath.Join(t.TempDir(), "sm.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetHashPolicy("sha3"); err != nil {
		return nil, err
	}
	return &smFixture{r: r, content: map[string][]byte{}}, nil
}

func (f *smFixture) store(data []byte) (string, error) {
	var uuid string
	err := f.r.WithTx(func(tx *db.Tx) error {
		var err error
		_, uuid, err = blob.Store(tx, data)
		return err
	})
	return uuid, err
}

func (f *smFixture) storeDeck(d *deck.Deck) (string, error) {
	data, err := d.Marshal()
	if err != nil {
		return "", err
	}
	return f.store(data)
}

func (f *smFixture) file(name string) (deck.FileCard, error) {
	data := []byte("content of " + name + "\n")
	f.content[name] = data
	uuid, err := f.store(data)
	return deck.FileCard{Name: name, UUID: uuid}, err
}

// leaves stores one leaf sub-manifest per group of file names, storing the
// files too, and returns the S cards that reference the leaves. Nothing is
// linked yet.
func (f *smFixture) leaves(groups ...[]string) ([]deck.SubmanifestCard, error) {
	var refs []deck.SubmanifestCard
	for _, names := range groups {
		var cards []deck.FileCard
		for _, name := range names {
			c, err := f.file(name)
			if err != nil {
				return nil, err
			}
			cards = append(cards, c)
		}
		uuid, err := f.storeDeck(&deck.Deck{F: cards})
		if err != nil {
			return nil, err
		}
		refs = append(refs, deck.SubmanifestCard{Name: names[0], UUID: uuid})
	}
	return refs, nil
}

func (f *smFixture) interior(refs ...deck.SubmanifestCard) (deck.SubmanifestCard, error) {
	uuid, err := f.storeDeck(&deck.Deck{S: refs})
	return deck.SubmanifestCard{Name: refs[0].Name, UUID: uuid}, err
}

var smTime = time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)

// checkin stores a tree check-in over refs on trunk and crosslinks.
func (f *smFixture) checkin(at time.Time, refs []deck.SubmanifestCard, parents ...string) (libfossil.FslID, string, error) {
	d := &deck.Deck{C: "tree", D: at, S: refs, P: parents, U: deck.User("u")}
	if len(parents) == 0 {
		d.T = []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagPropagating, Name: "sym-trunk", UUID: "*"},
		}
	}
	return f.link(d)
}

func (f *smFixture) link(d *deck.Deck) (libfossil.FslID, string, error) {
	uuid, err := f.storeDeck(d)
	if err != nil {
		return 0, "", err
	}
	if _, err := manifest.Crosslink(f.r); err != nil {
		return 0, "", err
	}
	rid, _ := blob.Exists(f.r.DB(), uuid)
	return rid, uuid, nil
}

func (f *smFixture) names(rid libfossil.FslID) (string, error) {
	files, err := manifest.ListFiles(f.r, rid)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range files {
		names = append(names, e.Name)
	}
	return strings.Join(names, " "), nil
}

func (f *smFixture) count(query string, args ...any) int {
	var n int
	if err := f.r.DB().QueryRow(query, args...).Scan(&n); err != nil {
		return -1
	}
	return n
}

// smRepoCase builds a case that runs against a fresh fixture.
func smRepoCase(id, req string, run func(f *smFixture) error) smCase {
	return smCase{Case{ID: id, Req: req}, func(t *testing.T) error {
		f, err := newSMFixture(t)
		if err != nil {
			return err
		}
		return run(f)
	}}
}

// smFault builds a tree over refs and requires ListFiles to refuse it with a
// tree fault, never listing it in part.
func smFault(id, req string, build func(f *smFixture) ([]deck.SubmanifestCard, error)) smCase {
	return smRepoCase(id, req, func(f *smFixture) error {
		refs, err := build(f)
		if err != nil {
			return err
		}
		rid, _, err := f.checkin(smTime, refs)
		if err != nil {
			return err
		}
		if names, err := f.names(rid); err == nil {
			return fmt.Errorf("listed %q", names)
		}
		if n := f.count("SELECT count(*) FROM event WHERE objid=?", rid); n != 0 {
			return fmt.Errorf("a tree that cannot be walked was linked")
		}
		return nil
	})
}

func smWalkCases() []smCase {
	twoLevel := func(f *smFixture) (libfossil.FslID, error) {
		refs, err := f.leaves([]string{"README.md", "src/main.c"}, []string{"src/node11.c", "src/util.c"})
		if err != nil {
			return 0, err
		}
		rid, _, err := f.checkin(smTime, refs)
		return rid, err
	}
	return []smCase{
		smRepoCase("submanifest.5.tree-manifest-walked", "SM-5-1", func(f *smFixture) error {
			rid, err := twoLevel(f)
			if err != nil {
				return err
			}
			got, err := f.names(rid)
			return wantNoErr(err, "ListFiles", want("files", got, "README.md src/main.c src/node11.c src/util.c"))
		}),
		smRepoCase("submanifest.5.1.three-level", "SM-5.1-1", func(f *smFixture) error {
			refs, err := f.leaves([]string{"a", "b"}, []string{"c"}, []string{"d", "e"}, []string{"f"})
			if err != nil {
				return err
			}
			left, err := f.interior(refs[0], refs[1])
			if err != nil {
				return err
			}
			right, err := f.interior(refs[2], refs[3])
			if err != nil {
				return err
			}
			rid, _, err := f.checkin(smTime, []deck.SubmanifestCard{left, right})
			if err != nil {
				return err
			}
			got, err := f.names(rid)
			return wantNoErr(err, "ListFiles", want("files", got, "a b c d e f"))
		}),
		smFault("submanifest.5.1.siblings-overlap", "SM-5.1-2", func(f *smFixture) ([]deck.SubmanifestCard, error) {
			refs, err := f.leaves([]string{"a", "c"}, []string{"b"})
			return refs, err
		}),
		smFault("submanifest.5.1.leaf-unsorted-against-s", "SM-5.1-2", func(f *smFixture) ([]deck.SubmanifestCard, error) {
			refs, err := f.leaves([]string{"b"}, []string{"c"})
			if err != nil {
				return nil, err
			}
			// The second S card is named before the first leaf's last file.
			return []deck.SubmanifestCard{refs[0], {Name: "bb", UUID: refs[1].UUID}}, nil
		}),
		smFault("submanifest.5.1.absent", "SM-5.1-3", func(f *smFixture) ([]deck.SubmanifestCard, error) {
			return []deck.SubmanifestCard{{Name: "a", UUID: hash.AlgSHA3.Hash([]byte("absent"))}}, nil
		}),
		smFault("submanifest.5.1.not-a-submanifest", "SM-5.1-4", func(f *smFixture) ([]deck.SubmanifestCard, error) {
			c, err := f.file("a")
			return []deck.SubmanifestCard{{Name: "a", UUID: c.UUID}}, err
		}),
		// A reference cycle cannot be built from content-addressed
		// artifacts; what stops any path from looping is shown here by the
		// depth bound, and the repeated-artifact check itself by
		// internal/manifest's TestTreeWalkRefusesRepeatedArtifact.
		smFault("submanifest.5.1.no-unbounded-path", "SM-5.1-5", func(f *smFixture) ([]deck.SubmanifestCard, error) {
			return smChain(f, 17)
		}),
		smFault("submanifest.5.1.depth-17", "SM-5.1-6", func(f *smFixture) ([]deck.SubmanifestCard, error) {
			return smChain(f, 17)
		}),
		smRepoCase("submanifest.5.1.depth-16", "SM-5.1-6", func(f *smFixture) error {
			refs, err := smChain(f, 16)
			if err != nil {
				return err
			}
			rid, _, err := f.checkin(smTime, refs)
			if err != nil {
				return err
			}
			got, err := f.names(rid)
			return wantNoErr(err, "ListFiles", want("files", got, "a"))
		}),
		smFault("submanifest.5.1.leaf-first-name", "SM-5.1-7", func(f *smFixture) ([]deck.SubmanifestCard, error) {
			refs, err := f.leaves([]string{"b"})
			return []deck.SubmanifestCard{{Name: "a", UUID: refs[0].UUID}}, err
		}),
		smFault("submanifest.5.1.interior-first-name", "SM-5.1-8", func(f *smFixture) ([]deck.SubmanifestCard, error) {
			refs, err := f.leaves([]string{"b"})
			if err != nil {
				return nil, err
			}
			node, err := f.interior(refs[0])
			return []deck.SubmanifestCard{{Name: "a", UUID: node.UUID}}, err
		}),
		smRepoCase("submanifest.5.1.mlink-like-flat", "SM-5.1-9", smMlinkLikeFlat),
		smRepoCase("submanifest.4.4.tokens-unaltered", "SM-4.4-5", func(f *smFixture) error {
			c, err := f.file("x.sh")
			if err != nil {
				return err
			}
			c.Perm, c.OldName = "x", "old.sh"
			leaf, err := f.storeDeck(&deck.Deck{F: []deck.FileCard{c}})
			if err != nil {
				return err
			}
			rid, _, err := f.checkin(smTime, []deck.SubmanifestCard{{Name: "x.sh", UUID: leaf}})
			if err != nil {
				return err
			}
			files, err := manifest.ListFiles(f.r, rid)
			if err != nil {
				return err
			}
			if len(files) != 1 || files[0].UUID != c.UUID || files[0].Perm != "x" {
				return fmt.Errorf("listed %+v", files)
			}
			if err := want("executable mlink rows",
				f.count("SELECT count(*) FROM mlink WHERE mid=? AND mperm=1", rid), 1); err != nil {
				return err
			}
			// The old-name token reaches crosslink: a tree child renaming
			// x.sh to y.sh records the prior name.
			_, parent, err := f.checkin(smTime, []deck.SubmanifestCard{{Name: "x.sh", UUID: leaf}})
			if err != nil {
				return err
			}
			renamed := deck.FileCard{Name: "y.sh", UUID: c.UUID, Perm: "x", OldName: "x.sh"}
			leaf2, err := f.storeDeck(&deck.Deck{F: []deck.FileCard{renamed}})
			if err != nil {
				return err
			}
			child, _, err := f.checkin(smTime.Add(time.Hour), []deck.SubmanifestCard{{Name: "y.sh", UUID: leaf2}}, parent)
			if err != nil {
				return err
			}
			return want("rename rows", f.count(`SELECT count(*) FROM mlink m
				JOIN filename p ON p.fnid=m.pfnid WHERE m.mid=? AND p.name='x.sh'`, child), 1)
		}),
		smRepoCase("submanifest.5.2.missing-is-fatal", "SM-5.2-1", smMissingIsFatal),
		smRepoCase("submanifest.5.2.gap-recorded-and-retried", "SM-5.2-2", smGapRetried),
		smRepoCase("submanifest.5.3.lookup", "SM-5.3-1", func(f *smFixture) error {
			rid, err := twoLevel(f)
			if err != nil {
				return err
			}
			for name, present := range map[string]bool{
				"README.md": true, "src/main.c": true, "src/util.c": true,
				"A": false, "src/m": false, "src/zzz": false,
			} {
				_, found, err := manifest.LookupFile(f.r, rid, name)
				if err != nil || found != present {
					return fmt.Errorf("LookupFile(%q) = %v, %v", name, found, err)
				}
			}
			return nil
		}),
		smRepoCase("submanifest.5.3.depth-bound", "SM-5.3-2", func(f *smFixture) error {
			refs, err := smChain(f, 17)
			if err != nil {
				return err
			}
			uuid, err := f.storeDeck(&deck.Deck{C: "t", D: smTime, S: refs, U: deck.User("u")})
			if err != nil {
				return err
			}
			rid, _ := blob.Exists(f.r.DB(), uuid)
			if _, _, err := manifest.LookupFile(f.r, rid, "a"); err == nil {
				return fmt.Errorf("a lookup descended past 16 sub-manifests")
			}
			return nil
		}),
		smRepoCase("submanifest.5.4.delta-over-tree", "SM-5.4-1", smDeltaOverTree),
		smRepoCase("submanifest.5.4.commit-on-tree-is-flat", "SM-5.4-2", smCommitOnTree),
		smRepoCase("submanifest.5.5.r-card", "SM-5.5-1", func(f *smFixture) error {
			refs, err := f.leaves([]string{"a", "b"}, []string{"c"})
			if err != nil {
				return err
			}
			d := &deck.Deck{C: "t", D: smTime, R: smR(f.content, "a", "b", "c"), S: refs, U: deck.User("u")}
			rid, _, err := f.link(d)
			if err != nil {
				return err
			}
			return f.r.WithTx(func(tx *db.Tx) error { return manifest.VerifyRCard(tx, nil, rid) })
		}),
		smRepoCase("submanifest.9.3.consumers", "SM-9.3-1", smConsumers),
		smRepoCase("submanifest.10.bounds", "SM-10-1", func(f *smFixture) error {
			refs, err := smChain(f, 17)
			if err != nil {
				return err
			}
			uuid, err := f.storeDeck(&deck.Deck{C: "t", D: smTime, S: refs, U: deck.User("u")})
			if err != nil {
				return err
			}
			rid, _ := blob.Exists(f.r.DB(), uuid)
			if _, err := manifest.ListFiles(f.r, rid); err == nil {
				return fmt.Errorf("the walk exceeded the depth bound")
			}
			return nil
		}),
	}
}

// smChain is a tree whose single leaf lies depth sub-manifests down.
func smChain(f *smFixture, depth int) ([]deck.SubmanifestCard, error) {
	refs, err := f.leaves([]string{"a"})
	if err != nil {
		return nil, err
	}
	ref := refs[0]
	for level := 1; level < depth; level++ {
		if ref, err = f.interior(ref); err != nil {
			return nil, err
		}
	}
	return []deck.SubmanifestCard{ref}, nil
}

// smR is the R card over the named files' content.
func smR(content map[string][]byte, names ...string) string {
	sort.Strings(names)
	h := md5.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s %d\n", name, len(content[name]))
		h.Write(content[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func wantNoErr(err error, what string, rest error) error {
	if err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}
	return rest
}

// smMlinkLikeFlat: a tree child of a tree parent gets the file-change rows a
// flat child of a flat parent gets for the same change.
func smMlinkLikeFlat(f *smFixture) error {
	refs, err := f.leaves([]string{"a", "b"}, []string{"c"})
	if err != nil {
		return err
	}
	_, parent, err := f.checkin(smTime, refs)
	if err != nil {
		return err
	}
	b2, err := f.store([]byte("b two\n"))
	if err != nil {
		return err
	}
	aCard := deck.FileCard{Name: "a", UUID: mustUUIDOf(f, "a")}
	leaf, err := f.storeDeck(&deck.Deck{F: []deck.FileCard{aCard, {Name: "b", UUID: b2}}})
	if err != nil {
		return err
	}
	child, _, err := f.checkin(smTime.Add(time.Hour), []deck.SubmanifestCard{{Name: "a", UUID: leaf}}, parent)
	if err != nil {
		return err
	}
	rows, err := f.r.DB().Query(`SELECT fn.name, m.fid=0 FROM mlink m JOIN filename fn
		ON fn.fnid=m.fnid WHERE m.mid=? ORDER BY fn.name`, child)
	if err != nil {
		return err
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		var deleted bool
		if err := rows.Scan(&name, &deleted); err != nil {
			return err
		}
		got = append(got, fmt.Sprintf("%s:%v", name, deleted))
	}
	return want("mlink rows", strings.Join(got, " "), "b:false c:true")
}

func mustUUIDOf(f *smFixture, name string) string {
	return hash.AlgSHA3.Hash(f.content[name])
}

func smMissingIsFatal(f *smFixture) error {
	refs, err := f.leaves([]string{"a"})
	if err != nil {
		return err
	}
	absent := deck.SubmanifestCard{Name: "b", UUID: hash.AlgSHA3.Hash([]byte("not stored"))}
	rid, _, err := f.checkin(smTime, append(refs, absent))
	if err != nil {
		return err
	}
	if _, err := manifest.ListFiles(f.r, rid); err == nil {
		return fmt.Errorf("ListFiles listed a tree with an absent sub-manifest")
	} else if got, ok := manifest.MissingSubmanifest(err); !ok || got != absent.UUID {
		return fmt.Errorf("ListFiles error %v does not name the absent sub-manifest", err)
	}
	if _, _, err := manifest.LookupFile(f.r, rid, "b"); err == nil {
		return fmt.Errorf("LookupFile resolved a name under an absent sub-manifest")
	}
	if n := f.count("SELECT count(*) FROM mlink WHERE mid=?", rid); n != 0 {
		return fmt.Errorf("%d file-change rows for an incomplete tree", n)
	}
	return nil
}

func smGapRetried(f *smFixture) error {
	file, err := f.file("b")
	if err != nil {
		return err
	}
	leaf, err := (&deck.Deck{F: []deck.FileCard{file}}).Marshal()
	if err != nil {
		return err
	}
	absent := deck.SubmanifestCard{Name: "b", UUID: hash.AlgSHA3.Hash(leaf)}
	rid, _, err := f.checkin(smTime, []deck.SubmanifestCard{absent})
	if err != nil {
		return err
	}
	phantom, ok := blob.Exists(f.r.DB(), absent.UUID)
	if !ok {
		return fmt.Errorf("no phantom for the absent sub-manifest")
	}
	gap := "SELECT count(*) FROM orphan WHERE rid=? AND baseline=?"
	if n := f.count(gap, rid, phantom); n != 1 {
		return fmt.Errorf("gap rows %d, want 1", n)
	}
	if _, err := manifest.Crosslink(f.r); err != nil {
		return err
	}
	if n := f.count(gap, rid, phantom); n != 1 {
		return fmt.Errorf("gap rows after a second sweep %d, want 1", n)
	}
	if _, err := f.store(leaf); err != nil {
		return err
	}
	manifest.AfterDephantomize(context.Background(), f.r, phantom)
	if n := f.count("SELECT count(*) FROM event WHERE objid=?", rid); n != 1 {
		return fmt.Errorf("the tree was not linked when its sub-manifest arrived")
	}
	return nil
}

func smDeltaOverTree(f *smFixture) error {
	refs, err := f.leaves([]string{"a", "b"}, []string{"c"})
	if err != nil {
		return err
	}
	_, base, err := f.checkin(smTime, refs)
	if err != nil {
		return err
	}
	a2, err := f.store([]byte("a two\n"))
	if err != nil {
		return err
	}
	f.content["a"] = []byte("a two\n")
	delta := &deck.Deck{B: base, C: "delta", D: smTime.Add(time.Hour), P: []string{base},
		F: []deck.FileCard{{Name: "a", UUID: a2}, {Name: "c"}}, U: deck.User("u")}
	delta.R = smR(f.content, "a", "b")
	rid, _, err := f.link(delta)
	if err != nil {
		return err
	}
	got, err := f.names(rid)
	if err := wantNoErr(err, "ListFiles", want("files", got, "a b")); err != nil {
		return err
	}
	e, found, err := manifest.LookupFile(f.r, rid, "b")
	if err != nil || !found || e.UUID != mustUUIDOf(f, "b") {
		return fmt.Errorf("LookupFile(b) through the tree baseline = %+v, %v, %v", e, found, err)
	}
	return f.r.WithTx(func(tx *db.Tx) error { return manifest.VerifyRCard(tx, nil, rid) })
}

func smCommitOnTree(f *smFixture) error {
	refs, err := f.leaves([]string{"a", "b"})
	if err != nil {
		return err
	}
	parent, _, err := f.checkin(smTime, refs)
	if err != nil {
		return err
	}
	rid, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a", Content: f.content["a"]}, {Name: "b", Content: []byte("b2\n")}},
		Comment: "on a tree", User: "u", Time: smTime.Add(time.Hour), Parent: parent, Delta: true,
	})
	if err != nil {
		return err
	}
	d, err := manifest.GetManifest(f.r, rid)
	if err != nil {
		return err
	}
	if d.B != "" || len(d.S) != 0 {
		return fmt.Errorf("commit on a tree wrote B=%q and %d S cards", d.B, len(d.S))
	}
	return nil
}

// smConsumers: checkout, diff, rebuild, annotate and commit all read a tree
// check-in's file set through the seam, so each sees all of its files.
func smConsumers(f *smFixture) error {
	refs, err := f.leaves([]string{"a.txt", "b.txt"}, []string{"c.txt"})
	if err != nil {
		return err
	}
	rid, _, err := f.checkin(smTime, refs)
	if err != nil {
		return err
	}
	// Commit on it, then rebuild, then compare the two file sets.
	next, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: f.content["a.txt"]},
			{Name: "b.txt", Content: []byte("b changed\n")},
			{Name: "c.txt", Content: f.content["c.txt"]},
		},
		Comment: "edit", User: "u", Time: smTime.Add(time.Hour), Parent: rid,
	})
	if err != nil {
		return fmt.Errorf("commit: %v", err)
	}
	if _, err := verify.Rebuild(f.r); err != nil {
		return fmt.Errorf("rebuild: %v", err)
	}
	if n := f.count("SELECT count(*) FROM mlink WHERE mid=? AND pmid=?", next, rid); n != 1 {
		return fmt.Errorf("rebuilt diff rows %d, want 1 (b.txt)", n)
	}
	got, err := f.names(rid)
	return wantNoErr(err, "ListFiles after rebuild", want("files", got, "a.txt b.txt c.txt"))
}

func smStateCases() []smCase {
	return []smCase{
		smRepoCase("submanifest.7.1.phantoms-private", "SM-7.1-1", func(f *smFixture) error {
			missing := deck.SubmanifestCard{Name: "z", UUID: hash.AlgSHA3.Hash([]byte("never"))}
			uuid, err := f.storeDeck(&deck.Deck{S: []deck.SubmanifestCard{missing}})
			if err != nil {
				return err
			}
			rid, _ := blob.Exists(f.r.DB(), uuid)
			if err := content.MakePrivate(f.r.DB(), int64(rid)); err != nil {
				return err
			}
			if _, err := manifest.Crosslink(f.r); err != nil {
				return err
			}
			prid, ok := blob.Exists(f.r.DB(), missing.UUID)
			if !ok {
				return fmt.Errorf("no phantom for the absent reference")
			}
			if !content.IsPrivate(f.r.DB(), int64(prid)) {
				return fmt.Errorf("the phantom of a private reference is public")
			}
			return nil
		}),
		smRepoCase("submanifest.7.1.dephantomize-retries", "SM-7.1-2", smGapRetried),
		smRepoCase("submanifest.7.3.deferred-like-delta", "SM-7.3-1", smMissingIsFatal),
		smRepoCase("submanifest.7.3.no-crosslink-effect", "SM-7.3-2", func(f *smFixture) error {
			refs, err := f.leaves([]string{"a"})
			if err != nil {
				return err
			}
			rid, _ := blob.Exists(f.r.DB(), refs[0].UUID)
			if _, err := manifest.Crosslink(f.r); err != nil {
				return err
			}
			for _, table := range []string{"event", "plink", "mlink", "tagxref"} {
				col := map[string]string{"event": "objid", "plink": "cid", "mlink": "mid", "tagxref": "rid"}[table]
				if n := f.count("SELECT count(*) FROM "+table+" WHERE "+col+"=?", rid); n != 0 {
					return fmt.Errorf("a sub-manifest wrote %d %s rows", n, table)
				}
			}
			return nil
		}),
		smRepoCase("submanifest.7.3.verify-reports-gap", "SM-7.3-2", func(f *smFixture) error {
			absent := deck.SubmanifestCard{Name: "a", UUID: hash.AlgSHA3.Hash([]byte("absent leaf"))}
			if _, _, err := f.checkin(smTime, []deck.SubmanifestCard{absent}); err != nil {
				return err
			}
			report, err := verify.Verify(f.r)
			if err != nil {
				return err
			}
			for _, issue := range report.Issues {
				if issue.Kind == verify.IssueEventMissing || issue.Kind == verify.IssuePlinkMissing {
					return fmt.Errorf("an incomplete tree is reported as damage: %s", issue.Message)
				}
			}
			return want("missing references", report.MissingRefs, 1)
		}),
		smRepoCase("submanifest.8.1.seen-indicator", "SM-8.1-2", func(f *smFixture) error {
			if seen, _ := manifest.SeenSubmanifest(f.r.DB()); seen {
				return fmt.Errorf("indicator set before any tree")
			}
			if _, err := f.leaves([]string{"a"}); err != nil {
				return err
			}
			if _, err := manifest.Crosslink(f.r); err != nil {
				return err
			}
			seen, err := manifest.SeenSubmanifest(f.r.DB())
			return wantNoErr(err, "SeenSubmanifest", want("seen", seen, true))
		}),
		smRepoCase("submanifest.8.1.no-s-in-commits", "SM-8.1-1", func(f *smFixture) error {
			if _, err := f.r.DB().Exec(`REPLACE INTO config(name,value,mtime) VALUES
				('enable-submanifests','on',1),('submanifest-k','1',1)`); err != nil {
				return err
			}
			files := make([]manifest.File, 40)
			for i := range files {
				files[i] = manifest.File{Name: fmt.Sprintf("f%02d.c", i), Content: []byte{byte(i), '\n'}}
			}
			rid, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{
				Files: files, Comment: "c", User: "u", Time: smTime})
			if err != nil {
				return err
			}
			d, err := manifest.GetManifest(f.r, rid)
			if err != nil {
				return err
			}
			return want("S cards", len(d.S), 0)
		}),
		smRepoCase("submanifest.9.2.sweep-marker", "SM-9.2-1", func(f *smFixture) error {
			refs, err := f.leaves([]string{"a"})
			if err != nil {
				return err
			}
			rid, _ := blob.Exists(f.r.DB(), refs[0].UUID)
			if _, err := manifest.Crosslink(f.r); err != nil {
				return err
			}
			if n := f.count("SELECT count(*) FROM crosslink_nonartifact_v2 WHERE rid=?", rid); n != 1 {
				return fmt.Errorf("no completion marker for the sub-manifest")
			}
			linked, err := manifest.Crosslink(f.r)
			return wantNoErr(err, "Crosslink", want("relinked", linked, 0))
		}),
		smRepoCase("submanifest.9.4.rediscovered", "SM-9.4-1", smGapRetried),
	}
}

// smServerTree is a fixture holding a tree, for the transfer cases.
func smServerTree(f *smFixture) (tree, leaf, file string, err error) {
	refs, err := f.leaves([]string{"a"})
	if err != nil {
		return "", "", "", err
	}
	_, tree, err = f.checkin(smTime, refs)
	return tree, refs[0].UUID, mustUUIDOf(f, "a"), err
}

func smGimme(r *repo.Repo, version, uuid string) (*xfer.Message, error) {
	var pc string
	r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc)
	return sync.HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "client-version", Values: []string{version, "20260101", "000000"}},
		&xfer.PullCard{ServerCode: "x", ProjectCode: pc},
		&xfer.GimmeCard{UUID: uuid},
	}})
}

func smCardsOf[T xfer.Card](msg *xfer.Message) []T {
	var out []T
	for _, c := range msg.Cards {
		if v, ok := c.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func smTransferCases() []smCase {
	return []smCase{
		smRepoCase("submanifest.7.2.gate", "SM-7.2-1", func(f *smFixture) error {
			tree, leaf, file, err := smServerTree(f)
			if err != nil {
				return err
			}
			for _, uuid := range []string{tree, leaf} {
				old, err := smGimme(f.r, "22800", uuid)
				if err != nil {
					return err
				}
				if len(smCardsOf[*xfer.FileCard](old)) != 0 {
					return fmt.Errorf("a 2.28 peer was sent %s", uuid)
				}
				if len(smCardsOf[*xfer.ErrorCard](old)) != 1 {
					return fmt.Errorf("no explicit refusal for %s", uuid)
				}
				current, err := smGimme(f.r, "22900", uuid)
				if err != nil {
					return err
				}
				if len(smCardsOf[*xfer.FileCard](current)) != 1 {
					return fmt.Errorf("a 2.29 peer was refused %s", uuid)
				}
			}
			plain, err := smGimme(f.r, "22800", file)
			if err != nil {
				return err
			}
			return want("file cards for ordinary content", len(smCardsOf[*xfer.FileCard](plain)), 1)
		}),
		{Case{ID: "submanifest.7.2.fossil-2.29-not-refused", Req: "SM-7.2-1",
			Deviation: "the threshold is zit's 22900, which fossil 2.29 and later announce " +
				"without reading trees; refusing them needs a non-fossil version (danmestas/zit#239)"},
			func(t *testing.T) error {
				f, err := newSMFixture(t)
				if err != nil {
					return err
				}
				tree, _, _, err := smServerTree(f)
				if err != nil {
					return err
				}
				// The announcement a fossil 2.29 build makes.
				resp, err := smGimme(f.r, "22900", tree)
				if err != nil {
					return err
				}
				if len(smCardsOf[*xfer.FileCard](resp)) != 0 {
					return fmt.Errorf("a fossil 2.29 peer, which reads no tree, is sent one")
				}
				return nil
			}},
		smRepoCase("submanifest.7.2.clone-delta-of-withheld-tree", "SM-7.2-1", smCloneDeltaOfTree),
		smRepoCase("submanifest.7.2.pushed-tree-withheld", "SM-7.2-1", smPushedTreeWithheld),
		smRepoCase("submanifest.7.2.avoid-pragma", "SM-7.2-2", func(f *smFixture) error {
			var pc string
			f.r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc)
			resp, err := sync.HandleSync(context.Background(), f.r, &xfer.Message{Cards: []xfer.Card{
				&xfer.PragmaCard{Name: "client-version", Values: []string{"22900", "20260101", "000000"}},
				&xfer.PullCard{ServerCode: "x", ProjectCode: pc},
				&xfer.PragmaCard{Name: "ci-lock", Values: []string{pc, "c1"}},
			}})
			if err != nil {
				return err
			}
			var names []string
			for _, p := range smCardsOf[*xfer.PragmaCard](resp) {
				names = append(names, p.Name)
			}
			joined := strings.Join(names, " ")
			if !strings.Contains(joined, "avoid-delta-manifests avoid-submanifests") {
				return fmt.Errorf("reply pragmas %q", joined)
			}
			return nil
		}),
		smRepoCase("submanifest.7.2.client-commits-flat", "SM-7.2-3", smCommitOnTree),
		smRepoCase("submanifest.6.3.project-members", "SM-6.3-1", func(f *smFixture) error {
			if _, err := f.r.DB().Exec(`REPLACE INTO config(name,value,mtime) VALUES
				('enable-submanifests','on',5),('submanifest-k','3',6),('submanifest-auto-retune','off',7)`); err != nil {
				return err
			}
			resp, err := sync.HandleSync(context.Background(), f.r, &xfer.Message{Cards: []xfer.Card{
				&xfer.ReqConfigCard{Name: "/project"},
			}})
			if err != nil {
				return err
			}
			var body bytes.Buffer
			for _, c := range smCardsOf[*xfer.ConfigCard](resp) {
				body.Write(c.Content)
				body.WriteByte('\n')
			}
			for _, name := range []string{"enable-submanifests", "submanifest-k", "submanifest-auto-retune"} {
				if !strings.Contains(body.String(), "'"+name+"'") {
					return fmt.Errorf("/project omits %s", name)
				}
			}
			return nil
		}),
		{Case{ID: "submanifest.6.3.staged-atomic", Req: "SM-6.3-2"}, smStagedConfig},
		{Case{ID: "submanifest.6.3.reqconfig-on-pull", Req: "SM-6.3-3",
			Deviation: "follows zit: the request is reqconfig /config, the scalar marker zit's " +
				"server answers (it answers /project with nothing), sent once in the clone's " +
				"cleanup round and never on pull (draft text: danmestas/zit#240)"}, func(t *testing.T) error {
			return fmt.Errorf("go-libfossil sends reqconfig /config in the clone cleanup round only")
		}},
		{Case{ID: "submanifest.6.3.clone-without-group", Req: "SM-6.3-4"}, func(t *testing.T) error {
			return smCloneWithoutGroup(t)
		}},
	}
}

// smOldTransport serves r to a client that announces fossil 2.28.
func smOldTransport(r *repo.Repo) *sync.MockTransport {
	return &sync.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		for _, c := range req.Cards {
			if p, ok := c.(*xfer.PragmaCard); ok && p.Name == "client-version" {
				p.Values[0] = "22800"
			}
		}
		resp, err := sync.HandleSync(context.Background(), r, req)
		if err != nil {
			return &xfer.Message{}
		}
		return resp
	}}
}

// smCloneDeltaOfTree: a flat check-in stored as a delta of a tree manifest
// -- the shape a repository takes when it moves between flat and tree
// check-ins -- reaches a 2.28 client whole, never as a delta of a tree that
// client is refused.
func smCloneDeltaOfTree(f *smFixture) error {
	refs, err := f.leaves([]string{"a"})
	if err != nil {
		return err
	}
	comment := strings.Repeat("shared comment text ", 200)
	_, tree, err := f.link(&deck.Deck{C: comment, D: smTime, S: refs, U: deck.User("u")})
	if err != nil {
		return err
	}
	_, flat, err := f.link(&deck.Deck{C: comment, D: smTime.Add(time.Hour), P: []string{tree},
		F: []deck.FileCard{{Name: "a", UUID: mustUUIDOf(f, "a")}}, U: deck.User("u")})
	if err != nil {
		return err
	}
	treeRID, _ := blob.Exists(f.r.DB(), tree)
	flatRID, _ := blob.Exists(f.r.DB(), flat)
	if err := f.r.WithTx(func(tx *db.Tx) error {
		_, err := content.Deltify(tx, flatRID, treeRID)
		return err
	}); err != nil {
		return err
	}
	if src, _ := content.DeltaSource(f.r.DB(), flatRID); src != treeRID {
		return fmt.Errorf("fixture: the flat check-in is not a delta of the tree")
	}
	dir, err := os.MkdirTemp("", "sm-clone")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	clone, _, err := sync.Clone(context.Background(), filepath.Join(dir, "old.fossil"), smOldTransport(f.r), sync.CloneOpts{})
	if err != nil {
		return err
	}
	defer clone.Close()
	rid, ok := content.AvailableByUUID(clone.DB(), flat)
	if !ok {
		return fmt.Errorf("the flat check-in did not reach the 2.28 client whole")
	}
	if _, err := content.Expand(clone.DB(), rid); err != nil {
		return fmt.Errorf("the flat check-in cannot be rebuilt: %v", err)
	}
	if _, ok := content.AvailableByUUID(clone.DB(), tree); ok {
		return fmt.Errorf("the 2.28 client was sent the tree")
	}
	return nil
}

// smPushedTreeWithheld: a tree pushed to the server is withheld from the
// next 2.28 client that asks, whatever has or has not been crosslinked.
func smPushedTreeWithheld(f *smFixture) error {
	if _, err := f.r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		return err
	}
	var pc string
	f.r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc)
	file := []byte("pushed file\n")
	fileUUID := hash.AlgSHA3.Hash(file)
	leaf, err := (&deck.Deck{F: []deck.FileCard{{Name: "a", UUID: fileUUID}}}).Marshal()
	if err != nil {
		return err
	}
	tree, err := (&deck.Deck{C: "pushed", D: smTime, U: deck.User("u"),
		S: []deck.SubmanifestCard{{Name: "a", UUID: hash.AlgSHA3.Hash(leaf)}}}).Marshal()
	if err != nil {
		return err
	}
	cards := []xfer.Card{
		&xfer.PragmaCard{Name: "client-version", Values: []string{"22900", "20260101", "000000"}},
		&xfer.PushCard{ServerCode: "x", ProjectCode: pc},
	}
	for _, data := range [][]byte{file, leaf, tree} {
		cards = append(cards, &xfer.FileCard{UUID: hash.AlgSHA3.Hash(data), Content: data})
	}
	if _, err := sync.HandleSync(context.Background(), f.r, &xfer.Message{Cards: cards}); err != nil {
		return err
	}
	if _, ok := content.AvailableByUUID(f.r.DB(), hash.AlgSHA3.Hash(tree)); !ok {
		return fmt.Errorf("fixture: the push stored no tree")
	}
	resp, err := smGimme(f.r, "22800", hash.AlgSHA3.Hash(tree))
	if err != nil {
		return err
	}
	if len(smCardsOf[*xfer.FileCard](resp)) != 0 {
		return fmt.Errorf("a pushed tree went to a 2.28 client")
	}
	return nil
}

// smCloneWithoutGroup clones a repository whose server sends no sub-manifest
// settings: the clone leaves enable-submanifests unset, which reads as off.
func smCloneWithoutGroup(t *testing.T) error {
	src, err := newSMFixture(t)
	if err != nil {
		return err
	}
	if _, err := src.leaves([]string{"a"}); err != nil {
		return err
	}
	transport := &sync.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := sync.HandleSync(context.Background(), src.r, req)
		if err != nil {
			return &xfer.Message{}
		}
		return resp
	}}
	clone, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"), transport, sync.CloneOpts{})
	if err != nil {
		return err
	}
	defer clone.Close()
	var n int
	clone.DB().QueryRow("SELECT count(*) FROM config WHERE name='enable-submanifests'").Scan(&n)
	return want("enable-submanifests rows", n, 0)
}

// smCloneWithConfig clones a small repository through a transport that
// answers the clone's configuration request with rows, one config card each,
// in the order given, and returns the three settings the clone holds.
func smCloneWithConfig(t *testing.T, rows []string) (map[string]string, error) {
	src, err := newSMFixture(t)
	if err != nil {
		return nil, err
	}
	if _, err := src.file("a"); err != nil {
		return nil, err
	}
	transport := &sync.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := sync.HandleSync(context.Background(), src.r, req)
		if err != nil {
			return &xfer.Message{}
		}
		if len(smCardsOf[*xfer.ReqConfigCard](req)) > 0 {
			for _, row := range rows {
				resp.Cards = append(resp.Cards, &xfer.ConfigCard{Name: "/config", Content: []byte(row)})
			}
		}
		return resp
	}}
	clone, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"), transport, sync.CloneOpts{})
	if err != nil {
		return nil, err
	}
	defer clone.Close()
	got := map[string]string{}
	for _, name := range []string{"enable-submanifests", "submanifest-k", "submanifest-auto-retune"} {
		var v string
		if clone.DB().QueryRow("SELECT value FROM config WHERE name=?", name).Scan(&v) == nil {
			got[name] = v
		}
	}
	return got, nil
}

// smStagedConfig: the three settings arrive whole in any card order, and an
// invalid value or an enable without a valid k leaves none applied.
func smStagedConfig(t *testing.T) error {
	rows := []string{
		"10 'enable-submanifests' value 'on'",
		"11 'submanifest-k' value '3'",
		"12 'submanifest-auto-retune' value 'off'",
	}
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}, {1, 2, 0}} {
		var ordered []string
		for _, i := range order {
			ordered = append(ordered, rows[i])
		}
		got, err := smCloneWithConfig(t, ordered)
		if err != nil {
			return err
		}
		if got["enable-submanifests"] != "on" || got["submanifest-k"] != "3" || got["submanifest-auto-retune"] != "off" {
			return fmt.Errorf("order %v applied %v", order, got)
		}
	}
	for _, bad := range [][]string{
		{rows[0], "11 'submanifest-k' value '13'", rows[2]},
		{rows[0], rows[2]},
		{rows[1], "12 'submanifest-auto-retune' value 'maybe'", rows[0]},
	} {
		got, err := smCloneWithConfig(t, bad)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			return fmt.Errorf("rejected set %q applied %v", bad, got)
		}
	}
	return nil
}

// smVector reads one of zit's Appendix A vector files, dropping the leading
// "#" lines that keep the file inert on disk.
func smVector(name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join("testdata", "submanifest", "submanifest_"+name+".txt"))
	if err != nil {
		return nil, err
	}
	for bytes.HasPrefix(data, []byte("#")) {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil, fmt.Errorf("%s: comment without a newline", name)
		}
		data = data[i+1:]
	}
	return data, nil
}

// smVectorSet is one Appendix A check-in: its manifest, its sub-manifests,
// every file's content, and the artifact names the vector records.
type smVectorSet struct {
	manifest string
	subs     []string
	files    map[string]string
	names    map[string]string // vector name to its recorded artifact name
	want     string            // the walked enumeration
	baseline *smVectorSet      // for a delta: the tree it names
}

var smA1 = map[string]string{
	"README.md": "# Example\n", "src/main.c": "int main(void){return 0;}\n",
	"src/node11.c": "/* node11 */\n", "src/util.c": "/* util */\n",
}

func smA4Files(names ...string) map[string]string {
	files := map[string]string{}
	for _, name := range names {
		files[name] = "/* " + strings.TrimSuffix(strings.TrimPrefix(name, "f"), ".c") + " */\n"
	}
	return files
}

var smA4Names = []string{"f1257.c", "f1346.c", "f1523.c", "f2146.c", "f2260.c", "f2468.c", "f2614.c", "f2954.c", "f3514.c"}

func smVectorSets() map[string]*smVectorSet {
	a3 := &smVectorSet{
		manifest: "a3_tree_manifest", subs: []string{"a2_1_leaf", "a2_2_leaf"}, files: smA1,
		names: map[string]string{
			"a2_1_leaf":        "aa9d404c14fe4dadd2ab398961fb0dfe650a877c061f650d8233a5888e920acf",
			"a2_2_leaf":        "a50a722b465ed67b42ee0fbe61e4a19d6b1d22940ba5740e99e450921e3f52ea",
			"a3_tree_manifest": "95fba39cbaba76fccb15571516659cea6a0e8a60123c3e7bda1397649d87c509",
		},
		want: "README.md src/main.c src/node11.c src/util.c",
	}
	a4 := &smVectorSet{
		manifest: "a4_tree_manifest", subs: []string{"a4_1_leaf", "a4_2_leaf", "a4_3_leaf"},
		files: smA4Files(smA4Names...),
		names: map[string]string{
			"a4_1_leaf":        "b8e53d0c46cb57b616cc0ed6de5ce2453715be6157b73d247bcd5de8a6510aaa",
			"a4_2_leaf":        "cff23e5ba71569397c1d816afa2dd0bfffc42339e402df36b63623846bd5fcbe",
			"a4_3_leaf":        "9604c3f9774f7472d7bff44d5965f30250358a3e7a18046eeaf5f9d83bd5cdc6",
			"a4_tree_manifest": "02dad8d4451ecf930c0c8637cf66351579aa746aebf0cf10f249b2a6dea37caa",
		},
		want: strings.Join(smA4Names, " "),
	}
	enc := map[string]string{}
	for _, name := range []string{"v00 w.c", "v00\\w.c", "v01 w.c", "v01\\w.c", "v02 w.c",
		"v02\\w.c", "v03 w.c", "v03\\w.c", "v04 w.c"} {
		enc[name] = name + "\n"
	}
	encNames := make([]string, 0, len(enc))
	for name := range enc {
		encNames = append(encNames, name)
	}
	sort.Strings(encNames)
	deltaFiles := map[string]string{}
	for k, v := range smA1 {
		deltaFiles[k] = v
	}
	deltaFiles["src/main.c"] = "int main(void){return 1;}\n"
	return map[string]*smVectorSet{
		"a3": a3,
		"a4": a4,
		"three-level": {
			manifest: "a5_three_level_manifest",
			subs:     []string{"a5_three_level_interior_1", "a5_three_level_interior_2", "a4_1_leaf", "a4_2_leaf", "a4_3_leaf"},
			files:    smA4Files(smA4Names...),
			names: map[string]string{
				"a5_three_level_interior_1": "d42946de7b59647235f2166d964e9e5878bb1b3aba796b5e9c5e9dc719784e23",
				"a5_three_level_interior_2": "c1cc48fa9a3fbc34da7614632137890b4b09b33e750abc75514836c533e5f42e",
				"a5_three_level_manifest":   "c136ef62f5f13c18d06564649da6022d9fe25cde5be15dd1ad10b04015096424",
			},
			want: strings.Join(smA4Names, " "),
		},
		"merge": {
			manifest: "a5_merge_manifest", subs: []string{"a5_merge_leaf_1", "a5_merge_leaf_2"},
			files: smA4Files("f0004.c", "f0005.c", "f0006.c", "f0007.c", "f0016.c"),
			names: map[string]string{
				"a5_merge_leaf_1":   "4bf4476e3aa41a1e3d26c85173c18fcd19f61871d4fc31215e523d7502078cc8",
				"a5_merge_leaf_2":   "6f3c993caa208b6fd44332d241676f9aa6a8c29dc7e28f287d5cdc088005a842",
				"a5_merge_manifest": "cef55b9b325fbc8abd59a044acafa57603c1b11a605e7fbba02d511a6da7a806",
			},
			want: "f0004.c f0005.c f0006.c f0007.c f0016.c",
		},
		"encoding": {
			manifest: "a5_encoding_manifest",
			subs:     []string{"a5_encoding_leaf_1", "a5_encoding_leaf_2", "a5_encoding_leaf_3"},
			files:    enc,
			names: map[string]string{
				"a5_encoding_leaf_1":   "e2899273b6a258ee18e053a79903d02469a275e2c80279c3657c7b803e2af6c0",
				"a5_encoding_leaf_2":   "d7edb65681faaa4fd4f3541f04370421b057bf299098b781c7ec50d609869082",
				"a5_encoding_leaf_3":   "c634332663f1cc0298aae6b61c799214a522877102cd6acdfe633f29fd79eeaf",
				"a5_encoding_manifest": "deb0cb8b56fddfc4eebbab0f928cbdb19e64a6ba568064d5b43663dc1ef7263f",
			},
			want: strings.Join(encNames, " "),
		},
		"delta": {
			manifest: "a5_delta_manifest", files: deltaFiles, baseline: a3,
			names: map[string]string{
				"a5_delta_manifest": "c74e37f650677523f7b6c2b4f094f766c4427a8ea180768ad75fa2de6bd14902",
			},
			want: "README.md src/main.c src/node11.c",
		},
	}
}

// smCheckVectors stores a vector set, checks every recorded name, and reads
// the manifest back: type, walked enumeration and R card.
func smCheckVectors(f *smFixture, set *smVectorSet) error {
	if set.baseline != nil {
		if err := smCheckVectors(f, set.baseline); err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
	}
	for name, data := range set.files {
		if _, err := f.store([]byte(data)); err != nil {
			return fmt.Errorf("file %s: %w", name, err)
		}
	}
	for _, name := range append(append([]string(nil), set.subs...), set.manifest) {
		data, err := smVector(name)
		if err != nil {
			return err
		}
		d, err := deck.Parse(data)
		if err != nil {
			return fmt.Errorf("%s rejected: %v", name, err)
		}
		wantType := deck.Submanifest
		if name == set.manifest {
			wantType = deck.Checkin
		}
		if d.Type != wantType {
			return fmt.Errorf("%s: type %d, want %d", name, d.Type, wantType)
		}
		uuid, err := f.store(data)
		if err != nil {
			return err
		}
		if recorded, ok := set.names[name]; ok && uuid != recorded {
			return fmt.Errorf("%s is named %s, the vector records %s", name, uuid, recorded)
		}
	}
	if _, err := manifest.Crosslink(f.r); err != nil {
		return err
	}
	rid, ok := blob.Exists(f.r.DB(), set.names[set.manifest])
	if !ok {
		return fmt.Errorf("%s not stored", set.manifest)
	}
	got, err := f.names(rid)
	if err := wantNoErr(err, "ListFiles", want("enumeration", got, set.want)); err != nil {
		return err
	}
	return f.r.WithTx(func(tx *db.Tx) error { return manifest.VerifyRCard(tx, nil, rid) })
}

func smVectorCases() []smCase {
	sets := smVectorSets()
	vector := func(id, req, key string) smCase {
		return smRepoCase(id, req, func(f *smFixture) error { return smCheckVectors(f, sets[key]) })
	}
	return []smCase{
		vector("submanifest.a.2-3", "SM-A-1", "a3"),
		vector("submanifest.a.4", "SM-A-2", "a4"),
		vector("submanifest.a.5.three-level", "SM-A-3", "three-level"),
		vector("submanifest.a.5.merge", "SM-A-3", "merge"),
		vector("submanifest.a.5.encoding", "SM-A-3", "encoding"),
		vector("submanifest.a.5.delta-over-tree", "SM-A-3", "delta"),
		smRepoCase("submanifest.a.5.lookup", "SM-5.3-1", func(f *smFixture) error {
			if err := smCheckVectors(f, sets["a4"]); err != nil {
				return err
			}
			rid, _ := blob.Exists(f.r.DB(), sets["a4"].names["a4_tree_manifest"])
			for name, present := range map[string]bool{
				"f1257.c": true, "f2146.c": true, "f3514.c": true,
				"f2000.c": false, "f0000.c": false,
			} {
				_, found, err := manifest.LookupFile(f.r, rid, name)
				if err != nil || found != present {
					return fmt.Errorf("LookupFile(%q) = %v, %v", name, found, err)
				}
			}
			return nil
		}),
	}
}
