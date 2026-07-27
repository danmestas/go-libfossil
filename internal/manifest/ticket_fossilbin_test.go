package manifest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

// ticketDerivedTables are the tables a ticket artifact is responsible for,
// added to the ones every crosslink writes. Emptying all of them puts the
// repository back in a freshly-transferred clone's pre-crosslink state.
var ticketDerivedTables = append(append([]string{}, crosslinkDerivedTables...), "ticket", "ticketchng")

// TestFossilBinaryTicketParity is the acceptance check for issue #184: the
// rows Crosslink derives for a corpus of real ticket artifacts must be the
// rows canonical fossil derives from the same blobs.
//
// The corpus is built by the fossil binary itself -- `ticket add` and `ticket
// change` emit the D,J,K,U,Z card shape that was 100% lost -- so neither the
// artifacts nor the expected output is anything this repository invented.
// Every branch of the event comment fossil can produce is exercised: the
// creating artifact, a change that sets a status, a change that sets a status
// plus other fields, and a change that sets no status at all.
func TestFossilBinaryTicketParity(t *testing.T) {
	bin := testutil.RequireFossilBin(t)

	dir := t.TempDir()
	repoPath := filepath.Join(dir, "tickets.fossil")
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	runIn := func(wd string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = wd
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fossil %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	runIn(dir, "init", repoPath, "-A", "alice")
	runIn(work, "open", repoPath, "--user", "alice")
	runIn(work, "user", "new", "bob", "Bob", "secret", "--user", "alice")

	// A check-in so the repository is not ticket-only: the sweep must reach
	// the ticket artifacts alongside everything else it links.
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	runIn(work, "add", "a.txt")
	runIn(work, "commit", "-m", "c1", "--user", "alice")

	// Ticket 1: a title carrying every character fossil's %h conversion
	// escapes, so a comment built with the wrong escaper cannot pass.
	out := runIn(work, "ticket", "add", "title", `A <b>&"quoted"</b> it's title`,
		"status", "Open", "type", "Code_Defect", "comment", "first report", "--user", "alice")
	tkt1 := lastField(t, out)
	runIn(work, "ticket", "change", tkt1, "status", "Closed", "--user", "bob")
	runIn(work, "ticket", "change", tkt1, "priority", "High", "--user", "bob")
	runIn(work, "ticket", "change", tkt1, "status", "Verified",
		"resolution", "Fixed", "foundin", "1.0", "--user", "alice")
	runIn(work, "ticket", "change", tkt1, "severity", "Critical", "subsystem", "net", "--user", "bob")

	// Ticket 2 proves the per-ticket rebuild does not bleed across tickets.
	// The comment is seeded here so the '+comment' change below has something
	// to append to: appending to an absent value is indistinguishable from
	// assigning it.
	out = runIn(work, "ticket", "add", "title", "Second bug", "status", "Open",
		"type", "Feature_Request", "comment", "initial note", "--user", "bob")
	tkt2 := lastField(t, out)
	runIn(work, "ticket", "change", tkt2, "icomment", "a follow-up note", "--user", "alice")
	// A '+field' card appends to the stored value instead of replacing it, and
	// is the one J-card shape whose ticket-table write is not a plain
	// assignment. Without it the append branch would go unexercised.
	runIn(work, "ticket", "change", tkt2, "+comment", " and then more", "--user", "bob")

	wantTicketArtifacts := 8

	// Put the repository back in its pre-crosslink state and re-derive.
	prep, err := db.Open(repoPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	for _, tbl := range ticketDerivedTables {
		var exists int
		if err := prep.QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", tbl, err)
		}
		if exists == 0 {
			continue
		}
		if _, err := prep.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := prep.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	got := snapshotTicketDerived(t, repoPath)

	// Canonical fossil re-derives the same tables from the same blobs.
	if out, err := exec.Command(bin, "rebuild", repoPath).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}
	reference := snapshotTicketDerived(t, repoPath)

	if reference["count"] != strconv.Itoa(wantTicketArtifacts) {
		t.Fatalf("canonical fossil derived %s ticket events, but the corpus has %d ticket artifacts; "+
			"the fixture no longer means what this test assumes",
			reference["count"], wantTicketArtifacts)
	}
	for _, key := range []string{"count", "event", "ticket", "ticketchng"} {
		if got[key] != reference[key] {
			t.Errorf("%s differs from what fossil derived\n fossil:    %s\n crosslink: %s",
				key, reference[key], got[key])
		}
	}
}

// lastField returns the final whitespace-separated token of a fossil command's
// output, which for `ticket add` is the new ticket's uuid.
func lastField(t *testing.T, out string) string {
	t.Helper()
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		t.Fatalf("fossil ticket add printed nothing to take a uuid from")
	}
	id := fields[len(fields)-1]
	if len(id) != 40 {
		t.Fatalf("fossil ticket add did not end in a ticket uuid: %q", out)
	}
	return id
}

// scalarString renders one digest column, which arrives as a string from
// group_concat and as an integer from count(*) depending on the driver.
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return fmt.Sprint(t)
	}
}

// snapshotTicketDerived digests every row a ticket artifact is responsible
// for. event.mtime is deliberately excluded: fossil round-trips it through a
// %.17g decimal literal, so the two derivations differ in the last unit in the
// last place of a double. Every other column is compared literally, tagid via
// the tag name it resolves to so the comparison does not depend on two
// independent derivations assigning the same autoincrement id.
func snapshotTicketDerived(t *testing.T, path string) map[string]string {
	t.Helper()

	queries := map[string]string{
		"count": `SELECT count(*) FROM event WHERE type='t'`,
		"event": `SELECT group_concat(v, '|') FROM
		            (SELECT objid || ':' || coalesce((SELECT tagname FROM tag WHERE tagid=e.tagid),'') ||
		                    ':' || coalesce(user,'') || ':' || coalesce(comment,'') ||
		                    ':' || coalesce(brief,'') AS v
		               FROM event e WHERE type='t' ORDER BY objid)`,
		"ticket": `SELECT group_concat(v, '|') FROM
		            (SELECT tkt_uuid || ':' || coalesce(type,'') || ':' || coalesce(status,'') || ':' ||
		                    coalesce(subsystem,'') || ':' || coalesce(priority,'') || ':' ||
		                    coalesce(severity,'') || ':' || coalesce(foundin,'') || ':' ||
		                    coalesce(resolution,'') || ':' || coalesce(title,'') || ':' ||
		                    coalesce(comment,'') AS v
		               FROM ticket ORDER BY tkt_uuid)`,
		"ticketchng": `SELECT group_concat(v, '|') FROM
		            (SELECT tkt_rid || ':' || coalesce(tkt_user,'') || ':' || coalesce(login,'') || ':' ||
		                    coalesce(username,'') || ':' || coalesce(mimetype,'') || ':' ||
		                    coalesce(icomment,'') AS v
		               FROM ticketchng ORDER BY tkt_rid)`,
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("snapshotTicketDerived open: %v", err)
	}
	defer d.Close()

	out := make(map[string]string, len(queries))
	for name, q := range queries {
		var v any
		if err := d.QueryRow(q).Scan(&v); err != nil {
			t.Fatalf("snapshotTicketDerived %s: %v", name, err)
		}
		s := scalarString(v)
		if s == "" || s == "0" {
			t.Fatalf("snapshotTicketDerived %s: empty digest; the table has no rows "+
				"and the comparison would be vacuous", name)
		}
		out[name] = s
	}
	return out
}
