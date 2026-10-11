package sync

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A reqconfig card asks the server for configuration. fossil answers only
// names in its transferable set (configure.c aConfig), only for a /GROUP
// request, and only for groups the requester may read (xfer.c page_xfer).
// These tests pin that a requester cannot read any other config row —
// project-code, server-code, or a setting fossil never transfers.

// seedReqConfigRows writes one row in each config group the tests probe, plus
// rows fossil never transfers. Every row carries an mtime: fossil sends only
// rows whose mtime is at least the request's start time.
func seedReqConfigRows(t *testing.T, r *repo.Repo) {
	t.Helper()
	for _, q := range []string{
		"REPLACE INTO config(name,value,mtime) VALUES('project-name','Demo O''Proj',100)",
		"REPLACE INTO config(name,value,mtime) VALUES('css','body{}',101)",
		"REPLACE INTO config(name,value,mtime) VALUES('user-color-map','x',102)",
		"REPLACE INTO config(name,value,mtime) VALUES('secret-setting','zzz',103)",
		"REPLACE INTO config(name,value,mtime) VALUES('walias:/a','/b',104)",
		"UPDATE config SET mtime=105 WHERE name IN ('project-code','server-code')",
		"INSERT INTO shun(uuid,mtime,scom) VALUES('abc',106,NULL)",
		"INSERT INTO concealed(hash,mtime,content) VALUES('h1',107,'a@b')",
		"UPDATE user SET mtime=108",
	} {
		if _, err := r.DB().Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
}

func setNobodyCaps(t *testing.T, r *repo.Repo, caps string) {
	t.Helper()
	if _, err := r.DB().Exec("UPDATE user SET cap=? WHERE login='nobody'", caps); err != nil {
		t.Fatalf("set nobody caps: %v", err)
	}
}

// reqConfig sends one reqconfig card per name, anonymously, and returns the
// reply. A refused name must not cost the exchange, so any error card fails.
func reqConfig(t *testing.T, r *repo.Repo, names ...string) *xfer.Message {
	t.Helper()
	var cards []xfer.Card
	for _, n := range names {
		cards = append(cards, &xfer.ReqConfigCard{Name: n})
	}
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if errs := findCards[*xfer.ErrorCard](resp); len(errs) != 0 {
		t.Fatalf("reqconfig %v: error cards %v, want none", names, errs)
	}
	return resp
}

// configLines renders each config card as "NAME PAYLOAD" for comparison.
func configLines(resp *xfer.Message) []string {
	var out []string
	for _, c := range findCards[*xfer.ConfigCard](resp) {
		out = append(out, c.Name+" "+string(c.Content))
	}
	return out
}

func TestReqConfigWithoutReadSendsNothing(t *testing.T) {
	r := setupSyncTestRepo(t)
	seedReqConfigRows(t, r)
	setNobodyCaps(t, r, "")

	resp := reqConfig(t, r, "project-code", "server-code", "/all", "/project", "/user")
	if got := configLines(resp); len(got) != 0 {
		t.Fatalf("requester without Read got config cards %q, want none", got)
	}
}

// A bare name is fossil's retired single-name request form; fossil ignores
// it whatever the requester's capabilities.
func TestReqConfigBareNameIgnored(t *testing.T) {
	r := setupSyncTestRepo(t)
	seedReqConfigRows(t, r)
	setNobodyCaps(t, r, "oi")

	resp := reqConfig(t, r, "project-code", "server-code", "project-name", "css")
	if got := configLines(resp); len(got) != 0 {
		t.Fatalf("bare reqconfig names got config cards %q, want none", got)
	}
}

func TestReqConfigSendsOnlyTransferableNames(t *testing.T) {
	r := setupSyncTestRepo(t)
	seedReqConfigRows(t, r)
	setNobodyCaps(t, r, "o")

	got := configLines(reqConfig(t, r, "/all"))
	want := []string{
		"/shun 106 'abc' scom NULL",
		"/config 104 'walias:/a' value '/b'",
		"/config 101 'css' value 'body{}'",
		"/config 100 'project-name' value 'Demo O''Proj'",
	}
	// hash-policy is seeded by repo.Create with the creation time as mtime,
	// so its payload is not fixed; check only that it is the one extra card.
	var rest []string
	for _, l := range got {
		if strings.HasPrefix(l, "/config ") && strings.Contains(l, " 'hash-policy' value ") {
			continue
		}
		rest = append(rest, l)
	}
	if strings.Join(rest, "\n") != strings.Join(want, "\n") {
		t.Fatalf("/all for a reader:\ngot  %q\nwant %q", rest, want)
	}
	for _, l := range got {
		for _, secret := range []string{"project-code", "server-code", "secret-setting"} {
			if strings.Contains(l, secret) {
				t.Errorf("/all disclosed %s: %q", secret, l)
			}
		}
	}
}

// Group names match by prefix, as configure_name_to_mask does, first match
// in fossil's table order: "/" alone selects /email.
func TestReqConfigGroupPrefix(t *testing.T) {
	r := setupSyncTestRepo(t)
	seedReqConfigRows(t, r)
	setNobodyCaps(t, r, "o")

	cases := []struct {
		name string
		want []string
	}{
		{"/s", []string{"/config 101 'css' value 'body{}'"}},
		{"/sh", []string{"/shun 106 'abc' scom NULL"}},
		{"/p", []string{"/config 100 'project-name' value 'Demo O''Proj'"}},
		{"/", nil},      // /email: concealed addresses need RdAddr
		{"/skinx", nil}, // longer than any group name
		{"/nosuch", nil},
	}
	for _, tc := range cases {
		var got []string
		for _, l := range configLines(reqConfig(t, r, tc.name)) {
			if !strings.Contains(l, "'hash-policy'") {
				got = append(got, l)
			}
		}
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("reqconfig %s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReqConfigAddrNeedsRdAddr(t *testing.T) {
	r := setupSyncTestRepo(t)
	seedReqConfigRows(t, r)

	setNobodyCaps(t, r, "o")
	if got := configLines(reqConfig(t, r, "/email")); len(got) != 0 {
		t.Fatalf("/email without e: got %q, want none", got)
	}
	setNobodyCaps(t, r, "oe")
	got := configLines(reqConfig(t, r, "/email"))
	if len(got) != 1 || got[0] != "/concealed 107 'h1' content 'a@b'" {
		t.Fatalf("/email with e: got %q", got)
	}
}

// The user table carries password hashes. fossil sends it only to Admin; an
// admin bit on the unauthenticated nobody principal does not count.
func TestReqConfigUserNeedsAuthenticatedAdmin(t *testing.T) {
	r := setupSyncTestRepo(t)
	seedReqConfigRows(t, r)

	for _, caps := range []string{"o", "oe", "s", "a"} {
		setNobodyCaps(t, r, caps)
		for _, l := range configLines(reqConfig(t, r, "/user", "/all")) {
			if strings.HasPrefix(l, "/user ") || strings.Contains(l, "user-color-map") {
				t.Errorf("anonymous caps %q got user group card %q", caps, l)
			}
		}
	}

	pc := testProjectCode(t, r.DB())
	if err := auth.CreateUser(r.DB(), pc, "adm", "pw", "a"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := r.DB().Exec("UPDATE user SET mtime=108"); err != nil {
		t.Fatalf("user mtime: %v", err)
	}
	rest := []xfer.Card{&xfer.ReqConfigCard{Name: "/user"}}
	payload, err := (&xfer.Message{Cards: rest}).EncodeUncompressed()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	login := buildTestLoginCard("adm", "pw", pc, payload)
	resp, err := HandleSync(context.Background(), r,
		&xfer.Message{Cards: append([]xfer.Card{login}, rest...)})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	users := 0
	for _, l := range configLines(resp) {
		if strings.HasPrefix(l, "/user 108 'adm' pw ") {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("authenticated admin /user: got %q, want the adm row", configLines(resp))
	}
}

// A refused reqconfig must not end the exchange: the cards around it are
// still served and the reply reports no errors.
func TestReqConfigRefusedNameKeepsExchange(t *testing.T) {
	r := setupSyncTestRepo(t)
	seedReqConfigRows(t, r)
	setNobodyCaps(t, r, "o")
	uuid := storeTestBlob(t, r, []byte("still served"))

	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "x", ProjectCode: testProjectCode(t, r.DB())},
		&xfer.ReqConfigCard{Name: "project-code"},
		&xfer.ReqConfigCard{Name: "/user"},
		&xfer.GimmeCard{UUID: uuid},
	}})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if errs := findCards[*xfer.ErrorCard](resp); len(errs) != 0 {
		t.Fatalf("error cards %v, want none", errs)
	}
	if n := len(findCards[*xfer.FileCard](resp)) + len(findCards[*xfer.CFileCard](resp)); n != 1 {
		t.Fatalf("gimme after refused reqconfig: %d file cards, want 1", n)
	}
	if got := configLines(resp); len(got) != 0 {
		t.Fatalf("refused names got config cards %q", got)
	}
}
