package manifest

import (
	"testing"

	"github.com/danmestas/go-libfossil/db"
)

// receiptRow is one rcvfrom row with the number of blobs naming it.
type receiptRow struct {
	rcvid, uid, blobs int64
	nonceNull, ipNull bool
}

func receiptRows(t *testing.T, q db.Querier) []receiptRow {
	t.Helper()
	rows, err := q.Query("SELECT rcvid, uid, nonce IS NULL, ipaddr IS NULL," +
		" (SELECT count(*) FROM blob WHERE blob.rcvid=rcvfrom.rcvid)" +
		" FROM rcvfrom ORDER BY rcvid")
	if err != nil {
		t.Fatalf("query rcvfrom: %v", err)
	}
	defer rows.Close()
	var out []receiptRow
	for rows.Next() {
		var r receiptRow
		if err := rows.Scan(&r.rcvid, &r.uid, &r.nonceNull, &r.ipNull, &r.blobs); err != nil {
			t.Fatalf("scan rcvfrom: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rcvfrom rows: %v", err)
	}
	return out
}

// Each check-in is one receipt, as each fossil commit is: one rcvfrom row,
// credited to the committing user with no nonce or address, named by the
// files it stored and its manifest.
func TestCheckinRecordsOneReceipt(t *testing.T) {
	r := setupTestRepo(t)
	rid1, _, err := Checkin(r, CheckinOpts{
		Comment: "one", User: "testuser",
		Files: []File{{Name: "a.txt", Content: []byte("a\n")}, {Name: "b.txt", Content: []byte("b\n")}},
	})
	if err != nil {
		t.Fatalf("Checkin one: %v", err)
	}
	if _, _, err := Checkin(r, CheckinOpts{
		Comment: "two", User: "testuser", Parent: rid1,
		Files: []File{{Name: "a.txt", Content: []byte("a\n")}, {Name: "b.txt", Content: []byte("b2\n")}},
	}); err != nil {
		t.Fatalf("Checkin two: %v", err)
	}
	want := []receiptRow{
		{rcvid: 1, uid: 1, blobs: 3, nonceNull: true, ipNull: true},
		{rcvid: 2, uid: 1, blobs: 2, nonceNull: true, ipNull: true},
	}
	got := receiptRows(t, r.DB())
	if len(got) != len(want) {
		t.Fatalf("rcvfrom rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rcvfrom row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A tree check-in's sub-manifests are stored by the commit that stores its
// manifest, so they name the commit's receipt, not one of their own.
func TestTreeCheckinSubmanifestsShareTheReceipt(t *testing.T) {
	r := sha3Repo(t)
	commit(t, r, CheckinOpts{Files: []File{{Name: "first.txt", Content: []byte("1\n")}},
		Comment: "an earlier receipt"})
	setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
	rid, d := commit(t, r, CheckinOpts{Files: a1CheckinFiles(), Comment: "tree"})
	if len(d.S) == 0 {
		t.Fatal("fixture: the check-in carries no S card")
	}
	rcvidOf := func(where string, arg any) int64 {
		t.Helper()
		var id int64
		if err := r.DB().QueryRow("SELECT rcvid FROM blob WHERE "+where, arg).Scan(&id); err != nil {
			t.Fatalf("rcvid where %s: %v", where, err)
		}
		return id
	}
	want := rcvidOf("rid=?", rid)
	if want <= 1 {
		t.Fatalf("manifest rcvid = %d, want the tree commit's own receipt", want)
	}
	for _, s := range d.S {
		if got := rcvidOf("uuid=?", s.UUID); got != want {
			t.Errorf("sub-manifest %s rcvid = %d, want the commit's %d", s.UUID[:10], got, want)
		}
	}
	var rows int
	if err := r.DB().QueryRow("SELECT count(*) FROM rcvfrom").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("rcvfrom rows = %d, want 2, one per commit", rows)
	}
}

// CheckinOpts.User is fossil's --user-override: a name the user table lacks
// still commits, under that name, and the rcvfrom row credits the local
// user, as fossil credits the logged-in user whatever the U card says.
func TestCheckinUnknownUserCreditsLocalUser(t *testing.T) {
	r := setupTestRepo(t)
	rid, _, err := Checkin(r, CheckinOpts{
		Comment: "ghostly", User: "ghost",
		Files: []File{{Name: "a.txt", Content: []byte("a\n")}},
	})
	if err != nil {
		t.Fatalf("Checkin by ghost: %v", err)
	}
	d, err := GetManifest(r, rid)
	if err != nil {
		t.Fatal(err)
	}
	if d.U == nil || *d.U != "ghost" {
		t.Fatalf("U card = %v, want ghost", d.U)
	}
	var login string
	if err := r.DB().QueryRow("SELECT user.login FROM blob JOIN rcvfrom USING(rcvid)"+
		" JOIN user ON user.uid=rcvfrom.uid WHERE blob.rid=?", rid).Scan(&login); err != nil {
		t.Fatalf("receipt of the check-in: %v", err)
	}
	if login != "testuser" {
		t.Fatalf("rcvfrom credits %q, want the local user testuser", login)
	}
}
