package verify_test

// The event rows fossil derives for every artifact type, not only check-ins:
// a repository made by the fossil binary, with tags, branch moves, wiki,
// tickets, tech notes, forum posts and attachments, is rebuilt once by
// `fossil rebuild` and once by verify.Rebuild, and the event and attachment
// tables must come back identical, comment text byte for byte.

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/branch"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/testutil"
)

// fossilEventHistory runs the fossil commands that write a control, wiki,
// technote, ticket or attachment artifact, each with the values whose
// comment text fossil escapes or links.
func fossilEventHistory(t *testing.T, bin, ck string) {
	t.Helper()
	path := filepath.Join(ck, "a.txt")
	steps := [][]string{
		{"commit", "-m", "first"},
		{"commit", "-m", "on feat", "--branch", "feat", "--bgcolor", "#abc"},
		{"tag", "add", "rel-1", "current"},
		{"tag", "add", "note", "current", `val "q" <b>`},
		{"tag", "add", "--propagate", "prop", "current", "pv"},
		{"tag", "cancel", "rel-1", "current"},
		{"tag", "add", "--raw", "plain", "current"},
		{"tag", "add", "--raw", "--propagate", "rawprop", "current"},
		{"tag", "cancel", "--raw", "plain", "current"},
		{"amend", "current", "-m", "edited comment"},
		{"amend", "current", "--date", "2026-01-02 03:04:05"},
		{"amend", "current", "--branchcolor", "#eee"},
		{"amend", "current", "--hide"},
		{"amend", "current", "--bgcolor", "#a1a", "--close", "--tag", "t'9", "--author", "carol"},
		{"tag", "cancel", "--raw", "bgcolor", "current"},
		{"update", "trunk"},
		{"commit", "-m", "third"},
		{"amend", "current", "--branch", "mo ved&/x"},
		{"wiki", "create", "Page1", path},
		{"wiki", "create", "Tech", path, "--technote", "2026-02-03 04:05:06",
			"--technote-tags", "t1 t2", "--technote-bgcolor", "#123"},
		{"ticket", "add", "title", "tkt one", "status", "Open"},
		{"attachment", "add", "Page1", path},
	}
	for i, args := range steps {
		if args[0] == "commit" { // a new length, as the same second hides an edit
			if err := os.WriteFile(path, []byte(strings.Repeat("v", i+2)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		fossilCmd(t, bin, ck, args...)
	}
}

// rawArtifact appends the Z card to cards (and a W card for body, when
// given), building an artifact the fossil commands cannot write.
func rawArtifact(body string, cards ...string) []byte {
	s := strings.Join(cards, "\n") + "\n"
	if body != "" {
		s += fmt.Sprintf("W %d\n%s\n", len(body), body)
	}
	return []byte(fmt.Sprintf("%sZ %x\n", s, md5.Sum([]byte(s))))
}

// storeRawArtifacts adds what no fossil command writes: a control artifact
// that edits two check-ins and moves one to a new branch, forum posts, and
// attachments to a ticket, a wiki page, a tech note and an unknown target,
// some of them deletions.
func storeRawArtifacts(t *testing.T, path string) {
	t.Helper()
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	one := func(q string) string {
		var v string
		if err := r.DB().QueryRow(q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}
	ciA := one("SELECT min(uuid) FROM blob JOIN event ON objid=rid WHERE type='ci'")
	ciB := one("SELECT max(uuid) FROM blob JOIN event ON objid=rid WHERE type='ci'")
	tkt := one("SELECT substr(tagname, 5) FROM tag WHERE tagname GLOB 'tkt-*'")
	note := one("SELECT substr(tagname, 7) FROM tag WHERE tagname GLOB 'event-*'")
	src := one("SELECT uuid FROM blob WHERE rid=(SELECT max(rid) FROM blob" +
		" WHERE rid NOT IN (SELECT objid FROM event))")
	put := func(art []byte) string {
		_, uuid, err := blob.Store(r.DB(), art)
		if err != nil {
			t.Fatal(err)
		}
		return uuid
	}
	root := put(rawArtifact("first post", `D 2026-03-01T10:00:00.000`, `H Hello\sforum`, "U alice"))
	put(rawArtifact("a reply", "D 2026-03-01T11:00:00.000", "G "+root, "I "+root, "U bob"))
	put(rawArtifact("edited", "D 2026-03-01T12:00:00.000", "G "+root, `H Hello\sagain`, "P "+root, "U alice"))
	put(rawArtifact("", "D 2026-03-02T10:00:00.000", `T *branch `+ciA+` x\sy&z/w`,
		`T *sym-x\sy&z/w `+ciA, "T +sym-one "+ciA, "T +sym-one "+ciB+" n<1>",
		"T -sym-trunk "+ciA, "T -zap "+ciB+" why", "U alice"))
	put(rawArtifact("", `A t\s<x>.txt `+tkt+" "+src, "D 2026-03-03T10:00:00.000", "U alice"))
	put(rawArtifact("", `A t\s<x>.txt `+tkt, "D 2026-03-03T11:00:00.000", "U alice"))
	put(rawArtifact("", "A a-very-long-attachment-name.txt Page1", "D 2026-03-03T12:00:00.000", "U alice"))
	put(rawArtifact("", "A a-very-long-attachment-name.txt "+note, "D 2026-03-03T13:00:00.000", "U alice"))
	put(rawArtifact("", "A early.txt "+strings.Repeat("ab", 32)+" "+src, "D 2026-03-03T14:00:00.000", "U alice"))
}

// eventRows reads every event and attachment column. Times follow textTime.
func eventRows(t *testing.T, path string) map[string][]string {
	t.Helper()
	return queryRows(t, path, map[string]string{
		"event": "SELECT objid, '|', type, '|', " + textTime("mtime") + ", '|', " + textTime("omtime") +
			", '|', quote(tagid), '|', quote(uid), '|', quote(bgcolor), '|', quote(euser), '|', quote(user)" +
			", '|', quote(ecomment), '|', quote(comment), '|', quote(brief) FROM event ORDER BY objid",
		"attachment": "SELECT attachid, '|', " + textTime("mtime") + ", '|', quote(src), '|', target" +
			", '|', filename, '|', quote(comment), '|', user, '|', isLatest FROM attachment ORDER BY attachid",
	})
}

func TestRebuildMatchesFossilEventRows(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	orig := filepath.Join(dir, "f.fossil")
	ck := filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilCmd(t, bin, dir, "init", "-A", "alice", orig)
	fossilCmd(t, bin, ck, "open", orig)
	fossilCmd(t, bin, ck, "user", "default", "alice")
	if err := os.WriteFile(filepath.Join(ck, "a.txt"), []byte("v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fossilCmd(t, bin, ck, "add", "a.txt")
	fossilEventHistory(t, bin, ck)
	fossilCmd(t, bin, ck, "close", "--force")
	storeRawArtifacts(t, orig)
	if err := testutil.FossilRebuild(orig); err != nil {
		t.Fatal(err)
	}

	rebuilt := filepath.Join(dir, "rebuilt.fossil")
	copyFile(t, orig, rebuilt)
	r, err := repo.Open(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify.Rebuild(r); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	want, got := eventRows(t, orig), eventRows(t, rebuilt)
	if len(want["event"]) < 30 || len(want["attachment"]) < 6 {
		t.Fatalf("history too small: %d events, %d attachments", len(want["event"]), len(want["attachment"]))
	}
	for table := range want {
		if !reflect.DeepEqual(got[table], want[table]) {
			t.Errorf("%s after Rebuild:\n  %s\nfossil rebuild wrote:\n  %s", table,
				strings.Join(got[table], "\n  "), strings.Join(want[table], "\n  "))
		}
	}
}

// TestFossilRebuildKeepsGoEventRows rebuilds a repository go-libfossil wrote,
// tags and a closed branch included, with the fossil binary: every event row
// must be the one go-libfossil stored.
func TestFossilRebuildKeepsGoEventRows(t *testing.T) {
	testutil.RequireFossilBin(t)
	path := goTimesHistory(t)
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var tip fsltype.FslID
	if err := r.DB().QueryRow("SELECT max(objid) FROM event WHERE type='ci'").Scan(&tip); err != nil {
		t.Fatal(err)
	}
	if _, err := tag.AddTag(r, tag.TagOpts{TargetRID: tip, TagName: "note", TagType: tag.TagPropagating,
		Value: `a "b" <c>`, User: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := branch.Close(r, "feat", "u"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	stored := eventRows(t, path)
	if err := testutil.FossilRebuild(path); err != nil {
		t.Fatal(err)
	}
	rebuilt := eventRows(t, path)
	if !reflect.DeepEqual(stored["event"], rebuilt["event"]) {
		t.Errorf("event as go-libfossil stored it:\n  %s\nafter fossil rebuild:\n  %s",
			strings.Join(stored["event"], "\n  "), strings.Join(rebuilt["event"], "\n  "))
	}
}
