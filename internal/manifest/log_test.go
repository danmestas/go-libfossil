package manifest

import (
	"bytes"
	"fmt"
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

// TestLogDeferredCheckinsSelectsSmallestSamples makes the bounded summary
// stable when a receive sweep encounters identifiers in reverse order.
func TestLogDeferredCheckinsSelectsSmallestSamples(t *testing.T) {
	buf := captureInfo(t)
	const deferrals = logDeferredSampleSize + 3

	rids := make([]libfossil.FslID, 0, deferrals)
	blobs := make(map[string]struct{}, deferrals)
	for value := deferrals; value > 0; value-- {
		rids = append(rids, libfossil.FslID(value))
		blobs[fmt.Sprintf("%040x", value)] = struct{}{}
	}

	logDeferredCheckins("Crosslink", rids, blobs, 0)

	line := buf.String()
	for _, want := range []string{
		"deferred=" + strconv.Itoa(deferrals),
		"missing_blob_count=" + strconv.Itoa(deferrals),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing exact count %q in %q", want, line)
		}
	}

	smallestRIDs := make([]string, logDeferredSampleSize)
	smallestUUIDs := make([]string, logDeferredSampleSize)
	for i := range smallestRIDs {
		value := i + 1
		smallestRIDs[i] = strconv.Itoa(value)
		smallestUUIDs[i] = fmt.Sprintf("%040x", value)
	}
	if want := `deferred_rids="` + strings.Join(smallestRIDs, " ") +
		" ...and " + strconv.Itoa(deferrals-logDeferredSampleSize) + ` more"`; !strings.Contains(line, want) {
		t.Errorf("deferred RID sample = %q, want to contain %q", line, want)
	}
	if want := `missing_blobs="` + strings.Join(smallestUUIDs, " ") +
		" ...and " + strconv.Itoa(deferrals-logDeferredSampleSize) + ` more"`; !strings.Contains(line, want) {
		t.Errorf("missing UUID sample = %q, want to contain %q", line, want)
	}
}

// TestLogDeferredCheckinSummaryReportsObservationsWithoutInventingElision
// distinguishes repeated deferral/reference observations from the bounded,
// distinct diagnostic samples retained by the production guard.
func TestLogDeferredCheckinSummaryReportsObservationsWithoutInventingElision(t *testing.T) {
	buf := captureInfo(t)
	guard := newCheckinDeferralGuard()

	const (
		distinctSamples     = logDeferredSampleSize + 4
		deferAttempts       = logDeferredSampleSize + 15
		missingObservations = logDeferredSampleSize + 23
	)
	for value := distinctSamples; value > 0; value-- {
		guard.deferredRids = insertDeferredRIDSample(guard.deferredRids, libfossil.FslID(value))
		guard.recordMissingBlobSample(fmt.Sprintf("%040x", value))
	}
	guard.deferredCount = deferAttempts
	guard.missingReferenceCount = missingObservations

	logDeferredCheckinSummary("Crosslink", guard, 3)

	line := buf.String()
	for _, want := range []string{
		"defer_attempt_count=" + strconv.Itoa(deferAttempts),
		"missing_reference_observation_count=" + strconv.Itoa(missingObservations),
		`deferred_rid_sample="1 2 3 4 5 6 7 8"`,
		`missing_uuid_sample="` + strings.Join([]string{
			"0000000000000000000000000000000000000001",
			"0000000000000000000000000000000000000002",
			"0000000000000000000000000000000000000003",
			"0000000000000000000000000000000000000004",
			"0000000000000000000000000000000000000005",
			"0000000000000000000000000000000000000006",
			"0000000000000000000000000000000000000007",
			"0000000000000000000000000000000000000008",
		}, " ") + `"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing summary field %q in %q", want, line)
		}
	}
	for _, misleading := range []string{"missing_blob_count=", "more"} {
		if strings.Contains(line, misleading) {
			t.Errorf("misleading observation-derived summary %q in %q", misleading, line)
		}
	}
}
