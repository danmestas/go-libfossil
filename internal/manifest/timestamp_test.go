package manifest

import (
	"regexp"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
)

// dCardMillis is the D card fossil and zit write: whole seconds plus exactly
// three fractional digits.
var dCardMillis = regexp.MustCompile(`(?m)^D \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}$`)

// TestCheckinWritesMillisecondTimestamps commits once with a caller time
// carrying nanoseconds and once with no time (the clock default), and reads
// back each manifest's D card. Both must carry milliseconds, as fossil and
// zit write them; finer digits are legal but zit cannot read them.
func TestCheckinWritesMillisecondTimestamps(t *testing.T) {
	r := setupTestRepo(t)
	cases := []struct {
		name string
		when time.Time
	}{
		{"caller time with nanoseconds", time.Date(2024, 1, 15, 10, 30, 0, 123456789, time.UTC)},
		{"default clock time", time.Time{}},
	}
	for i, c := range cases {
		rid, _, err := Checkin(r, CheckinOpts{
			Files:   []File{{Name: "f.txt", Content: []byte{byte('a' + i)}}},
			Comment: c.name,
			User:    "testuser",
			Time:    c.when,
		})
		if err != nil {
			t.Fatalf("%s: Checkin: %v", c.name, err)
		}
		data, err := content.Expand(r.DB(), rid)
		if err != nil {
			t.Fatalf("%s: Expand: %v", c.name, err)
		}
		if !dCardMillis.Match(data) {
			t.Fatalf("%s: D card is not millisecond precision:\n%s", c.name, data)
		}
	}
}
