package manifest

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// crosslinkDerivedTables are the tables a Crosslink sweep is responsible for.
// Emptying them puts a fully-built repository back in the state a
// freshly-transferred clone is in immediately before crosslinking.
var crosslinkDerivedTables = []string{
	"event", "plink", "leaf", "mlink", "tagxref", "forumpost",
	"attachment", "backlink", "cherrypick",
}

// TestFossilBinaryReadsCrosslinkedRepo drives the accelerated crosslink path
// end to end and checks two things our own tests cannot: that a repository
// whose relational tables were written by Crosslink -- expanding artifacts
// through the memoizing content cache rather than one full chain walk per
// blob -- is readable by canonical Fossil, and that what Crosslink wrote
// matches what Fossil itself derives from the same blobs.
//
// Method: build a history deep enough that the commit path deltifies it,
// empty the derived tables, run Crosslink over the untouched blobs, hand the
// result to the fossil binary, and finally let `fossil rebuild` re-derive the
// same tables from the same blobs so the two derivations can be compared.
//
// The rebuild runs last here so its output can be diffed against what
// Crosslink already wrote -- not because ordering matters anymore.
// TestCrosslinkAfterFossilRebuild below exercises the opposite order,
// where rebuild drops the on-demand tables (forumpost) before Crosslink
// ever runs.
func TestFossilBinaryReadsCrosslinkedRepo(t *testing.T) {
	bin := testutil.RequireFossilBin(t)

	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("fossil %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	path := filepath.Join(t.TempDir(), "crosslinked.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	incrementalHistory(t, r, 3, 25, 400)
	s := collectStorageStats(t, r)
	if s.deltaEncoded == 0 {
		t.Fatal("no deltas were produced; this test would prove nothing")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	for _, tbl := range crosslinkDerivedTables {
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r2, err := repo.Open(path)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	linked, err := Crosslink(r2)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked == 0 {
		t.Fatal("Crosslink linked nothing")
	}
	if err := r2.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	got := snapshotDerived(t, path)

	integrity := run("test-integrity", "-R", path)
	if !strings.Contains(integrity, "0 errors") {
		t.Fatalf("fossil test-integrity did not report 0 errors:\n%s", integrity)
	}
	if !strings.Contains(integrity, "low-level database integrity-check: ok") {
		t.Fatalf("fossil reported a low-level database problem:\n%s", integrity)
	}

	// timeline reads event and plink, the tables Crosslink just rewrote.
	timeline := run("timeline", "-R", path, "-n", "5")
	if !strings.Contains(timeline, "revision 24") {
		t.Fatalf("fossil timeline does not show the tip check-in:\n%s", timeline)
	}

	if stats := run("rebuild", path, "--stats"); !strings.Contains(stats, "Artifacts:") {
		t.Fatalf("fossil rebuild produced no statistics")
	}

	// Fossil has now re-derived the same tables from the same blobs. The two
	// derivations must agree row for row on the columns compared below.
	//
	// That is narrower than full-table equivalence: the event digest covers
	// objid, type, user and comment but not mtime or tagid, and tagxref,
	// backlink, attachment, cherrypick and forumpost are emptied above but
	// never compared. tagxref is the notable gap -- it holds the
	// order-sensitive state that visiting candidates in delta-chain order
	// would disturb.
	reference := snapshotDerived(t, path)
	for _, key := range []string{"event", "plink", "leaf", "mlink"} {
		if got[key] != reference[key] {
			t.Errorf("%s differs from what fossil derived\n fossil:    %s\n crosslink: %s",
				key, reference[key], got[key])
		}
	}
}

// TestCrosslinkAfterFossilRebuild is the #103 regression: a repository
// straight out of a canonical `fossil rebuild` has forumpost dropped (this
// history has no forum posts, so canonical never recreates it), and
// Crosslink's candidate query names that table unconditionally. It must
// still succeed, recreating the table the way canonical would if a forum
// artifact showed up.
func TestCrosslinkAfterFossilRebuild(t *testing.T) {
	bin, err := exec.LookPath("fossil")
	if err != nil {
		if os.Getenv("REQUIRE_FOSSIL_BIN") == "1" {
			t.Fatalf("REQUIRE_FOSSIL_BIN=1 but no fossil binary on PATH: %v", err)
		}
		t.Skip("fossil binary not on PATH; cannot verify against canonical rebuild")
	}

	path := filepath.Join(t.TempDir(), "rebuilt.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	incrementalHistory(t, r, 2, 10, 200)
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	// Canonical rebuild runs before Crosslink here -- the order Crosslink
	// could not previously tolerate, since rebuild drops forumpost for a
	// history that never populated it.
	if out, err := exec.Command(bin, "rebuild", path).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	var forumpostExists int
	if err := d.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='forumpost'").Scan(&forumpostExists); err != nil {
		t.Fatalf("check forumpost: %v", err)
	}
	if forumpostExists != 0 {
		t.Fatal("forumpost survived fossil rebuild; this test proves nothing without it gone")
	}
	for _, tbl := range crosslinkDerivedTables {
		if tbl == "forumpost" {
			continue // rebuild dropped it; nothing to clear
		}
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r2, err := repo.Open(path)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r2.Close()
	linked, err := Crosslink(r2)
	if err != nil {
		t.Fatalf("Crosslink after fossil rebuild: %v", err)
	}
	if linked == 0 {
		t.Fatal("Crosslink linked nothing")
	}
}

// snapshotDerived returns a per-table digest of the crosslink-derived tables,
// restricted to the columns Crosslink is responsible for writing.
func snapshotDerived(t *testing.T, path string) map[string]string {
	t.Helper()

	queries := map[string]string{
		"event": `SELECT group_concat(v, '|') FROM
		            (SELECT objid || ':' || type || ':' || coalesce(user,'') || ':' ||
		                    coalesce(comment,'') AS v FROM event ORDER BY objid)`,
		"plink": `SELECT group_concat(v, '|') FROM
		            (SELECT pid || '>' || cid || ':' || isprim AS v FROM plink ORDER BY pid, cid)`,
		"leaf": `SELECT group_concat(rid, '|') FROM (SELECT rid FROM leaf ORDER BY rid)`,
		"mlink": `SELECT group_concat(v, '|') FROM
		            (SELECT mid || ':' || fid || ':' || pid || ':' || fnid || ':' ||
		                    coalesce(pfnid,'') || ':' || coalesce(mperm,'') AS v
		               FROM mlink ORDER BY mid, fnid, fid)`,
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("snapshotDerived open: %v", err)
	}
	defer d.Close()

	out := make(map[string]string, len(queries))
	for name, q := range queries {
		var v any
		if err := d.QueryRow(q).Scan(&v); err != nil {
			t.Fatalf("snapshotDerived %s: %v", name, err)
		}
		s, ok := v.(string)
		if !ok || s == "" {
			// A NULL digest means the table is empty. Left unset, an empty
			// table would compare equal to another empty table and the
			// comparison would pass without having examined a single row.
			t.Fatalf("snapshotDerived %s: empty digest; the table has no rows "+
				"and the comparison would be vacuous", name)
		}
		out[name] = s
	}
	return out
}
