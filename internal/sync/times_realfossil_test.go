package sync_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// syncTextTime renders a time fossil writes as SQL text: bit for bit where
// this machine's fossil computes it as go-libfossil does (a 64-bit long
// double: macOS arm64, Windows), otherwise to the millisecond it stands for.
func syncTextTime(col string) string {
	if (runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") || runtime.GOOS == "windows" {
		return "printf('%!.20g', " + col + ")"
	}
	return "CAST(round((" + col + " - 2440587.5) * 86400000) AS INTEGER)"
}

// storedTimeQueries read every synced artifact's derived times and every
// unversioned file's time, keyed by hash and name, not by rid, which differs
// between repositories.
var storedTimeQueries = map[string]string{
	"event": "SELECT b.uuid||' '||" + syncTextTime("e.mtime") + "||' '||" + syncTextTime("e.omtime") +
		" FROM event e JOIN blob b ON b.rid=e.objid WHERE e.type='ci' ORDER BY 1",
	"plink": "SELECT p.uuid||' '||c.uuid||' '||" + syncTextTime("l.mtime") +
		" FROM plink l JOIN blob p ON p.rid=l.pid JOIN blob c ON c.rid=l.cid ORDER BY 1",
	"tagxref": "SELECT t.tagname||' '||b.uuid||' '||printf('%!.20g', x.mtime)" +
		" FROM tagxref x JOIN tag t ON t.tagid=x.tagid JOIN blob b ON b.rid=x.rid" +
		" WHERE t.tagname NOT GLOB 'sym-trunk' ORDER BY 1",
	"unversioned": "SELECT name||' '||typeof(mtime)||' '||mtime FROM unversioned ORDER BY 1",
}

// storedTimes runs storedTimeQueries against d. Both repositories are read
// through go-libfossil's SQLite, so their values are rendered alike.
func storedTimes(t *testing.T, d db.Querier) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for table, q := range storedTimeQueries {
		rows, err := d.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], s)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestSyncedTimesMatchRealFossil clones a go-libfossil server with the fossil
// binary, commits in the clone at a time finer than a millisecond and pushes
// it back. Each side derives every check-in's event, plink and tagxref times
// from the artifacts itself, and keeps an unversioned file's time as sent, so
// the two repositories must then hold the same times.
func TestSyncedTimesMatchRealFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	srv, err := repo.Create(filepath.Join(dir, "server.fossil"), "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	base := time.Date(2026, 10, 18, 9, 8, 7, 0, time.UTC)
	rid, _, err := manifest.Checkin(srv, manifest.CheckinOpts{
		Comment: "seed", User: "testuser", Time: base.Add(654321987),
		Files: []manifest.File{{Name: "seed.txt", Content: []byte("seed\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tag.AddTag(srv, tag.TagOpts{TargetRID: rid, TagName: "sym-rel",
		TagType: tag.TagSingleton, User: "testuser", Time: base.Add(time.Minute + 789499999)}); err != nil {
		t.Fatal(err)
	}
	if err := uv.EnsureSchema(srv.DB()); err != nil {
		t.Fatal(err)
	}
	if err := uv.Write(srv.DB(), "notes.txt", []byte("notes\n"), base.Unix()); err != nil {
		t.Fatal(err)
	}
	var pc string
	if err := srv.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc); err != nil {
		t.Fatal(err)
	}
	if err := auth.CreateUser(srv.DB(), pc, "alice", "pw123", "oiy"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := serveRepo(ctx, t, srv)
	u, err := url.Parse(addr)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("alice", "pw123")

	clonePath := filepath.Join(dir, "clone.fossil")
	fossilOK(t, bin, dir, "clone", "--unversioned", u.String(), clonePath)
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilOK(t, bin, work, "settings", "autosync", "off", "-R", clonePath)
	fossilOK(t, bin, work, "open", "--nosync", clonePath)
	if err := os.WriteFile(filepath.Join(work, "alice.txt"), []byte("from alice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fossilOK(t, bin, work, "add", "alice.txt")
	fossilOK(t, bin, work, "commit", "-m", "alice", "--no-warnings",
		"--date-override", "2026-10-18 10:11:12.3456789")
	fossilOK(t, bin, work, "push", u.String(), "--once")
	if _, err := manifest.Crosslink(srv); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	fossilOK(t, bin, work, "close")
	clone, err := db.OpenSQL(clonePath, db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	got, want := storedTimes(t, srv.DB()), storedTimes(t, clone)
	for table := range storedTimeQueries {
		if len(want[table]) == 0 {
			t.Errorf("%s: fossil's clone holds no rows; the comparison proves nothing", table)
		}
		if !reflect.DeepEqual(got[table], want[table]) {
			t.Errorf("%s on the go-libfossil server:\n  %s\nin fossil's clone:\n  %s", table,
				strings.Join(got[table], "\n  "), strings.Join(want[table], "\n  "))
		}
	}
}
