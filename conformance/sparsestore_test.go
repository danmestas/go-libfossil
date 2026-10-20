package conformance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The sparse store of draft-fossil-sparse-cone-00 §5, Appendix A step 3:
// elided-phantom classification and its verification semantics. Nothing in
// this step fills a store sparsely -- cone-scoped transfer is step 4 -- so a
// case makes a dense clone of the suite's repository and then elides what a
// cone-scoped clone would not have kept (scSparsify), by zit's rule: the
// blobs F cards name only outside the cone.

func scStoreCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.3.1.store-loss", Req: "SC-3.1-4"}, scStoreLoss},
		{Case{ID: "sparsecone.4.3.sparse-omits", Req: "SC-4.3-2"}, scSparseOmitsR},
		{Case{ID: "sparsecone.4.3.sparse-skips", Req: "SC-4.3-5"}, scSparseSkipsR},
		{Case{ID: "sparsecone.5.flat-dense", Req: "SC-5-1"}, scHybrid},
		{Case{ID: "sparsecone.5.1.holdings", Req: "SC-5.1-1"}, scHoldings},
		{Case{ID: "sparsecone.5.1.nodes", Req: "SC-5.1-1",
			Deviation: "out-of-cone sub-manifest nodes are held, not elided, as zit holds them: " +
				"a check-in whose tree cannot be enumerated is never linked " +
				"(draft-fossil-submanifests-00 §5.2, §9.4), so an elided node would cost the " +
				"check-in its event, plink and mlink rows; a node phantom is a transit phantom"},
			scNodes},
		{Case{ID: "sparsecone.5.2.classes", Req: "SC-5.2-1"}, scClasses},
		{Case{ID: "sparsecone.5.2.server", Req: "SC-5.2-1"}, scServerClasses},
		{Case{ID: "sparsecone.5.2.recompute", Req: "SC-5.2-2"}, scRecompute},
		{Case{ID: "sparsecone.5.2.fossil-rebuild", Req: "SC-5.2-2"}, scFossilRebuild},
		{Case{ID: "sparsecone.5.2.verify", Req: "SC-5.2-3"}, scVerifySparse},
		{Case{ID: "sparsecone.5.3.push", Req: "SC-5.3-1"}, scSparsePush},
		{Case{ID: "sparsecone.5.4.widen", Req: "SC-5.4-1"}, scWidenStore},
		{Case{ID: "sparsecone.5.5.hybrid", Req: "SC-5.5-1"}, scHybrid},
		{Case{ID: "sparsecone.6.2.delta-base", Req: "SC-6.2-3"}, scDeltaBase},
	}
}

// scSparsify makes r a store sparse at c: the store cone set, and every held
// blob that F cards name only outside c turned into a phantom, as a
// cone-scoped clone would leave it (§5.1, §6.3). A held delta over such a
// blob is first expanded in place, as §6.2 has a server send it in full. It
// returns the names it elided.
func scSparsify(t *testing.T, r *repo.Repo, c *cone.Cone) map[string]bool {
	t.Helper()
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	rows, err := r.DB().Query(`
		SELECT b.rid, b.uuid, f.name FROM blob b
		  JOIN mlink m ON m.fid=b.rid JOIN filename f ON f.fnid=m.fnid
		 WHERE b.size>=0
		UNION
		SELECT b.rid, b.uuid, f.name FROM blob b
		  JOIN mlink m ON m.pid=b.rid
		  JOIN filename f ON f.fnid=CASE WHEN m.pfnid>0 THEN m.pfnid ELSE m.fnid END
		 WHERE b.size>=0`)
	if err != nil {
		t.Fatal(err)
	}
	in, out := map[libfossil.FslID]bool{}, map[libfossil.FslID]string{}
	for rows.Next() {
		var rid libfossil.FslID
		var uuid, name string
		if err := rows.Scan(&rid, &uuid, &name); err != nil {
			t.Fatal(err)
		}
		if c.Covers(name) {
			in[rid] = true
		} else {
			out[rid] = uuid
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	elided := map[string]bool{}
	err = r.WithTx(func(tx *db.Tx) error {
		for rid := range out {
			if in[rid] {
				delete(out, rid)
			}
		}
		for rid := range out {
			children, err := scDeltaChildren(tx, rid)
			if err != nil {
				return err
			}
			for _, child := range children {
				if _, gone := out[child]; gone {
					continue
				}
				if err := content.Undelta(tx, child); err != nil {
					return err
				}
			}
		}
		for rid, uuid := range out {
			for _, stmt := range []string{
				"DELETE FROM delta WHERE rid=?",
				"UPDATE blob SET size=-1, content=NULL WHERE rid=?",
				"INSERT OR IGNORE INTO phantom(rid) VALUES(?)",
			} {
				if _, err := tx.Exec(stmt, rid); err != nil {
					return err
				}
			}
			elided[uuid] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return elided
}

func scDeltaChildren(q db.Querier, rid libfossil.FslID) ([]libfossil.FslID, error) {
	rows, err := q.Query("SELECT rid FROM delta WHERE srcid=?", rid)
	if err != nil {
		return nil, err
	}
	var out []libfossil.FslID
	for rows.Next() {
		var child libfossil.FslID
		if err := rows.Scan(&child); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, child)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// scPhantomize turns the held artifact uuid into a transit-shaped phantom,
// as an interrupted transfer would leave it.
func scPhantomize(t *testing.T, r *repo.Repo, uuid string) libfossil.FslID {
	t.Helper()
	rid, ok := content.AvailableByUUID(r.DB(), uuid)
	if !ok {
		t.Fatalf("scPhantomize: %s is not held", uuid)
	}
	err := r.WithTx(func(tx *db.Tx) error {
		children, err := scDeltaChildren(tx, rid)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := content.Undelta(tx, child); err != nil {
				return err
			}
		}
		for _, stmt := range []string{
			"DELETE FROM delta WHERE rid=?",
			"UPDATE blob SET size=-1, content=NULL WHERE rid=?",
			"INSERT OR IGNORE INTO phantom(rid) VALUES(?)",
		} {
			if _, err := tx.Exec(stmt, rid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

// scSparse is the suite's repository served densely, and a clone of it made
// sparse at scDefault.
type scSparse struct {
	origin *scRepo
	clone  *repo.Repo
	elided map[string]bool
	// gimmes records every gimme card the clone sent the origin.
	gimmes map[string]bool
}

func newSCSparse(t *testing.T, tree bool) *scSparse {
	t.Helper()
	return newSCSparseFrom(t, newSCRepo(t, tree))
}

// newSCSparseFrom serves origin, which nobody may push to, and makes the
// sparse clone of it.
func newSCSparseFrom(t *testing.T, origin *scRepo) *scSparse {
	t.Helper()
	s := &scSparse{origin: origin, gimmes: map[string]bool{}}
	if _, err := origin.r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	clone, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "sparse.fossil"),
		s.transport(origin.r), sync.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clone.Close() })
	s.clone = clone
	s.elided = scSparsify(t, clone, scDefault(t))
	if len(s.elided) == 0 {
		t.Fatal("scSparsify elided nothing")
	}
	return s
}

// transport serves server in process and records the gimme cards each
// request carries.
func (s *scSparse) transport(server *repo.Repo) sync.Transport {
	return &sync.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		for _, card := range req.Cards {
			if g, ok := card.(*xfer.GimmeCard); ok {
				s.gimmes[g.UUID] = true
			}
		}
		resp, err := sync.HandleSync(context.Background(), server, req)
		if err != nil {
			return &xfer.Message{}
		}
		return resp
	}}
}

// sync runs a session from client to server.
func (s *scSparse) sync(t *testing.T, client, server *repo.Repo, push, pull bool) error {
	t.Helper()
	code, err := client.Config("project-code")
	if err != nil {
		return err
	}
	_, err = sync.Sync(context.Background(), client, s.transport(server),
		sync.SyncOpts{Push: push, Pull: pull, ProjectCode: code})
	return err
}

// rid is the clone's rid for the origin's artifact rid.
func (s *scSparse) rid(t *testing.T, originRID libfossil.FslID) libfossil.FslID {
	t.Helper()
	var uuid string
	if err := s.origin.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", originRID).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	var rid libfossil.FslID
	if err := s.clone.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return rid
}

// scAbsences classifies r's phantoms, by uuid.
func scAbsences(t *testing.T, r *repo.Repo) map[string]content.Absence {
	t.Helper()
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.DB().Query("SELECT b.rid, b.uuid FROM phantom p JOIN blob b ON b.rid=p.rid")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]content.Absence{}
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			t.Fatal(err)
		}
		out[uuid] = a.Class(rid)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return out
}

// scClassed lists the uuids in class k, sorted.
func scClassed(classes map[string]content.Absence, k content.Absence) []string {
	var out []string
	for uuid, got := range classes {
		if got == k {
			out = append(out, uuid)
		}
	}
	sort.Strings(out)
	return out
}

func scSorted(set map[string]bool) []string {
	return slices.Sorted(maps.Keys(set))
}

// scUUIDOf is the uuid of name in check-in rid of r.
func scUUIDOf(t *testing.T, r *repo.Repo, rid libfossil.FslID, name string) string {
	t.Helper()
	e, ok, err := manifest.LookupFile(r, rid, name)
	if err != nil || !ok {
		t.Fatalf("LookupFile %s: %v, %v", name, ok, err)
	}
	return e.UUID
}

// scVerify runs verify and requires it to find no issue.
func scVerify(r *repo.Repo) (*verify.Report, error) {
	report, err := verify.Verify(r)
	if err != nil {
		return nil, err
	}
	if !report.OK() {
		return report, expectf("verify reports issues: %+v", report.Issues)
	}
	return report, nil
}

// --- §3.1, §5.2: the store cone, and losing it ------------------------------

// A store cone this implementation cannot read is refused, which is policy
// loss: every elided phantom becomes a transit phantom, and the next pull
// fetches them all.
func scStoreLoss(t *testing.T) error {
	s := newSCSparse(t, false)
	later := []byte{0, 9, 1, 0, 3, 0, 's', 'r', 'c'} // a later version's lead-in
	if _, err := s.clone.DB().Exec("UPDATE storecone SET cone=?", later); err != nil {
		return err
	}
	if c, err := content.StoreCone(s.clone.DB()); err != nil || c != nil {
		return expectf("a cone of an unknown version reads as %v, %v; want dense", c, err)
	}
	classes := scAbsences(t, s.clone)
	if err := scSameList("transit after the loss", scClassed(classes, content.Transit), scSorted(s.elided)); err != nil {
		return err
	}
	if err := s.sync(t, s.clone, s.origin.r, false, true); err != nil {
		return err
	}
	report, err := scVerify(s.clone)
	if err != nil {
		return err
	}
	if report.Phantoms != 0 || report.Elided != 0 {
		return expectf("after a pull the store lacks %d transit and %d elided artifacts; want it dense",
			report.Phantoms, report.Elided)
	}
	return nil
}

// --- §4.3: the R card over a sparse store -----------------------------------

// commit commits scEdit from a checkout of the sparse clone whose
// worktree cone is the store cone.
func (s *scSparse) commit(t *testing.T) (libfossil.FslID, error) {
	t.Helper()
	co, err := checkout.Create(s.clone, t.TempDir(), checkout.CreateOpts{Cone: scDefault(t)})
	if err != nil {
		return 0, err
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(s.rid(t, s.origin.c1), checkout.ExtractOpts{Force: true}); err != nil {
		return 0, fmt.Errorf("extract over a sparse store: %w", err)
	}
	rid, _, err := scEdit(t, co, s.origin.when.Add(time.Hour))
	if err != nil {
		return 0, fmt.Errorf("commit over a sparse store: %w", err)
	}
	return rid, nil
}

// A commit over a sparse store carries no R card, and names the out-of-cone
// files it cannot read by the hashes its parent gave them.
func scSparseOmitsR(t *testing.T) error {
	for _, tree := range []bool{false, true} {
		s := newSCSparse(t, tree)
		rid, err := s.commit(t)
		if err != nil {
			return err
		}
		d, err := manifest.GetManifest(s.clone, rid)
		if err != nil {
			return err
		}
		if d.R != "" {
			return expectf("tree=%v: the commit over a sparse store carries R %s", tree, d.R)
		}
		files, err := manifest.ListFiles(s.clone, rid)
		if err != nil {
			return err
		}
		var names []string
		for _, e := range files {
			names = append(names, e.Name)
			if !scDefault(t).Covers(e.Name) && e.UUID != scUUIDOf(t, s.clone, s.rid(t, s.origin.c1), e.Name) {
				return expectf("tree=%v: out-of-cone %s changed hash", tree, e.Name)
			}
		}
		sort.Strings(names)
		if err := scSameList(fmt.Sprintf("tree=%v: the sparse commit's files", tree), names, keysOf(scEdited())); err != nil {
			return err
		}
		if _, err := scVerify(s.clone); err != nil {
			return err
		}
	}
	return nil
}

// A present R card over a sparse store is not checked: the aggregate needs
// content the store deliberately lacks, so the card is unverifiable, not
// wrong.
func scSparseSkipsR(t *testing.T) error {
	s := newSCSparse(t, false)
	c1 := s.rid(t, s.origin.c1)
	d, err := manifest.GetManifest(s.clone, c1)
	if err != nil {
		return err
	}
	if d.R == "" {
		return expectf("the dense commit has no R card to skip")
	}
	// The aggregate really is uncomputable here.
	_, err = d.ComputeR(func(uuid string) ([]byte, error) {
		rid, ok := content.AvailableByUUID(s.clone.DB(), uuid)
		if !ok {
			return nil, fmt.Errorf("%s elided", uuid)
		}
		return content.Expand(s.clone.DB(), rid)
	})
	if err == nil {
		return expectf("the R aggregate computed over a sparse store")
	}
	if err := s.clone.WithTx(func(tx *db.Tx) error { return manifest.VerifyRCard(tx, nil, c1) }); err != nil {
		return expectf("R verification over a sparse store reported %v; want it skipped", err)
	}
	return nil
}

// --- §5, §5.1, §5.5: holdings ------------------------------------------------

// scHybrid: a flat check-in, then a tree one. Both manifests and every
// sub-manifest node are held; each segment's out-of-cone blobs are elided;
// and a phantom manifest, flat or not, is never elided.
func scHybrid(t *testing.T) error {
	origin := newSCRepo(t, false)
	for k, v := range map[string]string{
		manifest.SettingEnableSubmanifests: "on", manifest.SettingSubmanifestK: "1",
	} {
		if err := origin.r.SetConfig(k, v); err != nil {
			return err
		}
	}
	c2 := origin.commit(t, scEditedMap(map[string]string{"tools/t.c": "tools2\n", "src/a.c": "a2\n"}), origin.c1)
	s := newSCSparseFrom(t, origin)
	clone, elided := s.clone, s.elided
	flat, err := manifest.GetManifest(origin.r, origin.c1)
	if err != nil {
		return err
	}
	tree, err := manifest.GetManifest(origin.r, c2)
	if err != nil {
		return err
	}
	if len(flat.S) != 0 || len(tree.S) == 0 {
		return expectf("the fixture is not hybrid: %d and %d S cards", len(flat.S), len(tree.S))
	}
	held := []libfossil.FslID{s.rid(t, origin.c1), s.rid(t, c2)}
	for _, sc := range tree.S {
		rid, ok := content.AvailableByUUID(clone.DB(), sc.UUID)
		if !ok {
			return expectf("sub-manifest node %s is not held", sc.UUID)
		}
		held = append(held, rid)
	}
	for _, rid := range held {
		if _, err := content.Expand(clone.DB(), rid); err != nil {
			return expectf("structural artifact %d is not held: %v", rid, err)
		}
	}
	// Each segment's out-of-cone content is elided: tools/t.c in both.
	for _, rid := range []libfossil.FslID{origin.c1, c2} {
		uuid := scUUIDOf(t, origin.r, rid, "tools/t.c")
		if !elided[uuid] {
			return expectf("check-in %d's tools/t.c (%s) is not elided", rid, uuid)
		}
	}
	classes := scAbsences(t, clone)
	if err := scSameList("elided", scClassed(classes, content.Elided), scSorted(elided)); err != nil {
		return err
	}
	// A flat manifest the store lacks is wanted, never elided: no F card
	// names it.
	var flatUUID string
	origin.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", origin.c1).Scan(&flatUUID)
	scPhantomize(t, clone, flatUUID)
	if got := scAbsences(t, clone)[flatUUID]; got != content.Transit {
		return expectf("a phantom flat manifest is %v, want transit", got)
	}
	return nil
}

// Every manifest, every sub-manifest node, every blob an in-cone F card
// names and every non-file artifact is held; exactly the blobs F cards name
// only outside the cone are elided. A missing control artifact is wanted.
func scHoldings(t *testing.T) error {
	origin := newSCRepo(t, false)
	if _, err := tag.AddTag(origin.r, tag.TagOpts{TargetRID: origin.c1, TagName: "release",
		TagType: tag.TagSingleton, User: "u", Time: origin.when.Add(time.Minute)}); err != nil {
		return err
	}
	s := newSCSparseFrom(t, origin)
	files, err := manifest.ListFiles(s.origin.r, s.origin.c1)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, e := range files {
		if !scDefault(t).Covers(e.Name) {
			want[e.UUID] = true
		} else if _, ok := content.AvailableByUUID(s.clone.DB(), e.UUID); !ok {
			return expectf("in-cone %s is not held", e.Name)
		}
	}
	if err := scSameList("elided", scSorted(s.elided), scSorted(want)); err != nil {
		return err
	}
	// Everything not a file's content is held, the control artifact
	// included.
	var control string
	if err := s.clone.DB().QueryRow(
		"SELECT b.uuid FROM tagxref x JOIN tag USING(tagid) JOIN blob b ON b.rid=x.srcid WHERE tagname='sym-release' OR tagname='release'",
	).Scan(&control); err != nil {
		return fmt.Errorf("control artifact: %w", err)
	}
	for _, uuid := range []string{control} {
		if _, ok := content.AvailableByUUID(s.clone.DB(), uuid); !ok {
			return expectf("structural artifact %s is not held", uuid)
		}
	}
	scPhantomize(t, s.clone, control)
	if got := scAbsences(t, s.clone)[control]; got != content.Transit {
		return expectf("a phantom control artifact is %v, want transit", got)
	}
	if err := s.sync(t, s.clone, s.origin.r, false, true); err != nil {
		return err
	}
	if !s.gimmes[control] {
		return expectf("the pull did not ask for the missing control artifact")
	}
	if _, ok := content.AvailableByUUID(s.clone.DB(), control); !ok {
		return expectf("the pull did not restore the control artifact")
	}
	return nil
}

// An out-of-cone sub-manifest node the store lacks is a transit phantom,
// asked for by the next pull.
func scNodes(t *testing.T) error {
	s := newSCSparse(t, true)
	d, err := manifest.GetManifest(s.origin.r, s.origin.c1)
	if err != nil {
		return err
	}
	var node string
	for i, sc := range d.S {
		limit := ""
		if i+1 < len(d.S) {
			limit = d.S[i+1].Name
		}
		if !scDefault(t).MayCover(sc.Name, limit) {
			node = sc.UUID
			break
		}
	}
	if node == "" {
		return expectf("the tree has no out-of-cone node: %+v", d.S)
	}
	scPhantomize(t, s.clone, node)
	if got := scAbsences(t, s.clone)[node]; got != content.Elided {
		return expectf("an out-of-cone sub-manifest node the store lacks is %v, want elided", got)
	}
	return nil
}

// --- §5.2: the three absence classes ----------------------------------------

// The three classes are told apart, and a pull asks only for the transit
// phantom: neither the elided ones nor the shunned one.
func scClasses(t *testing.T) error {
	s := newSCSparse(t, false)
	c1 := s.rid(t, s.origin.c1)
	transit := scUUIDOf(t, s.clone, c1, "src/a.c")
	scPhantomize(t, s.clone, transit)
	shunned := scUUIDOf(t, s.clone, c1, "tools/t.c")
	if _, err := s.clone.DB().Exec("INSERT INTO shun(uuid, mtime) VALUES(?, 0)", shunned); err != nil {
		return err
	}
	elided := maps.Clone(s.elided)
	delete(elided, shunned)

	classes := scAbsences(t, s.clone)
	for k, want := range map[content.Absence][]string{
		content.Transit: {transit}, content.Shunned: {shunned}, content.Elided: scSorted(elided),
	} {
		if err := scSameList(k.String(), scClassed(classes, k), want); err != nil {
			return err
		}
	}
	if err := s.sync(t, s.clone, s.origin.r, false, true); err != nil {
		return err
	}
	if !s.gimmes[transit] {
		return expectf("the pull did not ask for the transit phantom")
	}
	for uuid := range s.elided {
		if s.gimmes[uuid] {
			return expectf("the pull asked for %s, which is elided or shunned", uuid)
		}
	}
	after := scAbsences(t, s.clone)
	if _, still := after[transit]; still {
		return expectf("the transit phantom was not filled")
	}
	if err := scSameList("elided after the pull", scClassed(after, content.Elided), scSorted(elided)); err != nil {
		return err
	}
	_, err := scVerify(s.clone)
	return err
}

// A sparse server asks a pushing client for its transit phantoms and never
// for its elided ones.
func scServerClasses(t *testing.T) error {
	s := newSCSparse(t, false)
	if _, err := s.clone.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		return err
	}
	transit := scUUIDOf(t, s.clone, s.rid(t, s.origin.c1), "src/a.c")
	scPhantomize(t, s.clone, transit)
	// The dense origin pushes to the sparse clone; record what the clone,
	// as server, asks for.
	asked := map[string]bool{}
	transport := &sync.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := sync.HandleSync(context.Background(), s.clone, req)
		if err != nil {
			return &xfer.Message{}
		}
		for _, card := range resp.Cards {
			if g, ok := card.(*xfer.GimmeCard); ok {
				asked[g.UUID] = true
			}
		}
		return resp
	}}
	code, err := s.origin.r.Config("project-code")
	if err != nil {
		return err
	}
	if _, err := sync.Sync(context.Background(), s.origin.r, transport,
		sync.SyncOpts{Push: true, ProjectCode: code}); err != nil {
		return err
	}
	if !asked[transit] {
		return expectf("the sparse server did not ask for its transit phantom")
	}
	for uuid := range s.elided {
		if asked[uuid] {
			return expectf("the sparse server asked for elided %s", uuid)
		}
	}
	return nil
}

// Elision is recomputed from the cone: changing the cone reclassifies
// without touching a phantom row, and losing it makes every elided phantom a
// transit one.
func scRecompute(t *testing.T) error {
	s := newSCSparse(t, false)
	before := scTableDump(t, s.clone)
	tools := map[string]bool{}
	c1 := s.rid(t, s.origin.c1)
	for _, name := range []string{"tools/t.c", "tools/x/y/z.txt"} {
		tools[scUUIDOf(t, s.clone, c1, name)] = true
	}
	wide := scCone(t, []string{"src", "src/vendor/keep", "tools"}, []string{"src/vendor"})
	if err := content.SetStoreCone(s.clone.DB(), wide); err != nil {
		return err
	}
	if err := scSameList("transit after widening", scClassed(scAbsences(t, s.clone), content.Transit), scSorted(tools)); err != nil {
		return err
	}
	if err := content.SetStoreCone(s.clone.DB(), scDefault(t)); err != nil {
		return err
	}
	if err := scSameList("elided after narrowing back", scClassed(scAbsences(t, s.clone), content.Elided), scSorted(s.elided)); err != nil {
		return err
	}
	if err := content.SetStoreCone(s.clone.DB(), nil); err != nil {
		return err
	}
	if err := scSameList("transit without a cone", scClassed(scAbsences(t, s.clone), content.Transit), scSorted(s.elided)); err != nil {
		return err
	}
	if after := scTableDump(t, s.clone); after != before {
		return expectf("reclassification changed the phantom records:\n%s\nwas\n%s", after, before)
	}
	return nil
}

// scTableDump renders the blob and phantom rows, which elision must never
// touch.
func scTableDump(t *testing.T, r *repo.Repo) string {
	t.Helper()
	var out string
	if err := r.DB().QueryRow(`SELECT
		(SELECT group_concat(rid || ':' || size || ':' || uuid, ',') FROM (SELECT * FROM blob ORDER BY rid)) || '|' ||
		(SELECT group_concat(rid, ',') FROM (SELECT * FROM phantom ORDER BY rid))`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// fossil rebuild drops every table it does not know, the store cone's among
// them: fossil reads a sparse store as an ordinary one with phantoms, and
// the store heals toward dense.
func scFossilRebuild(t *testing.T) error {
	ref := newFossilReference(t)
	if ref == nil {
		return nil
	}
	s := newSCSparse(t, false)
	path := s.clone.Path()
	// fossil(1) opens the file itself; hand it over closed.
	if err := s.clone.Close(); err != nil {
		return err
	}
	out, err := ref.command(context.Background(), "test-integrity", "-R", path).CombinedOutput()
	if err != nil {
		return expectf("fossil test-integrity on a sparse store: %v: %s", err, out)
	}
	if !regexp.MustCompile(`checked:\s+0 errors`).Match(out) {
		return expectf("fossil test-integrity on a sparse store: %s", out)
	}
	if out, err := ref.command(context.Background(), "rebuild", path).CombinedOutput(); err != nil {
		return expectf("fossil rebuild on a sparse store: %v: %s", err, out)
	}
	reopened, err := repo.Open(path)
	if err != nil {
		return err
	}
	s.clone = reopened
	if c, err := content.StoreCone(s.clone.DB()); err != nil || c != nil {
		return expectf("after fossil rebuild the store cone is %v, %v; want it gone", c, err)
	}
	if err := scSameList("transit after fossil rebuild", scClassed(scAbsences(t, s.clone), content.Transit), scSorted(s.elided)); err != nil {
		return err
	}
	report, err := scVerify(s.clone)
	if err != nil {
		return err
	}
	if report.Phantoms != len(s.elided) {
		return expectf("verify after fossil rebuild counts %d transit phantoms, want %d", report.Phantoms, len(s.elided))
	}
	// fossil's own pull from a dense peer then heals the store. fossil
	// pulls as a local user, which a library clone does not create.
	if _, err := reopened.DB().Exec("INSERT OR IGNORE INTO user(login, cap) VALUES('u', 's')"); err != nil {
		return err
	}
	if err := reopened.Close(); err != nil {
		return err
	}
	server := httptest.NewServer(sync.XferHandler(s.origin.r, sync.HandleSync))
	defer server.Close()
	if out, err := ref.command(context.Background(), "pull", server.URL, "-R", path).CombinedOutput(); err != nil {
		return expectf("fossil pull into a formerly sparse store: %v: %s", err, out)
	}
	healed, err := repo.Open(path)
	if err != nil {
		return err
	}
	defer healed.Close()
	if report, err = scVerify(healed); err != nil {
		return err
	}
	if report.Phantoms != 0 || report.Elided != 0 {
		return expectf("after fossil's pull the store lacks %d transit and %d elided artifacts; want it dense",
			report.Phantoms, report.Elided)
	}
	return nil
}

// --- §5.2: verification -------------------------------------------------------

// A store complete within its cone verifies healthy, with its elision
// counted apart. Transit phantoms and damaged artifacts are reported as a
// dense store reports them, and Rebuild keeps all of it.
func scVerifySparse(t *testing.T) error {
	s := newSCSparse(t, false)
	report, err := scVerify(s.clone)
	if err != nil {
		return err
	}
	if report.Elided != len(s.elided) || report.Phantoms != 0 {
		return expectf("a store complete within its cone: elided %d, transit %d; want %d, 0",
			report.Elided, report.Phantoms, len(s.elided))
	}
	rebuilt, err := verify.Rebuild(s.clone)
	if err != nil {
		return err
	}
	if !rebuilt.OK() || rebuilt.Elided != len(s.elided) {
		return expectf("rebuild of a sparse store: OK=%v elided %d, issues %+v", rebuilt.OK(), rebuilt.Elided, rebuilt.Issues)
	}
	if c, _ := content.StoreCone(s.clone.DB()); c == nil {
		return expectf("rebuild dropped the store cone")
	}

	// The same transit phantom and the same damage in a dense clone and in
	// the sparse one are reported alike.
	dense, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "dense.fossil"),
		s.transport(s.origin.r), sync.CloneOpts{})
	if err != nil {
		return err
	}
	defer dense.Close()
	c1 := s.origin.c1
	transit := scUUIDOf(t, s.origin.r, c1, "src/a.c")
	damaged := scUUIDOf(t, s.origin.r, c1, "src/b.c")
	var reports []*verify.Report
	for _, r := range []*repo.Repo{dense, s.clone} {
		scPhantomize(t, r, transit)
		rid, _ := content.AvailableByUUID(r.DB(), damaged)
		if err := r.WithTx(func(tx *db.Tx) error { return content.Undelta(tx, rid) }); err != nil {
			return err
		}
		if _, err := r.DB().Exec("UPDATE blob SET content=zeroblob(length(content)) WHERE rid=?", rid); err != nil {
			return err
		}
		report, err := verify.Verify(r)
		if err != nil {
			return err
		}
		reports = append(reports, report)
	}
	d, sp := reports[0], reports[1]
	if d.Phantoms != 1 || sp.Phantoms != 1 {
		return expectf("transit phantoms: dense %d, sparse %d; want 1 each", d.Phantoms, sp.Phantoms)
	}
	if sp.Elided != len(s.elided) {
		return expectf("sparse elided %d beside the damage, want %d", sp.Elided, len(s.elided))
	}
	if got, want := scIssueKinds(sp), scIssueKinds(d); !slices.Equal(got, want) || len(want) == 0 {
		return expectf("sparse issues %v, dense issues %v", got, want)
	}
	return nil
}

func scIssueKinds(r *verify.Report) []string {
	var out []string
	for _, issue := range r.Issues {
		out = append(out, fmt.Sprintf("%d:%s", issue.Kind, issue.UUID))
	}
	sort.Strings(out)
	return out
}

// --- §5.3, §5.4: commit, push and widening ------------------------------------

// A commit over elided subtrees, pushed to a dense peer, gives that peer the
// whole check-in: the edit, and the out-of-cone files the committer never
// held.
func scSparsePush(t *testing.T) error {
	for _, tree := range []bool{false, true} {
		s := newSCSparse(t, tree)
		rid, err := s.commit(t)
		if err != nil {
			return err
		}
		var uuid string
		s.clone.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid)
		if err := s.sync(t, s.clone, s.origin.r, true, true); err != nil {
			return err
		}
		var pushed libfossil.FslID
		if err := s.origin.r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=? AND size>=0", uuid).Scan(&pushed); err != nil {
			return expectf("tree=%v: the sparse commit did not reach the dense peer: %v", tree, err)
		}
		got, err := scContent(s.origin.r, pushed)
		if err != nil {
			return err
		}
		if err := scSameContent(fmt.Sprintf("tree=%v: the pushed check-in", tree), got, scEdited()); err != nil {
			return err
		}
		if _, err := scVerify(s.origin.r); err != nil {
			return err
		}
		// The dense peer can serve the whole check-in to a dense clone.
		dense, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "d.fossil"),
			s.transport(s.origin.r), sync.CloneOpts{})
		if err != nil {
			return err
		}
		defer dense.Close()
		var cloned libfossil.FslID
		dense.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&cloned)
		if got, err = scContent(dense, cloned); err != nil {
			return err
		}
		if err := scSameContent("a dense clone of the pushed check-in", got, scEdited()); err != nil {
			return err
		}
	}
	return nil
}

// Widening reclassifies what the cone newly covers as transit phantoms, and
// an ordinary pull fetches exactly those.
func scWidenStore(t *testing.T) error {
	s := newSCSparse(t, false)
	c1 := s.rid(t, s.origin.c1)
	tools := map[string]bool{}
	for _, name := range []string{"tools/t.c", "tools/x/y/z.txt"} {
		tools[scUUIDOf(t, s.clone, c1, name)] = true
	}
	wide := scCone(t, []string{"src", "src/vendor/keep", "tools"}, []string{"src/vendor"})
	if err := content.SetStoreCone(s.clone.DB(), wide); err != nil {
		return err
	}
	if err := s.sync(t, s.clone, s.origin.r, false, true); err != nil {
		return err
	}
	for uuid := range s.elided {
		if s.gimmes[uuid] != tools[uuid] {
			return expectf("pull after widening: asked for %s = %v, newly covered = %v", uuid, s.gimmes[uuid], tools[uuid])
		}
	}
	report, err := scVerify(s.clone)
	if err != nil {
		return err
	}
	if report.Phantoms != 0 || report.Elided != len(s.elided)-len(tools) {
		return expectf("after widening and a pull: transit %d, elided %d; want 0, %d",
			report.Phantoms, report.Elided, len(s.elided)-len(tools))
	}
	return nil
}

// --- §6.2: a delta's base -----------------------------------------------------

// A phantom a held delta names as its base is wanted whatever the cone says:
// without it the held artifact cannot be read. A server predating §6.2's
// full-form rule sends such a delta; the store keeps it as received.
func scDeltaBase(t *testing.T) error {
	s := newSCSparse(t, false)
	base := scUUIDOf(t, s.clone, s.rid(t, s.origin.c1), "tools/t.c")
	if !s.elided[base] {
		return expectf("tools/t.c is not elided")
	}
	source := []byte(scFiles["tools/t.c"])
	target := []byte("a file derived from tools\n")
	d, err := delta.Create(source, target)
	if err != nil {
		return err
	}
	var brid libfossil.FslID
	if err := s.clone.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", base).Scan(&brid); err != nil {
		return err
	}
	name := hash.SHA3(target)
	if len(base) == 40 {
		name = hash.SHA1(target)
	}
	if _, err := blob.StoreDeltaRaw(s.clone.DB(), name, d, brid, nil); err != nil {
		return err
	}
	if got := scAbsences(t, s.clone)[base]; got != content.Transit {
		return expectf("an out-of-cone delta base is %v, want transit", got)
	}
	return nil
}
