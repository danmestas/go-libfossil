package libfossil

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/testutil"
)

// ticketT0 is after the initial check-in Create makes at the current time, so
// every ticket in these tests is newer than every check-in.
var ticketT0 = time.Now().UTC().Truncate(time.Second).Add(time.Hour)

// sameMs compares times at the millisecond a D card records.
func sameMs(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -time.Millisecond && d < time.Millisecond
}

func newTicketRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "t.fossil"), CreateOpts{User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func mustTicketAdd(t *testing.T, r *Repo, opts TicketOpts) (string, string) {
	t.Helper()
	uuid, artifact, err := r.TicketAdd(opts)
	if err != nil {
		t.Fatalf("TicketAdd: %v", err)
	}
	if len(uuid) != 40 || strings.Trim(uuid, "0123456789abcdef") != "" {
		t.Fatalf("ticket uuid %q is not 40 lowercase hex digits", uuid)
	}
	if !isHex(artifact) || (len(artifact) != 40 && len(artifact) != 64) {
		t.Fatalf("artifact hash %q is not a full hash", artifact)
	}
	return uuid, artifact
}

// Issue #503's Go acceptance: two tickets added, the first amended; the
// ticket table and the change list must reflect all three artifacts at once,
// with no separate rebuild step.
func TestTicketAddSetCountStatusesOrder(t *testing.T) {
	r := newTicketRepo(t)
	first, art1 := mustTicketAdd(t, r, TicketOpts{
		Fields:  map[string]string{"title": "first", "status": "Open", "type": "Code_Defect"},
		User:    "alice",
		Comment: "first comment",
		Time:    ticketT0,
	})
	second, _ := mustTicketAdd(t, r, TicketOpts{
		Fields: map[string]string{"title": "second", "status": "Open"},
		User:   "bob",
		Time:   ticketT0.Add(time.Second),
	})
	art3, err := r.TicketSet(first, TicketOpts{
		Fields:  map[string]string{"status": "Closed", "resolution": "Fixed"},
		User:    "carol",
		Comment: "done",
		Time:    ticketT0.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("TicketSet: %v", err)
	}

	tickets, err := r.TicketList()
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 2 {
		t.Fatalf("TicketList returned %d tickets, want 2", len(tickets))
	}
	status := map[string]string{}
	for _, tk := range tickets {
		status[tk.UUID] = tk.Status
	}
	if status[first] != "Closed" || status[second] != "Open" {
		t.Fatalf("statuses = %v, want %s Closed and %s Open", status, first, second)
	}
	// Newest-changed first: the amended ticket changed last.
	if tickets[0].UUID != first || !sameMs(tickets[0].Changed, ticketT0.Add(2*time.Second)) {
		t.Fatalf("first listed ticket = %s changed %v, want %s changed %v",
			tickets[0].UUID, tickets[0].Changed, first, ticketT0.Add(2*time.Second))
	}
	if got := tickets[0].Fields["resolution"]; got != "Fixed" {
		t.Fatalf("resolution = %q, want Fixed", got)
	}

	changes, err := r.TicketChanges(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("TicketChanges returned %d changes, want 2", len(changes))
	}
	if changes[0].Artifact != art1 || changes[1].Artifact != art3 {
		t.Fatalf("change order = %s, %s; want %s, %s",
			changes[0].Artifact, changes[1].Artifact, art1, art3)
	}
	if changes[0].User != "alice" || changes[1].User != "carol" {
		t.Fatalf("change users = %q, %q; want alice, carol", changes[0].User, changes[1].User)
	}
	if changes[0].Comment != "first comment" || changes[1].Comment != "done" {
		t.Fatalf("change comments = %q, %q", changes[0].Comment, changes[1].Comment)
	}
	if changes[0].Fields["status"] != "Open" || changes[1].Fields["status"] != "Closed" {
		t.Fatalf("change statuses = %v, %v", changes[0].Fields, changes[1].Fields)
	}
	if !sameMs(changes[1].Time, ticketT0.Add(2*time.Second)) {
		t.Fatalf("second change time = %v", changes[1].Time)
	}
}

// The comment is stored as the stock ticket forms store a remark: icomment
// with the author's login, username and a mimetype, in ticketchng.
func TestTicketCommentIsStockRemark(t *testing.T) {
	r := newTicketRepo(t)
	uuid, _ := mustTicketAdd(t, r, TicketOpts{
		Fields: map[string]string{"title": "x"}, User: "alice", Comment: "hi\nthere",
		Time: ticketT0,
	})
	changes, err := r.TicketChanges(uuid)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"title": "x", "icomment": "hi\nthere", "login": "alice",
		"username": "alice", "mimetype": "text/plain",
	}
	got := changes[0].Fields
	if len(got) != len(want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("field %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	var icomment, login string
	if err := r.DB().QueryRow(
		"SELECT icomment, login FROM ticketchng").Scan(&icomment, &login); err != nil {
		t.Fatal(err)
	}
	if icomment != "hi\nthere" || login != "alice" {
		t.Fatalf("ticketchng row = %q, %q", icomment, login)
	}
}

// A "+name" field appends to the ticket column rather than replacing it.
func TestTicketSetAppendField(t *testing.T) {
	r := newTicketRepo(t)
	uuid, _ := mustTicketAdd(t, r, TicketOpts{
		Fields: map[string]string{"title": "x", "comment": "one"}, User: "alice", Time: ticketT0,
	})
	if _, err := r.TicketSet(uuid[:10], TicketOpts{
		Fields: map[string]string{"+comment": " two"}, User: "alice",
		Time: ticketT0.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	tickets, err := r.TicketList()
	if err != nil {
		t.Fatal(err)
	}
	if got := tickets[0].Fields["comment"]; got != "one two" {
		t.Fatalf("comment = %q, want %q", got, "one two")
	}
	changes, err := r.TicketChanges(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if got := changes[1].Fields["+comment"]; got != " two" {
		t.Fatalf("append card = %v", changes[1].Fields)
	}
}

func TestTicketWriteRejectsBadInput(t *testing.T) {
	r := newTicketRepo(t)
	uuid, _ := mustTicketAdd(t, r, TicketOpts{
		Fields: map[string]string{"title": "x"}, User: "alice", Time: ticketT0,
	})
	cases := map[string]TicketOpts{
		"unknown field":    {Fields: map[string]string{"nosuch": "v"}, User: "alice"},
		"no user":          {Fields: map[string]string{"title": "v"}},
		"nothing to write": {Fields: map[string]string{"title": ""}, User: "alice"},
		"set and append": {
			Fields: map[string]string{"comment": "a", "+comment": "b"}, User: "alice",
		},
		"comment collides": {
			Fields: map[string]string{"icomment": "a"}, User: "alice", Comment: "b",
		},
		"private field": {Fields: map[string]string{"private_contact": "a@b"}, User: "alice"},
	}
	for name, opts := range cases {
		if _, _, err := r.TicketAdd(opts); err == nil {
			t.Errorf("TicketAdd %s: no error", name)
		}
	}
	if _, err := r.TicketSet("0123456789", TicketOpts{
		Fields: map[string]string{"status": "Closed"}, User: "alice",
	}); err == nil {
		t.Error("TicketSet on an unknown ticket: no error")
	}
	if _, err := r.TicketSet(uuid, TicketOpts{User: "alice"}); err == nil {
		t.Error("TicketSet with nothing to write: no error")
	}
	if _, err := r.TicketChanges("0123456789"); err == nil {
		t.Error("TicketChanges on an unknown ticket: no error")
	}
}

// threeTickets commits a check-in, adds three tickets after it and replies
// to the second.
func threeTickets(t *testing.T, r *Repo) (checkin string, uuids, artifacts []string, reply string) {
	t.Helper()
	_, checkin, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "index.html", Content: []byte("<p>hi</p>")}},
		Comment: "publish", User: "alice", Time: ticketT0,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, title := range []string{"one", "two", "three"} {
		u, a := mustTicketAdd(t, r, TicketOpts{
			Fields:  map[string]string{"title": title, "status": "Open"},
			User:    "reader",
			Comment: "comment " + title,
			Time:    ticketT0.Add(time.Duration(i+1) * time.Minute),
		})
		uuids, artifacts = append(uuids, u), append(artifacts, a)
	}
	reply, err = r.TicketSet(uuids[1], TicketOpts{
		User: "editor", Comment: "reply @" + checkin[:10], Time: ticketT0.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return checkin, uuids, artifacts, reply
}

// Issue #504's acceptance: a repository with a check-in, three tickets and
// one reply, read through all three queries.
func TestTicketReadsAndTimelineSince(t *testing.T) {
	r := newTicketRepo(t)
	checkin, uuids, artifacts, reply := threeTickets(t, r)

	tickets, err := r.TicketList()
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 3 || tickets[0].UUID != uuids[1] {
		t.Fatalf("TicketList = %d tickets, first %v; want 3, first %s",
			len(tickets), tickets, uuids[1])
	}
	changes, err := r.TicketChanges(uuids[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[1].Artifact != reply || changes[1].User != "editor" ||
		changes[1].Comment != "reply @"+checkin[:10] {
		t.Fatalf("TicketChanges = %+v", changes)
	}

	since, err := r.TimelineSince(checkin, EventKindTicket)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{reply, artifacts[2], artifacts[1], artifacts[0]}
	if len(since) != len(want) {
		t.Fatalf("TimelineSince(checkin, t) = %d entries, want %d", len(since), len(want))
	}
	for i, e := range since {
		if e.UUID != want[i] || e.Kind != EventKindTicket {
			t.Fatalf("entry %d = %s %s, want %s t", i, e.UUID, e.Kind, want[i])
		}
	}
	all, err := r.TimelineSince(checkin)
	if err != nil || len(all) != 4 {
		t.Fatalf("TimelineSince(checkin) = %d entries, %v; want 4", len(all), err)
	}
	checkins, err := r.TimelineSince(checkin, EventKindCheckin, EventKindWiki, EventKindForum)
	if err != nil || len(checkins) != 0 {
		t.Fatalf("TimelineSince(checkin, ci w f) = %v, %v; want none", checkins, err)
	}
	atTip, err := r.TimelineSince(reply)
	if err != nil {
		t.Fatal(err)
	}
	if atTip == nil || len(atTip) != 0 {
		t.Fatalf("TimelineSince at the tip = %#v, want an empty, non-nil slice", atTip)
	}
	older, err := r.TimelineSince(artifacts[2], EventKindTicket)
	if err != nil || len(older) != 1 || older[0].UUID != reply {
		t.Fatalf("TimelineSince(third ticket) = %v, %v; want only the reply", older, err)
	}

	assertTicketJSON(t, tickets, changes, atTip)
}

// The results cross the wasm boundary as JSON unchanged, so their encoding is
// part of the contract: lower-case keys, and [] rather than null when empty.
func assertTicketJSON(
	t *testing.T, tickets []Ticket, changes []TicketChange, empty []TimelineEntry,
) {
	t.Helper()
	b, err := json.Marshal(tickets[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"uuid":`, `"status":"Open"`, `"changed":`, `"fields":{`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("ticket JSON %s lacks %s", b, key)
		}
	}
	b, err = json.Marshal(changes[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"artifact":`, `"user":"editor"`, `"time":`, `"fields":{`, `"comment":"reply @`,
	} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("change JSON %s lacks %s", b, key)
		}
	}
	if b, _ := json.Marshal(empty); string(b) != "[]" {
		t.Fatalf("empty timeline JSON = %s, want []", b)
	}
}

// TimelineSince goes by arrival, not timestamp: a ticket stamped before the
// check-in but stored after it is returned, and one stamped after the
// check-in but stored before it is not.
func TestTimelineSinceIsArrivalOrder(t *testing.T) {
	r := newTicketRepo(t)
	_, early := mustTicketAdd(t, r, TicketOpts{
		Fields: map[string]string{"title": "stored first, stamped late"}, User: "a",
		Time: ticketT0.Add(time.Hour),
	})
	_, checkin, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "a", Content: []byte("a")}},
		Comment: "c", User: "alice", Time: ticketT0,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, late := mustTicketAdd(t, r, TicketOpts{
		Fields: map[string]string{"title": "stored last, stamped early"}, User: "b",
		Time: ticketT0.Add(-30 * time.Minute),
	})
	got, err := r.TimelineSince(checkin, EventKindTicket)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UUID != late {
		t.Fatalf("TimelineSince(checkin, t) = %v; want only %s, not %s", got, late, early)
	}
	if atTip, err := r.TimelineSince(late); err != nil || atTip == nil || len(atTip) != 0 {
		t.Fatalf("TimelineSince at the last arrival = %#v, %v; want empty", atTip, err)
	}
}

func TestTimelineSinceRejectsBadInput(t *testing.T) {
	r := newTicketRepo(t)
	_, checkin, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "a", Content: []byte("a")}},
		Comment: "c", User: "alice", Time: ticketT0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.TimelineSince(strings.Repeat("0", 40)); err == nil {
		t.Error("TimelineSince of an unknown hash: no error")
	}
	if _, err := r.TimelineSince(checkin, EventKind("x")); err == nil {
		t.Error("TimelineSince with an unknown kind: no error")
	}
}

// Tickets written through TicketAdd/TicketSet against a stock fossil binary
// (#503), in the topology the html-artifact-publisher engine runs: this
// library's repository serves sync, and fossil clones, pulls and pushes.

type ticketInterop struct {
	bin, clone, url string
	repo            *Repo
}

func newTicketInterop(t *testing.T) *ticketInterop {
	t.Helper()
	bin := testutil.RequireFossilBin(t)
	r := newTicketRepo(t)
	if err := r.CreateUser(UserOpts{Login: "bob", Password: "pw", Caps: "gio"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.XferHandler())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("bob", "pw")
	return &ticketInterop{
		bin: bin, clone: filepath.Join(t.TempDir(), "clone.fossil"), url: u.String(), repo: r,
	}
}

func (x *ticketInterop) fossil(t *testing.T, args ...string) string {
	t.Helper()
	return ticketFossil(t, x.bin, filepath.Dir(x.clone), append(args, "-R", x.clone)...)
}

// ticketFossil runs the fossil binary in dir and fails the test on error.
func ticketFossil(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func requireLines(t *testing.T, what, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("%s lacks %q:\n%s", what, w, out)
		}
	}
}

// A ticket written here and cloned by stock fossil shows in `fossil ticket
// show` with the same fields, and in `fossil ticket history` with the same
// author and remark.
func TestTicketWrittenHereReadsInFossil(t *testing.T) {
	x := newTicketInterop(t)
	uuid, artifact, err := x.repo.TicketAdd(TicketOpts{
		Fields:  map[string]string{"title": "Broken link", "status": "Open", "type": "Code_Defect"},
		User:    "reader",
		Comment: "the second link 404s",
		Time:    ticketT0,
	})
	if err != nil {
		t.Fatal(err)
	}
	ticketFossil(t, x.bin, filepath.Dir(x.clone), "clone", x.url, x.clone)

	show := x.fossil(t, "ticket", "show", "0", "tkt_uuid='"+uuid+"'")
	requireLines(t, "fossil ticket show", show, uuid, "Broken link", "Open", "Code_Defect")
	history := x.fossil(t, "ticket", "history", uuid)
	requireLines(t, "fossil ticket history", history,
		"Ticket Change by reader on", "Change title: Broken link", "Change status: Open",
		"Change icomment: the second link 404s", "Change login: reader")
	if got := strings.TrimSpace(x.fossil(t, "sql",
		"SELECT count(*) FROM blob WHERE uuid='"+artifact+"'")); got != "1" {
		t.Fatalf("clone holds artifact %s %s times, want 1", artifact, got)
	}
}

// A ticket made by stock fossil and pushed here can be amended with
// TicketSet, and the amendment pulled back shows in `fossil ticket history`.
func TestFossilTicketAmendedHereShowsInHistory(t *testing.T) {
	x := newTicketInterop(t)
	dir := filepath.Dir(x.clone)
	ticketFossil(t, x.bin, dir, "clone", x.url, x.clone)
	x.fossil(t, "user", "default", "bob")
	out := x.fossil(t, "ticket", "add", "title", "From a terminal", "status", "Open")
	fields := strings.Fields(out)
	uuid := fields[len(fields)-1]
	ticketFossil(t, x.bin, dir, "push", x.url, "-R", x.clone)

	changes, err := x.repo.TicketChanges(uuid)
	if err != nil {
		t.Fatalf("pushed ticket is not readable here: %v", err)
	}
	if len(changes) != 1 || changes[0].User != "bob" {
		t.Fatalf("pushed ticket changes = %+v", changes)
	}
	artifact, err := x.repo.TicketSet(uuid, TicketOpts{
		Fields: map[string]string{"status": "Closed", "resolution": "Fixed"},
		User:   "editor", Comment: "fixed in the next version",
		// A minute on, so the two changes never share a second: fossil
		// orders history by time alone and leaves a tie unordered.
		Time: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	ticketFossil(t, x.bin, dir, "pull", x.url, "-R", x.clone)

	history := x.fossil(t, "ticket", "history", uuid)
	requireLines(t, "fossil ticket history", history,
		"Ticket Change by bob on", "Ticket Change by editor on", "Change status: Closed",
		"Change resolution: Fixed", "Change icomment: fixed in the next version")
	// `fossil ticket history` lists the newest change first.
	if strings.Index(history, "by editor") > strings.Index(history, "by bob") {
		t.Fatalf("fossil lists the ticket before its later amendment:\n%s", history)
	}
	show := x.fossil(t, "ticket", "show", "0", "tkt_uuid='"+uuid+"'")
	requireLines(t, "fossil ticket show", show, "Closed", "Fixed")
	if got := strings.TrimSpace(x.fossil(t, "sql",
		"SELECT count(*) FROM blob WHERE uuid='"+artifact+"'")); got != "1" {
		t.Fatalf("clone holds amendment %s %s times, want 1", artifact, got)
	}
}
