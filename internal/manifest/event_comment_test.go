package manifest

import (
	"crypto/md5"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
)

// The expected comments are the ones fossil 2.28 wrote for the same cards.
func TestAttachmentComment(t *testing.T) {
	const src = "e05badb474cffee2aa0d8a2c4c1e7e0f6a8b9c0d"
	const tkt = "f0fcd1568784da2c08318aecf16014d96408abe7"
	const note = "17a58f7773a1096ed9a2af9882293c4b95105c75"
	const long = "a-very-long-attachment-name.txt"
	cases := []struct {
		name               string
		kind               byte
		src, file, target  string
		crosslink, refresh string
	}{
		{"wiki add", 'w', src, "a.txt", "Page1",
			"Add attachment [/artifact/e05badb474cffee2|a.txt] to wiki page [Page1]", ""},
		{"wiki delete", 'w', "", long, "Page1",
			`Delete attachment "a-very-long-attachment-name.txt" from wiki page [Page1]`, ""},
		{"ticket add", 't', src, "t <x>.txt", tkt,
			"Add attachment [/artifact/e05badb474cffee2|t &lt;x&gt;.txt] to ticket [f0fcd1568784da2c|f0fcd15687]",
			"Add attachment [/artifact/e05badb474cffee2|t &lt;x&gt;.txt] to ticket [f0fcd1568784da2c|f0fcd15687]"},
		{"ticket delete", 't', "", "t <x>.txt", tkt,
			`Delete attachment "t &lt;x&gt;.txt" from ticket [f0fcd1568784da2c|f0fcd15687]`,
			`Delete attachment "t &lt;x&gt;.txt" from ticket [f0fcd1568784da2c|f0fcd15687]`},
		{"technote add", 'e', src, "a.txt", note,
			"Add attachment [/artifact/e05badb474cffee2|a.txt] to tech note [/technote/17a58f7773a1096e|17a58f7773]",
			"Add attachment [/artifact/e05badb474cffee2|a.txt] to tech note [/technote/17a58f7773a1096e|17a58f7773]"},
		{"technote delete", 'e', "", long, note,
			`Delete attachment "/artifact/a-very-long-atta|a-very-long-attachment-name.txt" from tech note` +
				` [/technote/17a58f7773a1096e|17a58f7773]`,
			`Delete attachment "a-very-long-attachment-name.txt" from tech note [/technote/17a58f7773a1096e|17a58f7773]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := attachmentComment(c.kind, c.src, c.file, c.target, false, 10, 16); got != c.crosslink {
				t.Errorf("crosslink comment:\n got %q\nwant %q", got, c.crosslink)
			}
			if c.refresh == "" {
				return
			}
			if got := attachmentComment(c.kind, c.src, c.file, c.target, true, 10, 16); got != c.refresh {
				t.Errorf("refreshed comment:\n got %q\nwant %q", got, c.refresh)
			}
		})
	}
}

// TestCheckinWithoutCCardHasNullComment checks a check-in with no C card
// gets a NULL event comment, as fossil stores its absent zComment.
func TestCheckinWithoutCCardHasNullComment(t *testing.T) {
	r := setupTestRepo(t)
	parent, parentUUID, err := Checkin(r, CheckinOpts{
		Files: []File{{Name: "a.txt", Content: []byte("a")}}, Comment: "first", User: "u",
		Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if parent <= 0 {
		t.Fatalf("parent rid = %d", parent)
	}
	body := "D 2024-01-15T11:00:00.000\nP " + parentUUID + "\nU u\n"
	art := []byte(fmt.Sprintf("%sZ %x\n", body, md5.Sum([]byte(body))))
	rid, _, err := blob.Store(r.DB(), art)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	var got string
	if err := r.DB().QueryRow("SELECT quote(comment) FROM event WHERE objid=? AND type='ci'", rid).
		Scan(&got); err != nil {
		t.Fatalf("event query: %v", err)
	}
	if got != "NULL" {
		t.Errorf("event comment %s, want NULL", got)
	}
}
