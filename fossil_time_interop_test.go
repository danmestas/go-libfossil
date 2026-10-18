package libfossil_test

// The times go-libfossil writes and stores, against the fossil binary as the
// oracle: each check is the same operation done by both, at the same time,
// with the stored or written values compared.

import (
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
		time.Date(2030, 3, 4, 12, 35, 56, 789500000, time.UTC), // .7895 rounds down
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
