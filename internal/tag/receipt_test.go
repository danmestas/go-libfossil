package tag_test

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/tag"
)

// A tag's control artifact is a receipt of its own, as `fossil tag add`
// records one rcvfrom row, credited to the tagging user.
func TestAddTagRecordsOneReceipt(t *testing.T) {
	r := setupTestRepo(t)
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit", User: "testuser",
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if _, err := r.DB().Exec("INSERT INTO user(login, pw, cap, info) VALUES('tagger', '', 'i', '')"); err != nil {
		t.Fatalf("add tagger: %v", err)
	}
	tagRid, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: rid, TagName: "label", TagType: tag.TagSingleton, User: "tagger",
	})
	if err != nil {
		t.Fatalf("AddTag: %v", err)
	}
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM rcvfrom").Scan(&n); err != nil {
		t.Fatalf("count rcvfrom: %v", err)
	}
	if n != 2 {
		t.Fatalf("rcvfrom rows = %d, want 2 (the check-in and the tag)", n)
	}
	var login string
	var blobs int
	if err := r.DB().QueryRow("SELECT user.login,"+
		" (SELECT count(*) FROM blob b WHERE b.rcvid=rcvfrom.rcvid)"+
		" FROM blob JOIN rcvfrom USING(rcvid) JOIN user ON user.uid=rcvfrom.uid"+
		" WHERE blob.rid=?", tagRid).Scan(&login, &blobs); err != nil {
		t.Fatalf("tag receipt: %v", err)
	}
	if login != "tagger" || blobs != 1 {
		t.Fatalf("tag receipt credits %q with %d blobs, want \"tagger\" with 1", login, blobs)
	}
}
