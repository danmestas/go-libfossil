package tag_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/tag"
)

// TestAddTagWritesMillisecondTimestamps tags a check-in with a time carrying
// nanoseconds and checks the control artifact's D card has exactly three
// fractional digits, rounded as `fossil tag add --date-override` rounds them.
func TestAddTagWritesMillisecondTimestamps(t *testing.T) {
	r := setupTestRepo(t)
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	tagRid, err := tag.AddTag(r, tag.TagOpts{
		TargetRID: rid,
		TagName:   "testlabel",
		TagType:   tag.TagSingleton,
		User:      "testuser",
		Time:      time.Date(2024, 1, 15, 11, 0, 0, 987654321, time.UTC),
	})
	if err != nil {
		t.Fatalf("tag.AddTag: %v", err)
	}
	data, err := content.Expand(r.DB(), tagRid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	millis := regexp.MustCompile(`(?m)^D 2024-01-15T11:00:00\.988$`)
	if !millis.Match(data) {
		t.Fatalf("D card is not millisecond precision:\n%s", data)
	}
}
