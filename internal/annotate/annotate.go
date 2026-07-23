package annotate

import (
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// Line represents an annotated line of a file, with the text and the version
// that last changed it.
type Line struct {
	Text    string
	Version VersionInfo
}

// VersionInfo identifies the commit that last changed a line.
type VersionInfo struct {
	UUID string
	User string
	Date time.Time
}

// Options controls the annotate operation.
type Options struct {
	FilePath  string          // pathname of the file to annotate
	StartRID  libfossil.FslID // checkin to start from (tip)
	Limit     int             // max ancestors to walk (0 = unlimited)
	OriginRID libfossil.FslID // stop at this checkin (0 = none)
}

// Annotate attributes each line of a file to the commit that last changed it.
// It walks the primary parent chain from StartRID, pushing line attributions
// back to the earliest ancestor that contains the same line.
func Annotate(r *repo.Repo, opts Options) ([]Line, error) {
	if r == nil {
		panic("annotate.Annotate: r must not be nil")
	}
	if opts.StartRID <= 0 {
		return nil, fmt.Errorf("annotate: invalid StartRID %d", opts.StartRID)
	}
	if opts.FilePath == "" {
		return nil, fmt.Errorf("annotate: FilePath is required")
	}

	// Load file content at StartRID.
	fileContent, err := loadFileAt(r, opts.StartRID, opts.FilePath)
	if err != nil {
		return nil, fmt.Errorf("annotate: load file at start: %w", err)
	}

	// Get version info for start commit.
	startInfo, err := versionInfoFor(r, opts.StartRID)
	if err != nil {
		return nil, fmt.Errorf("annotate: version info for start: %w", err)
	}

	// Split into lines and attribute all to start.
	lines := splitLines(string(fileContent))
	result := make([]Line, len(lines))
	for i, l := range lines {
		result[i] = Line{Text: l, Version: startInfo}
	}

	if len(result) == 0 {
		return result, nil
	}

	walkParentChain(r, opts, lines, result)
	return result, nil
}

// walkParentChain walks the primary parent chain from opts.StartRID, pushing
// line attributions back to the earliest ancestor that contains the same line.
func walkParentChain(r *repo.Repo, opts Options, currentLines []string, result []Line) {
	currentRID := opts.StartRID
	steps := 0

	for {
		// Check limit.
		if opts.Limit > 0 && steps >= opts.Limit {
			break
		}

		// Get primary parent.
		parentRID, err := primaryParent(r, currentRID)
		if err != nil || parentRID <= 0 {
			break
		}

		// Check origin boundary.
		if opts.OriginRID > 0 && currentRID == opts.OriginRID {
			break
		}

		// Load file in parent.
		parentContent, err := loadFileAt(r, parentRID, opts.FilePath)
		if err != nil {
			// File doesn't exist in parent — all remaining lines belong to current.
			break
		}

		parentInfo, err := versionInfoFor(r, parentRID)
		if err != nil {
			break
		}

		parentLines := splitLines(string(parentContent))

		// Compute LCS to find which current lines are unchanged from parent.
		matches := lcsMatch(parentLines, currentLines)
		for curIdx, parIdx := range matches {
			if parIdx >= 0 {
				// This line exists in the parent — push attribution back.
				result[curIdx].Version = parentInfo
			}
		}

		currentRID = parentRID
		currentLines = parentLines
		steps++
	}
}

// loadFileAt loads the content of a file at a given checkin RID.
func loadFileAt(r *repo.Repo, rid libfossil.FslID, filePath string) ([]byte, error) {
	if r == nil {
		panic("annotate.loadFileAt: r must not be nil")
	}
	if rid <= 0 {
		panic("annotate.loadFileAt: rid must be positive")
	}
	if filePath == "" {
		panic("annotate.loadFileAt: filePath must not be empty")
	}
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.Name == filePath {
			fileRID, ok := content.AvailableByUUID(r.DB(), f.UUID)
			if !ok {
				return nil, fmt.Errorf("blob %s not found", f.UUID)
			}
			return content.Expand(r.DB(), fileRID)
		}
	}
	return nil, fmt.Errorf("file %q not found in checkin %d", filePath, rid)
}

// versionInfoFor retrieves commit metadata for a checkin RID.
func versionInfoFor(r *repo.Repo, rid libfossil.FslID) (VersionInfo, error) {
	if r == nil {
		panic("annotate.versionInfoFor: r must not be nil")
	}
	if rid <= 0 {
		panic("annotate.versionInfoFor: rid must be positive")
	}
	var uuid, user string
	var mtimeRaw any
	err := r.DB().QueryRow(
		"SELECT b.uuid, e.user, e.mtime FROM blob b JOIN event e ON e.objid=b.rid WHERE b.rid=?",
		rid,
	).Scan(&uuid, &user, &mtimeRaw)
	if err != nil {
		return VersionInfo{}, fmt.Errorf("version info for rid %d: %w", rid, err)
	}

	t, _ := db.ScanTime(mtimeRaw)

	return VersionInfo{UUID: uuid, User: user, Date: t}, nil
}

// primaryParent returns the primary parent RID of a checkin, or 0 if none.
func primaryParent(r *repo.Repo, rid libfossil.FslID) (libfossil.FslID, error) {
	if r == nil {
		panic("annotate.primaryParent: r must not be nil")
	}
	if rid <= 0 {
		panic("annotate.primaryParent: rid must be positive")
	}
	var pid int64
	err := r.DB().QueryRow("SELECT pid FROM plink WHERE cid=? AND isprim=1", rid).Scan(&pid)
	if err != nil {
		return 0, err
	}
	return libfossil.FslID(pid), nil
}

// splitLines splits text into lines, preserving the trailing newline behavior.
// A trailing newline does NOT produce an extra empty line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// lcsMatch computes a mapping from indices in "cur" to indices in "par"
// using the longest common subsequence. For each index in cur, the returned
// slice contains the matching index in par, or -1 if no match.
func lcsMatch(par, cur []string) []int {
	m, n := len(par), len(cur)
	result := make([]int, n)
	for i := range result {
		result[i] = -1
	}

	if m == 0 || n == 0 {
		return result
	}

	// Build LCS table.
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if par[i-1] == cur[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack to find matching pairs.
	i, j := m, n
	for i > 0 && j > 0 {
		if par[i-1] == cur[j-1] {
			result[j-1] = i - 1
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			i--
		} else {
			j--
		}
	}

	return result
}
