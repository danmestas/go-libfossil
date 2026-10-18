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
