package manifest

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// captureInfo redirects the default logger into a buffer at INFO for one test.
func captureInfo(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestLogDeferredCheckinsBoundsIdentifierLists pins issue #194: the rollup used
// to render every deferred rid and every missing blob UUID inline, which on a
// clone of the Fossil repository was 417 hashes -- roughly 27 KB -- in one line.
// The counts are the actionable part and stay exact; the lists are capped.
func TestLogDeferredCheckinsBoundsIdentifierLists(t *testing.T) {
	buf := captureInfo(t)

	const (
		deferred = 180
		missing  = 417
	)
	rids := make([]libfossil.FslID, deferred)
	for i := range rids {
		rids[i] = libfossil.FslID(60000 + i)
	}
	blobs := make(map[string]struct{}, missing)
	for i := 0; i < missing; i++ {
		blobs[strings.Repeat(strconv.Itoa(i%10), 64)+strconv.Itoa(i)] = struct{}{}
	}

	logDeferredCheckins("Crosslink", rids, blobs, 3)

	line := buf.String()
	if !strings.Contains(line, "deferred="+strconv.Itoa(deferred)) {
		t.Errorf("deferred count missing from %q", line)
	}
	if !strings.Contains(line, "missing_blob_count="+strconv.Itoa(missing)) {
		t.Errorf("missing_blob_count missing from %q", line)
	}
	if !strings.Contains(line, "linked=3") {
		t.Errorf("linked count missing from %q", line)
	}

	// A line this size is readable in a terminal and survives a log shipper.
	// The pre-fix line for these inputs was ~28 KB.
	if len(line) > 2048 {
		t.Errorf("log line is %d bytes, want <= 2048:\n%s", len(line), line)
	}

	// The elided remainder is reported rather than silently dropped.
	for _, want := range []string{
		"and " + strconv.Itoa(deferred-logDeferredSampleSize) + " more",
		"and " + strconv.Itoa(missing-logDeferredSampleSize) + " more",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing elision marker %q in %q", want, line)
		}
	}
}

// TestLogDeferredCheckinsShortListsAreComplete keeps the common case whole: a
// handful of deferred artifacts is small enough to print in full, and must not
// grow a misleading "and 0 more".
func TestLogDeferredCheckinsShortListsAreComplete(t *testing.T) {
	buf := captureInfo(t)

	logDeferredCheckins("AfterDephantomize",
		[]libfossil.FslID{7, 9},
		map[string]struct{}{"aa": {}, "bb": {}},
		1)

	line := buf.String()
	for _, want := range []string{"7", "9", "aa", "bb", "deferred=2", "missing_blob_count=2"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %q", want, line)
		}
	}
	if strings.Contains(line, "more") {
		t.Errorf("unexpected elision marker in %q", line)
	}
}
