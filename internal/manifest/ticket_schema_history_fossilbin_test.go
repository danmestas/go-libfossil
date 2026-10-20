package manifest

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

// ticketSchemas are ticket table definitions fossil accepts as a
// repository's ticket-table setting, each lacking columns the default
// schema has: fossil probes for tkt_ctime, tkt_rid, tkt_user and
// "baseline for" columns and copies a field both tables share from the
// ticket row when an artifact leaves it unset (src/tkt.c, ticket_insert).
// The first is shaped like fossil's own repository, whose ticketchng has
// neither tkt_rid nor tkt_user.
var ticketSchemas = map[string]string{
	"no rid, user or ctime; shared field; baseline": `
		CREATE TABLE repository.ticket(tkt_id INTEGER PRIMARY KEY, tkt_uuid TEXT UNIQUE,
		  tkt_mtime DATE, type TEXT, status TEXT, title TEXT, comment TEXT,
		  "baseline for title" INTEGER);
		CREATE TABLE repository.ticketchng(tkt_id INTEGER REFERENCES ticket,
		  tkt_mtime DATE, status TEXT, icomment TEXT);`,
	"rid only, no fields": `
		CREATE TABLE repository.ticket(tkt_id INTEGER PRIMARY KEY, tkt_uuid TEXT UNIQUE,
		  tkt_mtime DATE, tkt_ctime DATE, type TEXT, status TEXT, title TEXT, comment TEXT);
		CREATE TABLE repository.ticketchng(tkt_id INTEGER REFERENCES ticket,
		  tkt_rid INTEGER, tkt_mtime DATE);`,
	"user only, no rid or ctime": `
		CREATE TABLE repository.ticket(tkt_id INTEGER PRIMARY KEY, tkt_uuid TEXT UNIQUE,
		  tkt_mtime DATE, type TEXT, status TEXT, title TEXT, comment TEXT);
		CREATE TABLE repository.ticketchng(tkt_id INTEGER REFERENCES ticket, tkt_mtime DATE,
		  tkt_user TEXT, icomment TEXT);`,
}

// TestFossilBinaryTicketSchemaParity derives the ticket tables of a
// repository whose ticket schema lacks optional columns, and requires the
// rows fossil rebuild derives.
func TestFossilBinaryTicketSchemaParity(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	for name, schema := range ticketSchemas {
		t.Run(name, func(t *testing.T) {
			orig := fossilTicketHistory(t, bin, schema)
			mine := filepath.Join(t.TempDir(), "mine.fossil")
			copyRepoFile(t, orig, mine)
			if out, err := exec.Command(bin, "rebuild", orig).CombinedOutput(); err != nil {
				t.Fatalf("fossil rebuild: %v\n%s", err, out)
			}
			clearTicketHistoryDerived(t, mine)
			r, err := repo.Open(mine)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Crosslink(r); err != nil {
				t.Fatalf("Crosslink: %v", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"ticket", "ticketchng"} {
				want, got := tableRows(t, orig, table), tableRows(t, mine, table)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s:\n got  %q\n want %q", table, got, want)
				}
			}
		})
	}
}

// fossilTicketHistory makes a repository with the given ticket schema and
// a ticket created, amended without its shared field, and amended again.
func fossilTicketHistory(t *testing.T, bin, schema string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.fossil")
	work := filepath.Join(dir, "w")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(wd string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = wd
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fossil %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run(dir, "init", path, "-A", "alice")
	run(dir, "sql", "-R", path, "REPLACE INTO config(name, value, mtime) VALUES('ticket-table', '"+
		strings.ReplaceAll(schema, "'", "''")+"', 0)")
	run(dir, "rebuild", path)
	run(work, "open", path, "--user", "alice")
	run(work, "user", "new", "bob", "Bob", "secret", "--user", "alice")
	tkt := lastField(t, run(work, "ticket", "add", "title", "one", "status", "Open", "type", "Bug", "--user", "alice"))
	if strings.Contains(schema, "icomment") {
		run(work, "ticket", "change", tkt, "icomment", "a note", "--user", "bob")
	}
	run(work, "ticket", "change", tkt, "status", "Closed", "title", "two", "--user", "bob")
	run(work, "close", "--force")
	return path
}

func copyRepoFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

func clearTicketHistoryDerived(t *testing.T, path string) {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, tbl := range ticketDerivedTables {
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil && !strings.Contains(err.Error(), "no such table") {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
}

// tableRows renders every row of table, every column quoted, times as
// fossil's julian day to 17 significant digits, ordered by every column.
func tableRows(t *testing.T, path, table string) []string {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query("SELECT name FROM pragma_table_info('" + table + "') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	var exprs []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		q := `"` + col + `"`
		if strings.HasSuffix(col, "time") {
			q = "printf('%!.17g', " + q + ")"
		}
		exprs = append(exprs, "quote("+q+")")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	sel := strings.Join(exprs, " || '|' || ")
	var out []string
	rs, err := d.Query("SELECT " + sel + " FROM " + table + " ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 && table == "ticket" {
		t.Fatalf("%s: no rows; the comparison would be vacuous", table)
	}
	return out
}
