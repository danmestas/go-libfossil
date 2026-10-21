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
// draft-fossil-submanifests-00 for a reader: the §7.2 capability gate in
// both directions, the send-submanifests and avoid-submanifests pragmas and
// where they go, the version this library announces, and the §6.3
// configuration exchange as zit's clone performs it.

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

// The announced version is the fossil release whose protocol this library
// speaks; reading trees is announced by send-submanifests, not by a version.
func TestAnnouncesFossilBaseVersion(t *testing.T) {
	p := versionPragma("client-version")
	if p.Values[0] != "22800" {
		t.Fatalf("announced %s, want 22800", p.Values[0])
	}
}

// gimmeReply asks r's handler for uuid as a client announcing version, with
// pragma send-submanifests after client-version when reads is set.
func gimmeReply(t *testing.T, r *repo.Repo, version string, reads bool, uuid string) *xfer.Message {
	t.Helper()
	cards := []xfer.Card{
		&xfer.PragmaCard{Name: "client-version", Values: []string{version, "20260101", "000000"}},
	}
	if reads {
		cards = append(cards, &xfer.PragmaCard{Name: "send-submanifests"})
	}
	cards = append(cards,
		&xfer.PullCard{ServerCode: "x", ProjectCode: testPC},
		&xfer.GimmeCard{UUID: uuid})
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	return resp
}

// §7.2: a server withholds a tree from a client that did not send
// send-submanifests, whatever version it announces, with one explicit
// error, and sends it to one that did.
func TestServerWithholdsTreeWithoutPragma(t *testing.T) {
	r := setupSyncTestRepo(t)
	a := storeTree(t, r)
	for _, uuid := range []string{a.uuid, a.leafUUID} {
		for _, version := range []string{"22800", "22900", "23000"} {
			old := gimmeReply(t, r, version, false, uuid)
			if files := findCards[*xfer.FileCard](old); len(files) != 0 {
				t.Fatalf("a %s client without the pragma received %s", version, uuid)
			}
			errs := findCards[*xfer.ErrorCard](old)
			if len(errs) != 1 || errs[0].Message != "Peer does not accept sub-manifests." {
				t.Fatalf("refusal cards %v", errs)
			}
		}
		current := gimmeReply(t, r, "22800", true, uuid)
		if files := findCards[*xfer.FileCard](current); len(files) != 1 {
			t.Fatalf("a client sending the pragma did not receive %s", uuid)
		}
		if errs := findCards[*xfer.ErrorCard](current); len(errs) != 0 {
			t.Fatalf("a client sending the pragma was refused: %v", errs)
		}
	}
	// Ordinary content is never gated.
	if files := findCards[*xfer.FileCard](gimmeReply(t, r, "22800", false, a.fileUUID)); len(files) != 1 {
		t.Fatal("a file blob was withheld")
	}
}

// §7.2: the server reads the whole request, so the pragma counts wherever
// in it the client put it.
func TestServerReadsPragmaAnywhereInRequest(t *testing.T) {
	r := setupSyncTestRepo(t)
	a := storeTree(t, r)
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "client-version", Values: []string{"22800", "20260101", "000000"}},
		&xfer.PullCard{ServerCode: "x", ProjectCode: testPC},
		&xfer.GimmeCard{UUID: a.uuid},
		&xfer.PragmaCard{Name: "send-submanifests"},
	}})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if files := findCards[*xfer.FileCard](resp); len(files) != 1 {
		t.Fatal("the tree was withheld from a request carrying the pragma")
	}
}

// stripPragma removes every pragma named name from msg.
func stripPragma(msg *xfer.Message, name string) {
	kept := msg.Cards[:0]
	for _, c := range msg.Cards {
		if p, ok := c.(*xfer.PragmaCard); ok && p.Name == name {
			continue
		}
		kept = append(kept, c)
	}
	msg.Cards = kept
}

// §7.2, client side: a push answers the gimme of a server that did not send
// send-submanifests with a refusal, records what it withheld, and the server
// never stores it -- even a server announcing a version above 22900.
func TestClientWithholdsTreeFromServerWithoutPragma(t *testing.T) {
	server, client := setupSyncTestRepo(t), setupSyncTestRepo(t)
	grantNobodyPush(t, server)
	a := storeTree(t, client)
	var refusals []string
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		for _, e := range findCards[*xfer.ErrorCard](req) {
			refusals = append(refusals, e.Message)
		}
		resp, err := HandleSync(context.Background(), server, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		stripPragma(resp, "send-submanifests")
		for _, c := range resp.Cards {
			if p, ok := c.(*xfer.PragmaCard); ok && p.Name == "server-version" {
				p.Values[0] = "23000"
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
	if len(refusals) == 0 || refusals[0] != "Peer does not accept sub-manifests." {
		t.Fatalf("refusals sent %q", refusals)
	}
}

// §7.2: with the pragma in the server's replies the same push delivers the
// tree whole.
func TestClientSendsTreeToServerWithPragma(t *testing.T) {
	server, client := setupSyncTestRepo(t), setupSyncTestRepo(t)
	grantNobodyPush(t, server)
	a := storeTree(t, client)
	res, err := Sync(context.Background(), client, handlerTransport(t, server), SyncOpts{Push: true, Pull: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("result errors %q", res.Errors)
	}
	for _, uuid := range []string{a.uuid, a.leafUUID, a.fileUUID} {
		if _, ok := blob.Exists(server.DB(), uuid); !ok || isPhantom(t, server, uuid) {
			t.Fatalf("the server lacks %s", uuid)
		}
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

// wireLines is msg as the lines it goes out as.
func wireLines(t *testing.T, msg *xfer.Message) []string {
	t.Helper()
	raw, err := msg.EncodeUncompressed()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// lineAfter fails unless the line following the first one beginning with
// prefix is want.
func lineAfter(t *testing.T, lines []string, prefix, want string) {
	t.Helper()
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			if i+1 >= len(lines) || lines[i+1] != want {
				t.Fatalf("after %q: got %q, want %q\n%s", line, lines[i+1:], want, strings.Join(lines, "\n"))
			}
			return
		}
	}
	t.Fatalf("no line begins %q:\n%s", prefix, strings.Join(lines, "\n"))
}

// §7.2: every request a sync or a clone sends carries send-submanifests
// right after client-version, private sync included, whatever the
// sub-manifest settings say.
func TestRequestsCarrySendSubmanifestsAfterClientVersion(t *testing.T) {
	for _, enable := range []string{"off", "on"} {
		server, client := newTestRepoPair(t)
		grantNobodyPush(t, server)
		if _, err := server.DB().Exec("UPDATE user SET cap=cap||'x' WHERE login='nobody'"); err != nil {
			t.Fatal(err)
		}
		if _, err := client.DB().Exec(`REPLACE INTO config(name, value, mtime) VALUES
			('enable-submanifests', ?, 1), ('submanifest-k', '3', 1)`, enable); err != nil {
			t.Fatal(err)
		}
		var requests int
		transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
			requests++
			lineAfter(t, wireLines(t, req), "pragma client-version ", "pragma send-submanifests")
			resp, err := HandleSync(context.Background(), server, req)
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			return resp
		}}
		for _, private := range []bool{false, true} {
			if _, err := Sync(context.Background(), client, transport,
				SyncOpts{Push: true, Pull: true, Private: private}); err != nil {
				t.Fatalf("Sync: %v", err)
			}
		}
		clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"),
			transport, CloneOpts{})
		if err != nil {
			t.Fatalf("Clone: %v", err)
		}
		clone.Close()
		if requests < 3 {
			t.Fatalf("only %d requests seen", requests)
		}
	}
}

// §7.2: every reply carries send-submanifests in its leading pragma run,
// right after server-version, then avoid-submanifests while
// enable-submanifests is off; with no server-version they lead the reply.
// The clone reply is no different.
func TestRepliesCarryCapabilityPragmasAfterServerVersion(t *testing.T) {
	r := setupSyncTestRepo(t)
	storeTree(t, r)
	cloneReq := func(cv *xfer.PragmaCard) []xfer.Card {
		return []xfer.Card{cv, &xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true}}
	}
	pullReq := func(cv *xfer.PragmaCard) []xfer.Card {
		return []xfer.Card{cv, &xfer.PullCard{ServerCode: "x", ProjectCode: testPC}}
	}
	dated := &xfer.PragmaCard{Name: "client-version", Values: []string{"22800", "20260101", "000000"}}
	bare := &xfer.PragmaCard{Name: "client-version", Values: []string{"22800"}}
	for _, enable := range []string{"off", "on"} {
		if _, err := r.DB().Exec(`REPLACE INTO config(name, value, mtime) VALUES
			('enable-submanifests', ?, 1), ('submanifest-k', '3', 1)`, enable); err != nil {
			t.Fatal(err)
		}
		want := []string{"pragma send-submanifests"}
		if enable == "off" {
			want = append(want, "pragma avoid-submanifests")
		}
		for name, cards := range map[string][]xfer.Card{
			"pull":       pullReq(dated),
			"clone":      cloneReq(dated),
			"pull bare":  pullReq(bare),
			"clone bare": cloneReq(bare),
			"no version": {&xfer.PullCard{ServerCode: "x", ProjectCode: testPC}},
		} {
			resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
			if err != nil {
				t.Fatalf("%s: HandleSync: %v", name, err)
			}
			lines := wireLines(t, resp)
			lead := 0
			if cards[0] == dated {
				if !strings.HasPrefix(lines[0], "pragma server-version 22800 ") {
					t.Fatalf("%s/%s: first line %q", enable, name, lines[0])
				}
				lead = 1
			}
			if len(lines) < lead+len(want)+1 {
				t.Fatalf("%s/%s: reply %q", enable, name, lines)
			}
			got := lines[lead : lead+len(want)]
			if strings.Join(got, "|") != strings.Join(want, "|") ||
				lines[lead+len(want)] == "pragma avoid-submanifests" {
				t.Fatalf("%s/%s: leading run %q, want %q\n%s", enable, name, got, want,
					strings.Join(lines, "\n"))
			}
			for _, line := range lines[lead+len(want):] {
				if line == "pragma send-submanifests" || line == "pragma avoid-submanifests" {
					t.Fatalf("%s/%s: %q repeated later in the reply", enable, name, line)
				}
			}
		}
	}
}

// §7.2: a card error that resets the reply discards the run with
// server-version, as zit and fossil reset theirs, and a client-version
// repeated in the request does not repeat it.
func TestCapabilityPragmasOncePerReply(t *testing.T) {
	r := setupSyncTestRepo(t)
	cv := &xfer.PragmaCard{Name: "client-version", Values: []string{"22800", "20260101", "000000"}}
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		cv, &xfer.BadCommandCard{Line: "bogus\n"},
		&xfer.PullCard{ServerCode: "x", ProjectCode: testPC},
	}})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if n := len(findCards[*xfer.PragmaCard](resp)); n != 0 {
		t.Fatalf("%d pragmas survived the reset", n)
	}
	resp, err = HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		cv, cv, &xfer.PullCard{ServerCode: "x", ProjectCode: testPC},
	}})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	sends := 0
	for _, p := range findCards[*xfer.PragmaCard](resp) {
		if p.Name == "send-submanifests" {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("send-submanifests %d times", sends)
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

// Against fossil 2.28: its server takes the clone's reqconfig /config
// without complaint and answers it with nothing, and, since its replies
// carry no send-submanifests, a push from a repository holding a tree never
// hands it the tree.
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

// writtenTreeFiles is the Appendix A.1 file set.
func writtenTreeFiles() []manifest.File {
	return []manifest.File{
		{Name: "README.md", Content: []byte("# Example\n")},
		{Name: "src/main.c", Content: []byte("int main(void){return 0;}\n")},
		{Name: "src/node11.c", Content: []byte("/* node11 */\n")},
		{Name: "src/util.c", Content: []byte("/* util */\n")},
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
	rid, uuid, err := manifest.Checkin(r, manifest.CheckinOpts{Files: writtenTreeFiles(),
		Comment: "written tree", User: "u"})
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

// asOldClient poses as a client that does not read trees, fossil's: its
// requests carry no send-submanifests.
func asOldClient(server *repo.Repo, t *testing.T) *MockTransport {
	return &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		stripPragma(req, "send-submanifests")
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
	if files := findCards[*xfer.FileCard](gimmeReply(t, r, "22800", false, treeUUID)); len(files) != 0 {
		t.Fatal("a received, uncrosslinked tree went to a peer that reads no tree")
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
	if files := findCards[*xfer.FileCard](gimmeReply(t, r2, "22800", false, secondUUID)); len(files) != 0 {
		t.Fatal("a tree received as a delta went to a peer that reads no tree")
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
	if files := findCards[*xfer.FileCard](gimmeReply(t, r, "22800", false, treeUUID)); len(files) != 0 {
		t.Fatal("a tree in a repository an older version swept went to a peer that reads no tree")
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

// A leaf a private check-in stored first is made public by the public tree
// commit that shares it, and the push carries the whole tree.
func TestPublicTreeSendsSharedPrivateLeaves(t *testing.T) {
	server, client := newTestRepoPair(t)
	name := hash.MustNamingFor(client.DB()).New.Hash
	var cards []deck.FileCard
	for _, f := range writtenTreeFiles() {
		cards = append(cards, deck.FileCard{Name: f.Name, UUID: name(f.Content)})
	}
	built, err := manifest.BuildTree(cards, 3, func(b []byte) (string, error) { return name(b), nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WithTx(func(tx *db.Tx) error {
		for _, sub := range built.Subs {
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
	tree, _ := writeTree(t, client)
	if _, err := server.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), server, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		return resp
	}}
	if _, err := Sync(context.Background(), client, transport, SyncOpts{Push: true}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, uuid := range tree {
		if rid, ok := blob.Exists(server.DB(), uuid); !ok || content.IsPrivate(server.DB(), int64(rid)) {
			t.Fatalf("the server did not receive %s publicly", uuid[:10])
		}
		var size int64
		server.DB().QueryRow("SELECT size FROM blob WHERE uuid=?", uuid).Scan(&size)
		if size < 0 {
			t.Fatalf("the server holds %s only as a phantom", uuid[:10])
		}
	}
}
