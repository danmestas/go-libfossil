package manifest

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/content"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/repo"
)

// incrementalHistory commits nRev revisions of nFile files into r, editing
// roughly one line in twenty on each revision. This is the corpus shape the
// deltification numbers in issue #71 were measured against: enough shared
// text between adjacent versions that a delta should be a small fraction of
// the whole, and enough revisions that chain depth becomes observable.
func incrementalHistory(t *testing.T, r *repo.Repo, nFile, nRev, nLine int) {
	t.Helper()

	bodies := make([][]string, nFile)
	for f := range bodies {
		lines := make([]string, nLine)
		for i := range lines {
			lines[i] = fmt.Sprintf("file %d line %04d: the quick brown fox jumps over the lazy dog", f, i)
		}
		bodies[f] = lines
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var parent libfossil.FslID
	for rev := 0; rev < nRev; rev++ {
		files := make([]File, nFile)
		for f := range bodies {
			if rev > 0 {
				for i := rev % 20; i < nLine; i += 20 {
					bodies[f][i] = fmt.Sprintf("file %d line %04d: revision %d edited this line", f, i, rev)
				}
			}
			body := ""
			for _, l := range bodies[f] {
				body += l + "\n"
			}
			files[f] = File{Name: fmt.Sprintf("src/file%d.txt", f), Content: []byte(body)}
		}
		rid, _, err := Checkin(r, CheckinOpts{
			Files:   files,
			Comment: fmt.Sprintf("revision %d", rev),
			User:    "testuser",
			Parent:  parent,
			Time:    base.Add(time.Duration(rev) * time.Hour),
		})
		if err != nil {
			t.Fatalf("Checkin(rev %d): %v", rev, err)
		}
		parent = rid
	}
}

// storageStats reports how the repository actually stored what it was given.
// Content bytes come from sum(length(content)) rather than the SQLite file
// size: without VACUUM, pages freed by rewriting a blob into delta form stay
// in the file, so file size does not move when deltification works.
type storageStats struct {
	artifacts    int
	deltaEncoded int
	contentBytes int64
	depthP50     int
	depthMax     int
}

func collectStorageStats(t *testing.T, r *repo.Repo) storageStats {
	t.Helper()
	var s storageStats

	if err := r.DB().QueryRow(
		"SELECT count(*), coalesce(sum(length(content)), 0) FROM blob WHERE size >= 0",
	).Scan(&s.artifacts, &s.contentBytes); err != nil {
		t.Fatalf("blob totals: %v", err)
	}

	src := map[libfossil.FslID]libfossil.FslID{}
	rows, err := r.DB().Query("SELECT rid, srcid FROM delta")
	if err != nil {
		t.Fatalf("delta scan: %v", err)
	}
	for rows.Next() {
		var rid, srcid libfossil.FslID
		if err := rows.Scan(&rid, &srcid); err != nil {
			t.Fatalf("delta row: %v", err)
		}
		src[rid] = srcid
	}
	rows.Close()
	s.deltaEncoded = len(src)

	depths := make([]int, 0, s.artifacts)
	ridRows, err := r.DB().Query("SELECT rid FROM blob WHERE size >= 0")
	if err != nil {
		t.Fatalf("rid scan: %v", err)
	}
	for ridRows.Next() {
		var rid libfossil.FslID
		if err := ridRows.Scan(&rid); err != nil {
			t.Fatalf("rid row: %v", err)
		}
		d := 0
		for cur := rid; ; d++ {
			next, ok := src[cur]
			if !ok {
				break
			}
			if d > s.artifacts {
				t.Fatalf("delta chain from rid=%d does not terminate", rid)
			}
			cur = next
		}
		depths = append(depths, d)
	}
	ridRows.Close()

	sort.Ints(depths)
	if len(depths) > 0 {
		s.depthP50 = depths[len(depths)/2]
		s.depthMax = depths[len(depths)-1]
	}
	return s
}

func TestCheckinDeltifiesIncrementalHistory(t *testing.T) {
	r := setupTestRepo(t)
	incrementalHistory(t, r, 4, 12, 400)

	s := collectStorageStats(t, r)
	rate := float64(s.deltaEncoded) / float64(s.artifacts)
	t.Logf("artifacts=%d delta-encoded=%d (%.0f%%) p50depth=%d maxdepth=%d contentBytes=%d",
		s.artifacts, s.deltaEncoded, rate*100, s.depthP50, s.depthMax, s.contentBytes)

	if s.deltaEncoded == 0 {
		t.Fatalf("delta table is empty: nothing on the commit path deltified")
	}
	// Canonical fossil reaches 78% on this corpus shape. Demand a
	// substantial majority rather than pinning an exact rate, since our
	// artifact mix (no baseline manifests) is not identical.
	if rate < 0.60 {
		t.Errorf("delta encoding rate %.0f%% < 60%%", rate*100)
	}
	// This corpus stored 58,973 content bytes with every artifact whole and
	// 23,994 once deltified. The bound sits between the two so a regression
	// that quietly stops deltifying fails here rather than passing on the
	// encoding-rate check alone.
	if s.contentBytes > 35000 {
		t.Errorf("content bytes %d exceeds 35000; deltification is not paying off", s.contentBytes)
	}
	// Backward deltification makes each artifact's depth its age in
	// revisions, so depth must stay on the order of the history length.
	if s.depthMax > 50 {
		t.Errorf("max chain depth %d is larger than the history justifies", s.depthMax)
	}
}

// TestCheckinDeltifiesParentManifest covers the check-in manifest itself,
// which the incremental-history corpus above does not exercise: when every
// file changes on every revision, every F-card hash changes too, so adjacent
// manifests are genuinely dissimilar and the 75% rule declines them. Touching
// one file per revision leaves the other F-cards identical, which is the
// common real-world shape and where manifest deltification pays.
func TestCheckinDeltifiesParentManifest(t *testing.T) {
	r := setupTestRepo(t)

	// Long file names make the manifest big enough that the unchanged
	// F-cards dominate, the same way a real project's manifest does.
	names := []string{
		"src/internal/storage/engine/compaction_scheduler.go",
		"src/internal/storage/engine/write_ahead_log_reader.go",
		"src/internal/storage/engine/manifest_version_edit.go",
		"src/internal/storage/engine/block_cache_eviction.go",
	}
	bodies := make([][]byte, len(names))
	for i := range bodies {
		bodies[i] = []byte(strings.Repeat(fmt.Sprintf("package engine // file %d\n", i), 60))
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var parent libfossil.FslID
	manifestRids := make([]libfossil.FslID, 0, 12)
	for rev := 0; rev < 12; rev++ {
		touched := rev % len(names)
		if rev > 0 {
			bodies[touched] = append(bodies[touched],
				[]byte(fmt.Sprintf("// revision %d touched this file\n", rev))...)
		}
		files := make([]File, len(names))
		for i := range names {
			files[i] = File{Name: names[i], Content: bodies[i]}
		}
		rid, _, err := Checkin(r, CheckinOpts{
			Files:   files,
			Comment: "revision",
			User:    "testuser",
			Parent:  parent,
			Time:    base.Add(time.Duration(rev) * time.Hour),
		})
		if err != nil {
			t.Fatalf("Checkin(rev %d): %v", rev, err)
		}
		manifestRids = append(manifestRids, rid)
		parent = rid
	}

	// Every manifest but the tip should now be a delta against its child.
	deltified := 0
	for i, rid := range manifestRids {
		var srcid libfossil.FslID
		err := r.DB().QueryRow("SELECT srcid FROM delta WHERE rid=?", rid).Scan(&srcid)
		if err != nil {
			continue
		}
		deltified++
		if want := manifestRids[i+1]; srcid != want {
			t.Errorf("manifest %d deltified against rid %d, want its child %d", i, srcid, want)
		}
	}
	if deltified == 0 {
		t.Fatal("no check-in manifest was deltified: the manifest call site is not wired up")
	}
	t.Logf("deltified %d of %d check-in manifests", deltified, len(manifestRids))

	// The tip manifest must stay whole so reading the newest check-in costs
	// one blob load.
	tip := manifestRids[len(manifestRids)-1]
	var srcid libfossil.FslID
	if err := r.DB().QueryRow("SELECT srcid FROM delta WHERE rid=?", tip).Scan(&srcid); err == nil {
		t.Errorf("tip manifest rid=%d was stored as a delta against %d", tip, srcid)
	}
}

// TestDeltifiedArtifactsAllExpand is the invariant deltification must not
// break: every stored artifact, delta-encoded or not, still expands to
// content matching its declared UUID. content.Expand verifies that hash
// internally, so a successful call on every rid is the assertion.
func TestDeltifiedArtifactsAllExpand(t *testing.T) {
	r := setupTestRepo(t)
	incrementalHistory(t, r, 3, 8, 200)

	rows, err := r.DB().Query("SELECT rid FROM blob WHERE size >= 0 ORDER BY rid")
	if err != nil {
		t.Fatalf("rid scan: %v", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			t.Fatalf("rid row: %v", err)
		}
		if _, err := content.Expand(r.DB(), rid); err != nil {
			t.Errorf("content.Expand(rid=%d): %v", rid, err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no artifacts to expand")
	}
	if s := collectStorageStats(t, r); s.deltaEncoded == 0 {
		t.Fatal("no artifacts were delta-encoded; the expansion check proved nothing")
	}
}
