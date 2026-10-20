package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

// TestFossilBinaryTicketOptionalSchema checks the ticket tables a repository
// may configure for itself through its ticket-table setting. Fossil's
// getAllTicketFields treats the TICKET table, TICKET.tkt_ctime, the TICKETCHNG
// table, TICKETCHNG.tkt_rid and TICKETCHNG.tkt_user as optional, and fossil's
// own repository has no tkt_user. For each schema below the rows Crosslink
// derives must be the rows `fossil rebuild` derives from the same blobs.
func TestFossilBinaryTicketOptionalSchema(t *testing.T) {
	bin := testutil.RequireFossilBin(t)

	const ticketCore = `CREATE TABLE ticket(
  tkt_id INTEGER PRIMARY KEY,
  tkt_uuid TEXT UNIQUE,
  tkt_mtime DATE,
  tkt_ctime DATE,
  type TEXT, status TEXT, subsystem TEXT, priority TEXT, severity TEXT,
  foundin TEXT, private_contact TEXT, resolution TEXT, title TEXT, comment TEXT
);`
	const ticketNoCTime = `CREATE TABLE ticket(
  tkt_id INTEGER PRIMARY KEY,
  tkt_uuid TEXT UNIQUE,
  tkt_mtime DATE,
  type TEXT, status TEXT, subsystem TEXT, priority TEXT, severity TEXT,
  foundin TEXT, private_contact TEXT, resolution TEXT, title TEXT, comment TEXT
);`

	schemas := []struct {
		name  string
		table string
	}{
		{"no tkt_user", ticketCore + `
CREATE TABLE ticketchng(
  tkt_id INTEGER REFERENCES ticket, tkt_rid INTEGER REFERENCES blob, tkt_mtime DATE,
  login TEXT, username TEXT, mimetype TEXT, icomment TEXT
);`},
		// With neither bookkeeping column, a change row is written only for
		// an artifact that sets a ticketchng field.
		{"no tkt_rid", ticketCore + `
CREATE TABLE ticketchng(
  tkt_id INTEGER REFERENCES ticket, tkt_mtime DATE, icomment TEXT
);`},
		{"no tkt_ctime", ticketNoCTime + `
CREATE TABLE ticketchng(
  tkt_id INTEGER REFERENCES ticket, tkt_rid INTEGER REFERENCES blob, tkt_mtime DATE,
  tkt_user TEXT, login TEXT, username TEXT, mimetype TEXT, icomment TEXT
);`},
		{"no ticketchng", ticketCore},
		{"no ticket", `
CREATE TABLE ticketchng(
  tkt_id INTEGER, tkt_rid INTEGER REFERENCES blob, tkt_mtime DATE,
  tkt_user TEXT, icomment TEXT
);`},
		// status lives in both tables, so a change row copies it from the
		// ticket row when the artifact does not set it; title has a baseline
		// column recording the rid that last assigned it.
		{"shared field and baseline", `CREATE TABLE ticket(
  tkt_id INTEGER PRIMARY KEY,
  tkt_uuid TEXT UNIQUE,
  tkt_mtime DATE,
  tkt_ctime DATE,
  type TEXT, status TEXT, priority TEXT, resolution TEXT, title TEXT, comment TEXT,
  "baseline for title" INTEGER
);
CREATE TABLE ticketchng(
  tkt_id INTEGER REFERENCES ticket, tkt_rid INTEGER REFERENCES blob, tkt_mtime DATE,
  tkt_user TEXT, status TEXT, icomment TEXT
);`},
	}

	for _, s := range schemas {
		t.Run(s.name, func(t *testing.T) {
			repoPath := buildTicketCorpus(t, bin)

			setConfig(t, repoPath, "ticket-table", s.table)
			if out, err := exec.Command(bin, "rebuild", repoPath).CombinedOutput(); err != nil {
				t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
			}
			reference := snapshotTicketTables(t, repoPath)

			clearTicketDerived(t, repoPath)
			r, err := repo.Open(repoPath)
			if err != nil {
				t.Fatalf("repo.Open: %v", err)
			}
			if _, err := Crosslink(r); err != nil {
				r.Close()
				t.Fatalf("Crosslink: %v", err)
			}
			if err := r.Close(); err != nil {
				t.Fatalf("repo.Close: %v", err)
			}
			got := snapshotTicketTables(t, repoPath)

			for _, key := range []string{"event", "ticket", "ticketchng"} {
				if got[key] != reference[key] {
					t.Errorf("%s differs from what fossil derived\n fossil:    %s\n crosslink: %s",
						key, reference[key], got[key])
				}
			}
		})
	}
}

// buildTicketCorpus creates a repository under the default ticket schema and
// fills it with ticket artifacts made by the fossil binary: creations, status
// and non-status changes, a ticketchng-only change, appends to a field never
// set, to a field with a baseline and to a field stored in both tables.
func buildTicketCorpus(t *testing.T, bin string) string {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "tickets.fossil")
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	runIn := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = work
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fossil %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	runIn("init", repoPath, "-A", "alice")
	runIn("open", repoPath, "--user", "alice")
	runIn("user", "new", "bob", "Bob", "secret", "--user", "alice")

	tkt1 := lastField(t, runIn("ticket", "add", "title", "First bug", "status", "Open",
		"type", "Code_Defect", "comment", "first report", "--user", "alice"))
	runIn("ticket", "change", tkt1, "status", "Closed", "--user", "bob")
	runIn("ticket", "change", tkt1, "priority", "High", "--user", "bob")
	runIn("ticket", "change", tkt1, "icomment", "a follow-up note", "--user", "alice")
	runIn("ticket", "change", tkt1, "title", "First bug, renamed", "--user", "alice")
	// An append to a field no artifact has set must start from an empty
	// string, not NULL; an append to a field with a baseline leaves the
	// baseline at the rid that last assigned it; status lives in both tables
	// in one of the schemas.
	runIn("ticket", "change", tkt1, "+subsystem", "kernel", "--user", "bob")
	runIn("ticket", "change", tkt1, "+title", " (appended)", "--user", "alice")
	runIn("ticket", "change", tkt1, "+status", "-ish", "--user", "bob")

	tkt2 := lastField(t, runIn("ticket", "add", "title", "Second bug", "status", "Open",
		"comment", "initial note", "--user", "bob"))
	runIn("ticket", "change", tkt2, "+comment", " and then more", "--user", "bob")
	return repoPath
}

func setConfig(t *testing.T, repoPath, name, value string) {
	t.Helper()
	d, err := db.Open(repoPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	if _, err := d.Exec(
		"REPLACE INTO config(name, value, mtime) VALUES(?, ?, strftime('%s','now'))", name, value,
	); err != nil {
		t.Fatalf("set config %s: %v", name, err)
	}
}

// clearTicketDerived empties every derived table that exists, returning the
// repository to its pre-crosslink state.
func clearTicketDerived(t *testing.T, repoPath string) {
	t.Helper()
	d, err := db.Open(repoPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	for _, tbl := range ticketDerivedTables {
		if !tableExists(t, d, tbl) {
			continue
		}
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
}

func tableExists(t *testing.T, d *db.DB, name string) bool {
	t.Helper()
	var n int
	if err := d.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name,
	).Scan(&n); err != nil {
		t.Fatalf("check %s: %v", name, err)
	}
	return n > 0
}

// snapshotTicketTables digests every column of whichever ticket tables the
// schema has, plus the ticket event rows. tkt_id is an autoincrement two
// derivations need not agree on, so ticket rows omit it and ticketchng rows
// resolve it to the ticket's uuid. event.mtime is omitted for the reason
// snapshotTicketDerived gives.
func snapshotTicketTables(t *testing.T, path string) map[string]string {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("snapshotTicketTables open: %v", err)
	}
	defer d.Close()

	digest := func(table, idExpr string) string {
		if !tableExists(t, d, table) {
			return "<absent>"
		}
		rows, err := d.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		if err != nil {
			t.Fatalf("columns %s: %v", table, err)
		}
		var exprs []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("columns %s scan: %v", table, err)
			}
			if name == "tkt_id" {
				if idExpr != "" {
					exprs = append(exprs, idExpr)
				}
				continue
			}
			exprs = append(exprs, `quote("`+name+`")`)
		}
		rows.Close()
		q := "SELECT coalesce(group_concat(v, '|'), '') FROM (SELECT " +
			strings.Join(exprs, " || ':' || ") + " AS v FROM " + table + " c ORDER BY v)"
		var s string
		if err := d.QueryRow(q).Scan(&s); err != nil {
			t.Fatalf("digest %s: %v", table, err)
		}
		return s
	}

	chngID := "quote(c.tkt_id)"
	if tableExists(t, d, "ticket") {
		chngID = "quote((SELECT tkt_uuid FROM ticket WHERE tkt_id=c.tkt_id))"
	}
	out := map[string]string{
		"ticket":     digest("ticket", ""),
		"ticketchng": digest("ticketchng", chngID),
	}
	var ev string
	if err := d.QueryRow(`SELECT coalesce(group_concat(v, '|'), '') FROM
	    (SELECT objid || ':' || coalesce((SELECT tagname FROM tag WHERE tagid=e.tagid),'') ||
	            ':' || coalesce(user,'') || ':' || coalesce(comment,'') ||
	            ':' || coalesce(brief,'') AS v
	       FROM event e WHERE type='t' ORDER BY objid)`).Scan(&ev); err != nil {
		t.Fatalf("digest event: %v", err)
	}
	out["event"] = ev
	return out
}
