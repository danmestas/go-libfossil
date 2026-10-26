package manifest

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// ticketWriteCase is one `fossil ticket add|set` invocation and the
// TicketWrite that must produce the same artifact bytes.
type ticketWriteCase struct {
	name   string
	set    bool // `ticket set` on the ticket the first case created
	date   string
	user   string
	fields []string // name, value pairs as the fossil command line takes them
}

// TestWriteTicketMatchesFossilBytes checks the artifact WriteTicket stores
// against the one stock `fossil ticket add` / `ticket set` writes for the same
// ticket, fields, user and time: the bytes, and so the hash, must be equal.
// The fields cover a value fossil must escape (spaces, a newline, a
// backslash), an append card, and the remark fields the stock web forms
// write.
func TestWriteTicketMatchesFossilBytes(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	fossilRepo := filepath.Join(dir, "f.fossil")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("fossil %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("init", fossilRepo, "-A", "alice")
	run("user", "new", "bob", "Bob", "pw", "-R", fossilRepo)
	run("user", "default", "alice", "-R", fossilRepo)

	r, err := repo.Create(filepath.Join(dir, "g.fossil"), "alice", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const tkt = "0123456789abcdef0123456789abcdef01234567"
	cases := []ticketWriteCase{
		{name: "add", date: "2026-01-02T03:04:05.678", user: "alice", fields: []string{
			"title", "two words\nand a \\ backslash", "status", "Open", "type", "Code_Defect",
			"icomment", "first remark", "login", "alice", "username", "alice",
			"mimetype", "text/plain",
		}},
		{name: "set", set: true, date: "2026-01-02T03:04:06.000", user: "bob", fields: []string{
			"status", "Closed", "resolution", "Fixed", "+comment", " appended",
		}},
	}
	for _, c := range cases {
		args := []string{"ticket", "add"}
		if c.set {
			args = []string{"ticket", "set", tkt}
		}
		args = append(args, c.fields...)
		args = append(args, "--date-override", c.date, "--user-override", c.user,
			"--uuid-override", tkt, "-R", fossilRepo)
		run(args...)
		hash := strings.TrimSpace(run("sql", "-R", fossilRepo,
			"SELECT uuid FROM blob WHERE rid=(SELECT max(rid) FROM blob)"))
		hash = strings.Trim(hash, "'")
		want := run("artifact", hash, "-R", fossilRepo)
		if got := writeTicketCase(t, r, tkt, c); got != want {
			t.Fatalf("%s: artifact differs from fossil's\nfossil:\n%s\nours:\n%s",
				c.name, want, got)
		}
	}
	assertFossilRederivesTicketRows(t, bin, r.Path())
}

// writeTicketCase writes c to ticket tkt with WriteTicket and returns the
// stored artifact.
func writeTicketCase(t *testing.T, r *repo.Repo, tkt string, c ticketWriteCase) string {
	t.Helper()
	when, err := time.Parse("2006-01-02T15:04:05.000", c.date)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for i := 0; i < len(c.fields); i += 2 {
		fields[c.fields[i]] = c.fields[i+1]
	}
	rid, _, err := WriteTicket(r, TicketWrite{UUID: tkt, Fields: fields, User: c.user, Time: when})
	if err != nil {
		t.Fatalf("%s: WriteTicket: %v", c.name, err)
	}
	got, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// assertFossilRederivesTicketRows checks the rows WriteTicket derived as it
// stored each artifact against what `fossil rebuild` derives from the same
// blobs.
func assertFossilRederivesTicketRows(t *testing.T, bin, path string) {
	t.Helper()
	ours := snapshotTicketDerived(t, path)
	if ours["count"] == "0" {
		t.Fatal("WriteTicket derived no ticket events")
	}
	if out, err := exec.Command(bin, "rebuild", path).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild: %v\n%s", err, out)
	}
	reference := snapshotTicketDerived(t, path)
	for _, key := range []string{"count", "event", "ticket", "ticketchng"} {
		if ours[key] != reference[key] {
			t.Errorf("%s differs from what fossil derived\n fossil: %s\n ours:   %s",
				key, reference[key], ours[key])
		}
	}
}
