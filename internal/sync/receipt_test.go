package sync_test

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/simio"
)

// rcvRow is an rcvfrom row as fossil's /rcvfromlist shows it: who, from
// where, under what nonce, and how many blobs name it.
type rcvRow struct {
	Login  string
	Nonce  string // "NULL" when NULL
	IPAddr string // "NULL" when NULL
	Blobs  int
	Julian bool // mtime is a Julian day, as fossil's julianday('now')
}

func rcvRows(t *testing.T, q db.Querier) []rcvRow {
	t.Helper()
	rows, err := q.Query("SELECT coalesce((SELECT login FROM user WHERE user.uid=rcvfrom.uid), '?')," +
		" coalesce(nonce, 'NULL'), coalesce(ipaddr, 'NULL')," +
		" (SELECT count(*) FROM blob WHERE blob.rcvid=rcvfrom.rcvid)," +
		" typeof(mtime)='real' AND mtime BETWEEN 2440587.5 AND 2488069.5" +
		" FROM rcvfrom ORDER BY rcvid")
	if err != nil {
		t.Fatalf("query rcvfrom: %v", err)
	}
	defer rows.Close()
	var out []rcvRow
	for rows.Next() {
		var r rcvRow
		if err := rows.Scan(&r.Login, &r.Nonce, &r.IPAddr, &r.Blobs, &r.Julian); err != nil {
			t.Fatalf("scan rcvfrom: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rcvfrom rows: %v", err)
	}
	return out
}

var sha1Hex = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Each clone, pull or accepted push is one receipt, recorded as fossil
// records it: the client credits its own user and the server's address, and
// the server credits the logged-in user, the login card's nonce and the
// client's address. A sync that receives nothing records nothing.
func TestSyncRecordsOneReceiptPerTransfer(t *testing.T) {
	dir := t.TempDir()
	srv, err := repo.Create(filepath.Join(dir, "server.fossil"), "owner", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srv.Close()
	pc, err := srv.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.CreateUser(srv.DB(), pc, "alice", "pw", "oi"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, _, err := manifest.Checkin(srv, manifest.CheckinOpts{
		Comment: "seed", User: "owner",
		Files: []manifest.File{{Name: "seed.txt", Content: []byte("seed\n")}},
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url := serveRepo(ctx, t, srv)
	const user, pw = "alice", "pw"

	cl, _, err := sync.Clone(ctx, filepath.Join(dir, "clone.fossil"),
		&sync.HTTPTransport{URL: url}, sync.CloneOpts{User: user, Password: pw})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer cl.Close()
	got := rcvRows(t, cl.DB())
	want := []rcvRow{{Login: "alice", Nonce: "NULL", IPAddr: "127.0.0.1", Blobs: 2, Julian: true}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("clone rcvfrom = %+v, want %+v", got, want)
	}

	// Nothing new to pull: no receipt.
	syncOpts := sync.SyncOpts{Pull: true, Push: true, User: user, Password: pw}
	if _, err := sync.Sync(ctx, cl, &sync.HTTPTransport{URL: url}, syncOpts); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := rcvRows(t, cl.DB()); len(got) != 1 {
		t.Fatalf("an empty sync recorded a receipt: %+v", got)
	}
	if got := rcvRows(t, srv.DB()); len(got) != 1 {
		t.Fatalf("an empty sync recorded a receipt on the server: %+v", got)
	}

	// A push: one receipt on the server, credited to alice.
	tip := tipRid(t, cl)
	if _, _, err := manifest.Checkin(cl, manifest.CheckinOpts{
		Comment: "from alice", User: "alice", Parent: tip,
		Files: []manifest.File{{Name: "seed.txt", Content: []byte("seed\n")},
			{Name: "alice.txt", Content: []byte("alice\n")}},
	}); err != nil {
		t.Fatalf("clone Checkin: %v", err)
	}
	if _, err := sync.Sync(ctx, cl, &sync.HTTPTransport{URL: url}, syncOpts); err != nil {
		t.Fatalf("push Sync: %v", err)
	}
	srvRows := rcvRows(t, srv.DB())
	if len(srvRows) != 2 {
		t.Fatalf("server rcvfrom after push = %+v, want 2 rows", srvRows)
	}
	push := srvRows[1]
	if push.Login != "alice" || !sha1Hex.MatchString(push.Nonce) ||
		push.IPAddr != "127.0.0.1" || push.Blobs != 2 || !push.Julian {
		t.Fatalf("server push receipt = %+v, want alice, a SHA-1 nonce, 127.0.0.1, 2 blobs", push)
	}
	if got := rcvRows(t, cl.DB()); len(got) != 2 || got[1].Nonce != "NULL" || got[1].IPAddr != "NULL" {
		t.Fatalf("clone rcvfrom after local commit = %+v, want the commit's local receipt", got)
	}
}

func tipRid(t *testing.T, r *repo.Repo) libfossil.FslID {
	t.Helper()
	var rid int64
	if err := r.DB().QueryRow("SELECT objid FROM event WHERE type='ci' ORDER BY mtime DESC LIMIT 1").Scan(&rid); err != nil {
		t.Fatalf("tip: %v", err)
	}
	return libfossil.FslID(rid)
}

// uvReceipt reports who and where the unversioned file name was received
// from, through the rcvfrom row its rcvid names.
func uvReceipt(t *testing.T, q db.Querier, name string) rcvRow {
	t.Helper()
	var r rcvRow
	err := q.QueryRow("SELECT coalesce(user.login, '?'), coalesce(rcvfrom.nonce, 'NULL'),"+
		" coalesce(rcvfrom.ipaddr, 'NULL'), 0, typeof(rcvfrom.mtime)='real' FROM unversioned JOIN rcvfrom USING(rcvid)"+
		" LEFT JOIN user ON user.uid=rcvfrom.uid WHERE name=?", name).
		Scan(&r.Login, &r.Nonce, &r.IPAddr, &r.Blobs, &r.Julian)
	if err != nil {
		t.Fatalf("receipt of unversioned %s: %v", name, err)
	}
	return r
}

// An unversioned file received in a sync names that sync's receipt, on the
// server and on the client, as xfer_accept_unversioned_file stores g.rcvid.
func TestUVSyncRecordsTheReceipt(t *testing.T) {
	dir := t.TempDir()
	srv, err := repo.Create(filepath.Join(dir, "server.fossil"), "owner", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srv.Close()
	pc, err := srv.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.CreateUser(srv.DB(), pc, "alice", "pw", "oiy"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := uv.EnsureSchema(srv.DB()); err != nil {
		t.Fatal(err)
	}
	if err := uv.Write(srv.DB(), "down.txt", []byte("down\n"), 1700000000); err != nil {
		t.Fatalf("uv.Write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url := serveRepo(ctx, t, srv)

	cl, err := repo.Create(filepath.Join(dir, "client.fossil"), "alice", simio.CryptoRand{}, pc)
	if err != nil {
		t.Fatalf("client repo.Create: %v", err)
	}
	defer cl.Close()
	if err := uv.EnsureSchema(cl.DB()); err != nil {
		t.Fatal(err)
	}
	if err := uv.Write(cl.DB(), "up.txt", []byte("up\n"), 1700000001); err != nil {
		t.Fatalf("uv.Write: %v", err)
	}
	if _, err := sync.Sync(ctx, cl, &sync.HTTPTransport{URL: url},
		sync.SyncOpts{UV: true, User: "alice", Password: "pw"}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	up := uvReceipt(t, srv.DB(), "up.txt")
	if up.Login != "alice" || !sha1Hex.MatchString(up.Nonce) || up.IPAddr != "127.0.0.1" {
		t.Fatalf("server's receipt of up.txt = %+v, want alice, a nonce, 127.0.0.1", up)
	}
	down := uvReceipt(t, cl.DB(), "down.txt")
	if down.Login != "alice" || down.Nonce != "NULL" || down.IPAddr != "127.0.0.1" {
		t.Fatalf("client's receipt of down.txt = %+v, want alice, no nonce, 127.0.0.1", down)
	}
}
