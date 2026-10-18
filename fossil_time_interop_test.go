package libfossil_test

// The times go-libfossil writes and stores, against the fossil binary as the
// oracle: each check is the same operation done by both, at the same time,
// with the stored or written values compared.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/branch"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/testutil"
)

var dCardLine = regexp.MustCompile(`(?m)^D (\S+)$`)

// dCard returns the D-card time of artifact uuid, read by the fossil binary.
func dCard(t *testing.T, bin, repoPath, uuid string) string {
	t.Helper()
	art := fossilRun(t, bin, filepath.Dir(repoPath), "artifact", uuid, "-R", repoPath)
	m := dCardLine.FindStringSubmatch(art)
	if m == nil {
		t.Fatalf("artifact %s has no D card:\n%s", uuid, art)
	}
	return m[1]
}

// newestUUID returns the hash of the newest artifact of event type typ.
func newestUUID(t *testing.T, bin, repoPath, typ string) string {
	t.Helper()
	return strings.TrimSpace(fossilRun(t, bin, filepath.Dir(repoPath), "sql", "-R", repoPath,
		"SELECT uuid FROM blob, event WHERE rid=objid AND type='"+typ+
			"' ORDER BY event.mtime DESC LIMIT 1"))
}

// TestCardTimesMatchFossil makes a check-in, a tag and a branch at the same
// times, each finer than a millisecond, with fossil (--date-override) and
// with go-libfossil, and requires the same D card from both.
func TestCardTimesMatchFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	at := []time.Time{
		time.Date(2030, 3, 4, 12, 34, 56, 789600000, time.UTC), // rounds up
		time.Date(2030, 3, 4, 12, 35, 56, 789499999, time.UTC), // rounds down
		time.Date(2030, 3, 4, 12, 36, 59, 999700000, time.UTC), // held at .999
	}
	override := func(i int) string { return at[i].Format("2006-01-02 15:04:05.000000000") }

	dir := t.TempDir()
	fossilRepo := filepath.Join(dir, "f.fossil")
	ck := filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilRun(t, bin, dir, "init", fossilRepo)
	fossilRun(t, bin, ck, "open", fossilRepo)
	writeFile(t, filepath.Join(ck, "a.txt"), "one\n")
	fossilRun(t, bin, ck, "add", "a.txt")
	fossilRun(t, bin, ck, "commit", "-m", "c", "--date-override", override(0))
	ciUUID := newestUUID(t, bin, fossilRepo, "ci")
	fossilRun(t, bin, ck, "tag", "add", "--date-override", override(1), "rel", "current")
	fossilRun(t, bin, ck, "branch", "new", "feat", "current", "--date-override", override(2))
	want := []string{
		dCard(t, bin, fossilRepo, ciUUID),
		dCard(t, bin, fossilRepo, newestUUID(t, bin, fossilRepo, "g")),
		dCard(t, bin, fossilRepo, newestUUID(t, bin, fossilRepo, "ci")),
	}

	goRepo := filepath.Join(dir, "g.fossil")
	r, err := libfossil.Create(goRepo, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	ciRID, goCI, err := r.Commit(libfossil.CommitOpts{
		Files:   []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("one\n")}},
		Comment: "c", User: "u", Time: at[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	tagRID, err := r.Tag(libfossil.TagOpts{Name: "rel", TargetID: ciRID, User: "u", Time: at[1]})
	if err != nil {
		t.Fatal(err)
	}
	goTag, err := r.UUIDFromRID(tagRID)
	if err != nil {
		t.Fatal(err)
	}
	_, goBranch, err := branch.Create(r.Inner(), branch.CreateOpts{
		Name: "feat", Parent: fsltype.FslID(ciRID), User: "u", Time: at[2],
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	got := []string{
		dCard(t, bin, goRepo, goCI), dCard(t, bin, goRepo, goTag), dCard(t, bin, goRepo, goBranch),
	}
	for i, kind := range []string{"check-in", "tag", "branch"} {
		if got[i] != want[i] {
			t.Errorf("%s at %s: go-libfossil writes D %s, fossil writes D %s",
				kind, override(i), got[i], want[i])
		}
	}
}

// secondsColumn reads a time column fossil keeps in seconds since 1970 and
// requires an integer between before and after, inclusive.
func secondsColumn(t *testing.T, bin, repoPath, what, query string, before, after int64) {
	t.Helper()
	out := strings.TrimSpace(fossilRun(t, bin, filepath.Dir(repoPath), "sql", "-R", repoPath, query))
	var typ string
	var v int64
	if _, err := fmt.Sscanf(out, "%s %d", &typ, &v); err != nil || typ != "integer" {
		t.Errorf("%s in %s: stored %q, want an integer count of seconds", what, filepath.Base(repoPath), out)
		return
	}
	if v < before || v > after {
		t.Errorf("%s in %s: stored %d, want seconds since 1970 in [%d, %d]",
			what, filepath.Base(repoPath), v, before, after)
	}
}

// TestConfigAndUserTimesMatchFossil sets a configuration value, adds a user
// and changes its capabilities and password, with fossil and with
// go-libfossil. Fossil stamps each row with now(), seconds since 1970, so
// config.mtime and user.mtime must hold that in both repositories.
func TestConfigAndUserTimesMatchFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	fossilRepo := filepath.Join(dir, "f.fossil")
	goRepo := filepath.Join(dir, "g.fossil")
	fossilRun(t, bin, dir, "init", fossilRepo)
	r, err := libfossil.Create(goRepo, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// goDo runs fn on the go-libfossil repository, closed again before
	// fossil reads it.
	goDo := func(fn func(r *libfossil.Repo) error) error {
		r, err := libfossil.Open(goRepo)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			r.Close()
			return err
		}
		return r.Close()
	}

	const userTime = "SELECT typeof(mtime)||' '||mtime FROM user WHERE login='bob'"
	const configTime = "SELECT typeof(mtime)||' '||mtime FROM config WHERE name='crlf-glob'"
	const zeroUser = "UPDATE user SET mtime=0 WHERE login='bob'"
	type step struct {
		what   string
		fossil func()
		goside func(r *libfossil.Repo) error
		query  string
	}
	steps := []step{
		{"config.mtime after a setting",
			func() { fossilRun(t, bin, dir, "settings", "crlf-glob", "x", "-R", fossilRepo) },
			func(r *libfossil.Repo) error { return r.SetConfig("crlf-glob", "x") }, configTime},
		{"user.mtime of a new user",
			func() { fossilRun(t, bin, dir, "user", "new", "bob", "", "ei", "pw", "-R", fossilRepo) },
			func(r *libfossil.Repo) error {
				return r.CreateUser(libfossil.UserOpts{Login: "bob", Password: "pw", Caps: "ei"})
			},
			userTime},
		{"user.mtime after a capability change",
			func() {
				fossilRun(t, bin, dir, "sql", "-R", fossilRepo, zeroUser)
				fossilRun(t, bin, dir, "user", "capabilities", "bob", "eio", "-R", fossilRepo)
			},
			func(r *libfossil.Repo) error {
				if _, err := r.DB().Exec(zeroUser); err != nil {
					return err
				}
				return r.SetCaps("bob", "eio")
			}, userTime},
		{"user.mtime after a password change",
			func() {
				fossilRun(t, bin, dir, "sql", "-R", fossilRepo, zeroUser)
				fossilRun(t, bin, dir, "user", "password", "bob", "pw2", "-R", fossilRepo)
			},
			func(r *libfossil.Repo) error {
				if _, err := r.DB().Exec(zeroUser); err != nil {
					return err
				}
				return r.SetPassword("bob", "pw2")
			}, userTime},
	}
	for _, s := range steps {
		before := time.Now().Unix()
		s.fossil()
		if err := goDo(s.goside); err != nil {
			t.Fatalf("%s: %v", s.what, err)
		}
		after := time.Now().Unix()
		secondsColumn(t, bin, fossilRepo, s.what, s.query, before, after)
		secondsColumn(t, bin, goRepo, s.what, s.query, before, after)
	}
}

// TestNameTimestampBoundaryMatchesFossil resolves BRANCH:TIMESTAMP where a
// check-in sits on the very millisecond the timestamp rounds up to. Fossil
// compares the stored event time, which it wrote as text, with julianday()
// of the bound, and for this millisecond the text form is the larger, so
// fossil does not count the check-in as at or before the bound. Both must
// resolve to the same check-in.
func TestNameTimestampBoundaryMatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "r.fossil")
	ck := filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilRun(t, bin, dir, "init", repoPath)
	fossilRun(t, bin, ck, "open", repoPath)
	writeFile(t, filepath.Join(ck, "a.txt"), "one\n")
	fossilRun(t, bin, ck, "add", "a.txt")
	fossilRun(t, bin, ck, "commit", "-m", "first", "--date-override", "2030-01-02 03:03:00.000")
	writeFile(t, filepath.Join(ck, "a.txt"), "two, longer\n")
	fossilRun(t, bin, ck, "commit", "-m", "second", "--date-override", "2030-01-02 03:04:00.999")
	fossilRun(t, bin, ck, "close")

	const query = "trunk:2030-01-02 03:04:00"
	info := fossilRun(t, bin, dir, "info", query, "-R", repoPath)
	m := regexp.MustCompile(`(?m)^hash:\s+([0-9a-f]+)`).FindStringSubmatch(info)
	if m == nil {
		t.Fatalf("fossil info %q printed no hash:\n%s", query, info)
	}
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rid, err := r.ResolveVersion(query)
	if err != nil {
		t.Fatalf("ResolveVersion(%q): %v", query, err)
	}
	got, err := r.UUIDFromRID(rid)
	if err != nil {
		t.Fatal(err)
	}
	// fossil info shows the hash's first 40 digits.
	if !strings.HasPrefix(got, m[1]) {
		t.Errorf("ResolveVersion(%q) = %s, fossil resolves %s", query, got, m[1])
	}
}
