package sync

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// These tests pin the transfer and configuration rules of
// draft-fossil-submanifests-00 for a reader: the §7.2 peer-version gate in
// both directions, the avoid-submanifests pragma, the version this library
// announces, and the §6.3 configuration exchange as zit's clone performs it.

// treeArtifacts is a one-leaf tree check-in and its parts.
type treeArtifacts struct {
	file, leaf, tree         []byte
	fileUUID, leafUUID, uuid string
}

// storeTree stores a one-file tree check-in in r and crosslinks it, which
// also sets the seen-submanifest indicator that arms the §7.2 gate.
func storeTree(t *testing.T, r *repo.Repo) treeArtifacts {
	t.Helper()
	marshal := func(d *deck.Deck) []byte {
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var a treeArtifacts
	a.file = []byte("tree file\n")
	a.fileUUID = storeAndLink(t, r, a.file)
	a.leaf = marshal(&deck.Deck{F: []deck.FileCard{{Name: "a.txt", UUID: a.fileUUID}}})
	a.leafUUID = storeAndLink(t, r, a.leaf)
	a.tree = marshal(&deck.Deck{C: "tree", D: time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC),
		S: []deck.SubmanifestCard{{Name: "a.txt", UUID: a.leafUUID}}, U: deck.User("u"),
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagPropagating, Name: "sym-trunk", UUID: "*"},
		}})
	a.uuid = storeAndLink(t, r, a.tree)
	return a
}

func TestTransferGatedShapes(t *testing.T) {
	r := setupSyncTestRepo(t)
	a := storeTree(t, r)
	for name, tc := range map[string]struct {
		data  []byte
		gated bool
	}{
		"tree manifest": {a.tree, true},
		"leaf":          {a.leaf, true},
		"file":          {a.file, false},
	} {
		if got := transferGated(tc.data); got != tc.gated {
			t.Errorf("%s: transferGated = %v, want %v", name, got, tc.gated)
		}
	}
	flat, err := (&deck.Deck{C: "flat", D: time.Now().UTC(),
		F: []deck.FileCard{{Name: "a.txt", UUID: a.fileUUID}}, U: deck.User("u")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if transferGated(flat) {
		t.Error("a flat manifest is gated")
	}
}

func TestAnnouncedVersionIsSubmanifestThreshold(t *testing.T) {
	p := versionPragma("client-version")
	if p.Values[0] != "22900" {
		t.Fatalf("announced %s, want 22900", p.Values[0])
	}
}

// gimmeReply asks r's handler for uuid as a client announcing version.
func gimmeReply(t *testing.T, r *repo.Repo, version, uuid string) *xfer.Message {
	t.Helper()
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "client-version", Values: []string{version, "20260101", "000000"}},
		&xfer.PullCard{ServerCode: "x", ProjectCode: testPC},
		&xfer.GimmeCard{UUID: uuid},
	}})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	return resp
}

// §7.2: a server withholds a tree from a client older than sub-manifest
// support, with one explicit error, and sends it to one that is not.
func TestServerWithholdsTreeFromOldClient(t *testing.T) {
	r := setupSyncTestRepo(t)
	a := storeTree(t, r)
	for _, uuid := range []string{a.uuid, a.leafUUID} {
		old := gimmeReply(t, r, "22800", uuid)
		if files := findCards[*xfer.FileCard](old); len(files) != 0 {
			t.Fatalf("a 2.28 client received %s", uuid)
		}
		errs := findCards[*xfer.ErrorCard](old)
		if len(errs) != 1 || !strings.Contains(errs[0].Message, "2.29 or later") {
			t.Fatalf("refusal cards %v", errs)
		}
		current := gimmeReply(t, r, "22900", uuid)
		if files := findCards[*xfer.FileCard](current); len(files) != 1 {
			t.Fatalf("a 2.29 client did not receive %s", uuid)
		}
	}
	// Ordinary content is never gated.
	if files := findCards[*xfer.FileCard](gimmeReply(t, r, "22800", a.fileUUID)); len(files) != 1 {
		t.Fatal("a file blob was withheld")
	}
}

// §7.2, client side: a push answers a 2.28 server's gimme for a tree with a
// refusal, records what it withheld, and the server never stores it.
func TestClientWithholdsTreeFromOldServer(t *testing.T) {
	server, client := setupSyncTestRepo(t), setupSyncTestRepo(t)
	grantNobodyPush(t, server)
	a := storeTree(t, client)
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), server, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		for _, c := range resp.Cards { // pose as fossil 2.28
			if p, ok := c.(*xfer.PragmaCard); ok && p.Name == "server-version" {
				p.Values[0] = "22800"
			}
		}
		return resp
	}}
	res, err := Sync(context.Background(), client, transport, SyncOpts{Push: true, Pull: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, ok := blob.Exists(server.DB(), a.uuid); ok && !isPhantom(t, server, a.uuid) {
		t.Fatal("the server stored the tree manifest")
	}
	if _, ok := blob.Exists(server.DB(), a.leafUUID); ok && !isPhantom(t, server, a.leafUUID) {
		t.Fatal("the server stored the leaf")
	}
	if !strings.Contains(strings.Join(res.Errors, "\n"), "withheld") {
		t.Fatalf("result errors %q do not record the withheld artifacts", res.Errors)
	}
}

func isPhantom(t *testing.T, r *repo.Repo, uuid string) bool {
	t.Helper()
	var size int
	if err := r.DB().QueryRow("SELECT size FROM blob WHERE uuid=?", uuid).Scan(&size); err != nil {
		t.Fatal(err)
	}
	return size < 0
}

// A server whose enable-submanifests is off answers a ci-lock with
// avoid-submanifests, where avoid-delta-manifests goes; with it on, not.
func TestCiLockReplyCarriesAvoidSubmanifests(t *testing.T) {
	r := setupSyncTestRepo(t)
	ask := func() bool {
		resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
			&xfer.PragmaCard{Name: "client-version", Values: []string{"22900", "20260101", "000000"}},
			&xfer.PullCard{ServerCode: "x", ProjectCode: testPC},
			&xfer.PragmaCard{Name: "ci-lock", Values: []string{testPC, "client-1"}},
		}})
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		for _, p := range findCards[*xfer.PragmaCard](resp) {
			if p.Name == "avoid-submanifests" {
				return true
			}
		}
		return false
	}
	if !ask() {
		t.Fatal("no avoid-submanifests with the setting off")
	}
	if _, err := r.DB().Exec(`REPLACE INTO config(name, value, mtime) VALUES
		('enable-submanifests', 'on', 1), ('submanifest-k', '3', 1)`); err != nil {
		t.Fatal(err)
	}
	if ask() {
		t.Fatal("avoid-submanifests with the setting on")
	}
}

// §6.3 as zit's clone performs it: the cleanup round asks for the scalar
// settings, and the three sub-manifest settings arrive whole.
func TestCloneAdoptsSubmanifestSettings(t *testing.T) {
	server := setupSyncTestRepo(t)
	storeTree(t, server)
	if _, err := server.DB().Exec(`REPLACE INTO config(name, value, mtime) VALUES
		('enable-submanifests', 'on', 10), ('submanifest-k', '3', 11),
		('submanifest-auto-retune', 'off', 12)`); err != nil {
		t.Fatal(err)
	}
	client := cloneTreeRepo(t, server)
	for name, want := range map[string]string{
		"enable-submanifests": "on", "submanifest-k": "3", "submanifest-auto-retune": "off",
	} {
		got, _ := currentSetting(client.DB(), name)
		if got != want {
			t.Errorf("cloned %s = %q, want %q", name, got, want)
		}
	}
}

// §6.3: enable-submanifests on with no valid k rejects all three settings.
func TestCloneRejectsInvalidSubmanifestSettings(t *testing.T) {
	server := setupSyncTestRepo(t)
	if _, err := server.DB().Exec(`REPLACE INTO config(name, value, mtime) VALUES
		('enable-submanifests', 'on', 10), ('submanifest-k', '13', 11),
		('submanifest-auto-retune', 'off', 12)`); err != nil {
		t.Fatal(err)
	}
	client := cloneTreeRepo(t, server)
	for _, name := range submanifestSettings {
		if got, ok := currentSetting(client.DB(), name); ok {
			t.Errorf("rejected set applied %s = %q", name, got)
		}
	}
}

func cloneTreeRepo(t *testing.T, server *repo.Repo) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clone.fossil")
	client, _, err := Clone(context.Background(), path, handlerTransport(t, server), CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// Staging makes card order irrelevant (§6.3): the same rows in any order,
// across one or several cards, give the same state, and any invalid row
// rejects all three.
func TestStagedSubmanifestConfig(t *testing.T) {
	rows := []string{
		"10 'enable-submanifests' value 'on'",
		"11 'submanifest-k' value '3'",
		"12 'submanifest-auto-retune' value 'off'",
	}
	orders := [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}}
	for _, order := range orders {
		r := setupSyncTestRepo(t)
		var staged stagedSubmanifestConfig
		for _, i := range order {
			staged.stage([]byte(rows[i]))
		}
		if err := applyInTx(t, r, &staged); err != nil {
			t.Fatalf("order %v: %v", order, err)
		}
		if k, _ := currentSetting(r.DB(), "submanifest-k"); k != "3" {
			t.Fatalf("order %v: k = %q", order, k)
		}
	}
	for _, bad := range []string{
		"13 'submanifest-auto-retune' value 'maybe'",
		"13 'submanifest-k' value '0'",
		"13 'enable-submanifests' value 'perhaps'",
	} {
		r := setupSyncTestRepo(t)
		var staged stagedSubmanifestConfig
		staged.stage([]byte(strings.Join(append(rows[:2:2], bad), "\n")))
		if err := applyInTx(t, r, &staged); err == nil {
			t.Fatalf("%q accepted", bad)
		}
		for _, name := range submanifestSettings {
			if v, ok := currentSetting(r.DB(), name); ok {
				t.Fatalf("%q: partial state %s=%q", bad, name, v)
			}
		}
	}
	// A k already pinned locally satisfies enable-submanifests on its own.
	r := setupSyncTestRepo(t)
	if _, err := r.DB().Exec("REPLACE INTO config VALUES('submanifest-k','4',1)"); err != nil {
		t.Fatal(err)
	}
	var staged stagedSubmanifestConfig
	staged.stage([]byte(rows[0]))
	if err := applyInTx(t, r, &staged); err != nil {
		t.Fatalf("enable with a local k: %v", err)
	}
}

// Against fossil 2.28: its server sees the 2.29 announcement and the clone's
// reqconfig /config without complaint, answers the latter with nothing, and
// a push from a repository holding a tree never hands it the tree.
func TestTreesNeverReachFossil228(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	repoPath := filepath.Join(t.TempDir(), "fossil.fossil")
	fossilRun(t, bin, "new", repoPath)
	fossilRun(t, bin, "settings", "backoffice-disable", "1", "-R", repoPath)
	fossilRun(t, bin, "user", "capabilities", "nobody", "gio", "-R", repoPath)
	pc := strings.Trim(strings.TrimSpace(fossilOut(t, bin, "sql", "-R", repoPath,
		"SELECT value FROM config WHERE name='project-code'")), "'")

	resp := fossilHTTPXfer(t, bin, repoPath, &xfer.Message{Cards: []xfer.Card{
		versionPragma("client-version"), &xfer.ReqConfigCard{Name: configScalarMarker},
	}})
	if errs := findCards[*xfer.ErrorCard](resp); len(errs) != 0 {
		t.Fatalf("fossil refused reqconfig /config: %v", errs)
	}
	if cfg := findCards[*xfer.ConfigCard](resp); len(cfg) != 0 {
		t.Fatalf("fossil answered reqconfig /config with %d cards", len(cfg))
	}

	// A tree stored as received, and one this library's writer made.
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, r *repo.Repo) (tree []string, file string)
	}{
		{"stored", func(t *testing.T, r *repo.Repo) ([]string, string) {
			a := storeTree(t, r)
			return []string{a.uuid, a.leafUUID}, a.fileUUID
		}},
		{"written", writeTree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := repo.Create(filepath.Join(t.TempDir(), "client.fossil"), "u", simio.CryptoRand{}, pc)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			tree, file := tc.build(t, client)
			transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
				return fossilHTTPXfer(t, bin, repoPath, req)
			}}
			res, err := Sync(context.Background(), client, transport, SyncOpts{Push: true, Pull: true})
			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			for _, uuid := range tree {
				n := strings.TrimSpace(fossilOut(t, bin, "sql", "-R", repoPath,
					"SELECT count(*) FROM blob WHERE uuid='"+uuid+"' AND size>=0"))
				if n != "0" {
					t.Fatalf("fossil 2.28 stored %s", uuid)
				}
			}
			n := strings.TrimSpace(fossilOut(t, bin, "sql", "-R", repoPath,
				"SELECT count(*) FROM blob WHERE uuid='"+file+"' AND size>=0"))
			if n != "1" {
				t.Fatalf("fossil 2.28 did not receive the ordinary file (count %s)", n)
			}
			if !strings.Contains(strings.Join(res.Errors, "\n"), "withheld") {
				t.Fatalf("result errors %q do not record the withheld artifacts", res.Errors)
			}
		})
	}
}

// writeTree commits a two-leaf tree with emission enabled and returns the
// names of its manifest and leaves, and of one of its files.
func writeTree(t *testing.T, r *repo.Repo) ([]string, string) {
	t.Helper()
	if _, err := r.DB().Exec(`REPLACE INTO config(name, value, mtime) VALUES
		('enable-submanifests', 'on', 1), ('submanifest-k', '3', 1)`); err != nil {
		t.Fatal(err)
	}
	rid, uuid, err := manifest.Checkin(r, manifest.CheckinOpts{Files: []manifest.File{
		{Name: "README.md", Content: []byte("# Example\n")},
		{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
		{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
		{Name: "src/util.c", Content: []byte("/* util */\n")},
	}, Comment: "written tree", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := manifest.GetManifest(r, rid)
	if err != nil || len(d.S) != 2 {
		t.Fatalf("the commit is not a tree (%v)", err)
	}
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	return []string{uuid, d.S[0].UUID, d.S[1].UUID}, files[0].UUID
}

// storeFlatDeltaOfTree stores, beside a tree check-in, a flat check-in
// stored as a delta of that tree manifest -- the normal shape once a
// repository switches between flat and tree manifests -- and returns the
// flat check-in's name.
func storeFlatDeltaOfTree(t *testing.T, r *repo.Repo, a treeArtifacts) string {
	t.Helper()
	// A long shared comment makes the delta worth storing.
	comment := strings.Repeat("shared comment text ", 200)
	tree, err := (&deck.Deck{C: comment, D: time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		S: []deck.SubmanifestCard{{Name: "a.txt", UUID: a.leafUUID}}, U: deck.User("u")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	treeUUID := storeAndLink(t, r, tree)
	flat, err := (&deck.Deck{C: comment, D: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		F: []deck.FileCard{{Name: "a.txt", UUID: a.fileUUID}}, P: []string{treeUUID},
		U: deck.User("u")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	flatUUID := storeAndLink(t, r, flat)
	treeRID, _ := blob.Exists(r.DB(), treeUUID)
	flatRID, _ := blob.Exists(r.DB(), flatUUID)
	if err := r.WithTx(func(tx *db.Tx) error {
		_, err := content.Deltify(tx, flatRID, treeRID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if src, _ := content.DeltaSource(r.DB(), flatRID); src != treeRID {
		t.Fatalf("the flat manifest is not stored as a delta of the tree (source %d)", src)
	}
	return flatUUID
}

// asOldClient rewrites a request's client-version to fossil 2.28's.
func asOldClient(server *repo.Repo, t *testing.T) *MockTransport {
	return &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		for _, c := range req.Cards {
			if p, ok := c.(*xfer.PragmaCard); ok && p.Name == "client-version" {
				p.Values[0] = "22800"
			}
		}
		resp, err := HandleSync(context.Background(), server, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		return resp
	}}
}

// §7.2 in a clone: a flat check-in stored as a delta of a withheld tree goes
// to a 2.28 client as full content, never as a delta it cannot apply.
func TestCloneSendsDeltaOfWithheldTreeInFull(t *testing.T) {
	server := setupSyncTestRepo(t)
	a := storeTree(t, server)
	flatUUID := storeFlatDeltaOfTree(t, server, a)
	path := filepath.Join(t.TempDir(), "old.fossil")
	client, _, err := Clone(context.Background(), path, asOldClient(server, t), CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer client.Close()
	rid, ok := content.AvailableByUUID(client.DB(), flatUUID)
	if !ok {
		t.Fatal("the flat check-in did not arrive whole")
	}
	if _, err := content.Expand(client.DB(), rid); err != nil {
		t.Fatalf("the flat check-in cannot be expanded: %v", err)
	}
	for _, uuid := range []string{a.uuid, a.leafUUID} {
		if _, ok := content.AvailableByUUID(client.DB(), uuid); ok {
			t.Fatalf("a 2.28 client received %s", uuid)
		}
	}
}

// treeBytes builds a one-file tree check-in's artifacts without storing them.
func treeBytes(t *testing.T, r *repo.Repo, comment string) (file, leaf, tree []byte) {
	t.Helper()
	marshal := func(d *deck.Deck) []byte {
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	file = []byte("file of " + comment + "\n")
	fileUUID := hash.MustNamingFor(r.DB()).New.Hash(file)
	leaf = marshal(&deck.Deck{F: []deck.FileCard{{Name: "a.txt", UUID: fileUUID}}})
	leafUUID := hash.MustNamingFor(r.DB()).New.Hash(leaf)
	tree = marshal(&deck.Deck{C: comment, D: time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC),
		S: []deck.SubmanifestCard{{Name: "a.txt", UUID: leafUUID}}, U: deck.User("u")})
	return file, leaf, tree
}

// §7.2 arms on arrival, not on crosslink: a tree a sync stored, before any
// crosslink has run, is already withheld from a 2.28 peer -- full content
// and a verified delta alike.
func TestGateArmsWhenTreeIsReceived(t *testing.T) {
	r := setupSyncTestRepo(t)
	file, leaf, tree := treeBytes(t, r, strings.Repeat("firsttree", 100))
	pass := newReceivePass()
	for _, data := range [][]byte{file, leaf, tree} {
		uuid := hash.MustNamingFor(r.DB()).New.Hash(data)
		if err := storeReceivedFile(context.Background(), r, pass, uuid, "", data, nil,
			visibilityPublic, nil); err != nil {
			t.Fatal(err)
		}
	}
	treeUUID := hash.MustNamingFor(r.DB()).New.Hash(tree)
	if files := findCards[*xfer.FileCard](gimmeReply(t, r, "22800", treeUUID)); len(files) != 0 {
		t.Fatal("a received, uncrosslinked tree went to a 2.28 peer")
	}

	// A second tree received as a delta of the first, on a fresh repository.
	r2 := setupSyncTestRepo(t)
	pass2 := newReceivePass()
	for _, data := range [][]byte{file, leaf} {
		if err := storeReceivedFile(context.Background(), r2, pass2,
			hash.MustNamingFor(r2.DB()).New.Hash(data), "", data, nil, visibilityPublic, nil); err != nil {
			t.Fatal(err)
		}
	}
	// The first tree arrives as ordinary bytes the gate never saw parsed:
	// stored directly, so only the delta's arrival can arm the gate.
	if _, _, err := blob.Store(r2.DB(), tree); err != nil {
		t.Fatal(err)
	}
	second := []byte(strings.Replace(string(tree), "firsttree", "othertree", 1))
	second = append(second[:len(second)-35], []byte(zCard(second[:len(second)-35]))...)
	d, err := delta.Create(tree, second)
	if err != nil {
		t.Fatal(err)
	}
	secondUUID := hash.MustNamingFor(r2.DB()).New.Hash(second)
	if err := storeReceivedFile(context.Background(), r2, pass2, secondUUID, treeUUID, d, nil,
		visibilityPublic, nil); err != nil {
		t.Fatal(err)
	}
	if files := findCards[*xfer.FileCard](gimmeReply(t, r2, "22800", secondUUID)); len(files) != 0 {
		t.Fatal("a tree received as a delta went to a 2.28 peer")
	}
}

// A repository an earlier version swept may hold trees filed as unparseable
// and no indicator: until a sweep re-examines them, the gate is armed.
func TestGateArmsOnRepositoryAnOlderVersionSwept(t *testing.T) {
	r := setupSyncTestRepo(t)
	file, leaf, tree := treeBytes(t, r, "legacy")
	for _, data := range [][]byte{file, leaf, tree} {
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.DB().Exec("CREATE TABLE crosslink_nonartifact(rid INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	treeUUID := hash.MustNamingFor(r.DB()).New.Hash(tree)
	if files := findCards[*xfer.FileCard](gimmeReply(t, r, "22800", treeUUID)); len(files) != 0 {
		t.Fatal("a tree in a repository an older version swept went to a 2.28 peer")
	}
}

func zCard(body []byte) string {
	sum := md5.Sum(body)
	return "Z " + hex.EncodeToString(sum[:]) + "\n"
}

// §6.3 all or none holds against the database too: a write that fails
// partway leaves none of the three settings applied.
func TestStagedSubmanifestConfigWriteFailureLeavesNone(t *testing.T) {
	r := setupSyncTestRepo(t)
	if _, err := r.DB().Exec(`CREATE TRIGGER refuse_k BEFORE INSERT ON config
		WHEN NEW.name='submanifest-k' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	var staged stagedSubmanifestConfig
	staged.stage([]byte("10 'enable-submanifests' value 'on'\n11 'submanifest-k' value '3'"))
	err := r.WithTx(func(tx *db.Tx) error {
		if err := staged.apply(tx); err == nil {
			t.Error("apply succeeded past a failing write")
		}
		return nil // the caller's transaction commits: only the savepoint undoes
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range submanifestSettings {
		if v, ok := currentSetting(r.DB(), name); ok {
			t.Fatalf("partial state %s=%q after a failed write", name, v)
		}
	}
}

// applyInTx runs apply as its callers do, inside one transaction.
func applyInTx(t *testing.T, r *repo.Repo, staged *stagedSubmanifestConfig) error {
	t.Helper()
	var applyErr error
	if err := r.WithTx(func(tx *db.Tx) error {
		applyErr = staged.apply(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return applyErr
}

// §7.2 and §8.1: a session whose server sends avoid-submanifests reports
// it, and a commit made in that session emits no S card even with emission
// enabled locally; without the pragma the same commit is a tree.
func TestAvoidSubmanifestsReachesTheSessionsCommit(t *testing.T) {
	server, client := newTestRepoPair(t)
	if _, err := client.DB().Exec(`REPLACE INTO config(name, value, mtime) VALUES
		('enable-submanifests', 'on', 1), ('submanifest-k', '3', 1)`); err != nil {
		t.Fatal(err)
	}
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), server, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		return resp
	}}
	res, err := Sync(context.Background(), client, transport, SyncOpts{Push: true, Pull: true,
		CkinLock: &CkinLockReq{ParentUUID: testPC, ClientID: "client-1"}})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !res.AvoidSubmanifests {
		t.Fatal("the session did not report avoid-submanifests")
	}
	files := []manifest.File{
		{Name: "README.md", Content: []byte("# Example\n")},
		{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
		{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
		{Name: "src/util.c", Content: []byte("/* util */\n")},
	}
	for _, avoid := range []bool{true, false} {
		rid, _, err := manifest.Checkin(client, manifest.CheckinOpts{Files: files, Comment: "c",
			User: "u", AvoidSubmanifests: avoid})
		if err != nil {
			t.Fatal(err)
		}
		d, err := manifest.GetManifest(client, rid)
		if err != nil {
			t.Fatal(err)
		}
		if tree := len(d.S) > 0; tree == avoid {
			t.Fatalf("avoid=%v: %d S cards", avoid, len(d.S))
		}
	}
}
