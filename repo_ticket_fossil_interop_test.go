package libfossil

// Tickets written through TicketAdd/TicketSet against a stock fossil binary
// (#503), in the topology the html-artifact-publisher engine runs: this
// library's repository serves sync, and fossil clones, pulls and pushes.

import (
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/testutil"
)

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
	})
	if err != nil {
		t.Fatal(err)
	}
	ticketFossil(t, x.bin, dir, "pull", x.url, "-R", x.clone)

	history := x.fossil(t, "ticket", "history", uuid)
	requireLines(t, "fossil ticket history", history,
		"Ticket Change by bob on", "Ticket Change by editor on", "Change status: Closed",
		"Change resolution: Fixed", "Change icomment: fixed in the next version")
	if strings.Index(history, "by bob") > strings.Index(history, "by editor") {
		t.Fatalf("fossil lists the amendment before the ticket:\n%s", history)
	}
	show := x.fossil(t, "ticket", "show", "0", "tkt_uuid='"+uuid+"'")
	requireLines(t, "fossil ticket show", show, "Closed", "Fixed")
	if got := strings.TrimSpace(x.fossil(t, "sql",
		"SELECT count(*) FROM blob WHERE uuid='"+artifact+"'")); got != "1" {
		t.Fatalf("clone holds amendment %s %s times, want 1", artifact, got)
	}
}
