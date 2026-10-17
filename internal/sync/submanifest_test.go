package sync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
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
		if err := staged.apply(r.DB()); err != nil {
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
		if err := staged.apply(r.DB()); err == nil {
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
	if err := staged.apply(r.DB()); err != nil {
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

	client, err := repo.Create(filepath.Join(t.TempDir(), "client.fossil"), "u", simio.CryptoRand{}, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	a := storeTree(t, client)
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		return fossilHTTPXfer(t, bin, repoPath, req)
	}}
	res, err := Sync(context.Background(), client, transport, SyncOpts{Push: true, Pull: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, uuid := range []string{a.uuid, a.leafUUID} {
		n := strings.TrimSpace(fossilOut(t, bin, "sql", "-R", repoPath,
			"SELECT count(*) FROM blob WHERE uuid='"+uuid+"' AND size>=0"))
		if n != "0" {
			t.Fatalf("fossil 2.28 stored %s", uuid)
		}
	}
	n := strings.TrimSpace(fossilOut(t, bin, "sql", "-R", repoPath,
		"SELECT count(*) FROM blob WHERE uuid='"+a.fileUUID+"' AND size>=0"))
	if n != "1" {
		t.Fatalf("fossil 2.28 did not receive the ordinary file (count %s)", n)
	}
	if !strings.Contains(strings.Join(res.Errors, "\n"), "withheld") {
		t.Fatalf("result errors %q do not record the withheld artifacts", res.Errors)
	}
}
