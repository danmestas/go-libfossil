package annotate

import (
	"bytes"
	"fmt"
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

	// One cache for the whole walk. Annotate expands the file blob -- and the
	// checkin manifest -- at every revision back along the parent chain, and
	// content_deltify stores each older revision as a delta against a newer
	// one, so those per-revision expansions are all walks of the same few
	// delta chains (one for the file, one for the manifest). Without the
	// cache each revision replays its chain from the root: O(revisions^2) blob
	// reads. With it, LRU keeps the just-visited newer revision hot, so the
	// next older revision truncates its walk one link back -- O(revisions).
	// Blob content is immutable and the repo is only read here, so no entry
	// this loop caches can go stale; the cache is dropped when Annotate returns.
	cache := content.NewCache(annotateCacheBytes)

	fileContent, err := loadFileAt(r, opts.StartRID, opts.FilePath, cache)
	if err != nil {
		return nil, fmt.Errorf("annotate: load file at start: %w", err)
	}
	startInfo, err := versionInfoFor(r, opts.StartRID)
	if err != nil {
		return nil, fmt.Errorf("annotate: version info for start: %w", err)
	}
	start, ok := breakIntoLines(fileContent)
	if !ok {
		return nil, fmt.Errorf(
			"annotate: %s is binary or has a line over %d bytes", opts.FilePath, lengthMask)
	}

	credit, versions := walkAncestors(r, opts, fileContent, start, startInfo, cache)
	result := make([]Line, len(start))
	for i, d := range start {
		v := credit[i]
		if v < 0 {
			v = len(versions) - 1 // in the oldest version the walk reached
		}
		result[i] = Line{Text: d.text, Version: versions[v]}
	}
	return result, nil
}

// annotateCacheBytes bounds the expanded content one Annotate walk keeps live.
// The working set is the handful of delta-chain nodes in flight for the single
// file and its manifests, not the whole repository, so this only has to be
// large enough that the immediately-preceding revision is never evicted before
// the next older one truncates its walk at it. A miss costs throughput, not
// correctness.
//
// It is deliberately left at the figure manifest.crosslinkCacheBytes has since
// been measured down from: this walk follows one file, so the budget is a
// ceiling it never approaches rather than a working-set bound that decides
// throughput, and cutting it would buy nothing.
const annotateCacheBytes = 256 << 20

// walkAncestors walks the primary parent chain from opts.StartRID the way
// fossil's annotate does. Each ancestor version of the file is diffed
// against the starting version, not against the next newer one, and a start
// line the ancestor lacks is credited to the version just newer than it, the
// first time that happens. versions lists the versions walked, newest first;
// credit[i] indexes it, or is -1 for a line every version walked contains.
//
// A run of check-ins that leave the file unchanged counts as its oldest
// check-in, which is where fossil's annotate, reading mlink, sees that
// version of the file appear.
func walkAncestors(
	r *repo.Repo, opts Options, startContent []byte, start []dline,
	startInfo VersionInfo, cache *content.Cache,
) (credit []int, versions []VersionInfo) {
	credit = make([]int, len(start))
	for i := range credit {
		credit[i] = -1
	}
	versions = []VersionInfo{startInfo}
	untagged := len(start)
	currentRID, currentContent := opts.StartRID, startContent
	for steps := 0; untagged > 0; steps++ {
		if opts.Limit > 0 && steps >= opts.Limit {
			break
		}
		if opts.OriginRID > 0 && currentRID == opts.OriginRID {
			break
		}
		parentRID, err := primaryParent(r, currentRID)
		if err != nil || parentRID <= 0 {
			break
		}
		parentContent, err := loadFileAt(r, parentRID, opts.FilePath, cache)
		if err != nil {
			break // the file is new here: what is left belongs to this version
		}
		parentInfo, err := versionInfoFor(r, parentRID)
		if err != nil {
			break
		}
		currentRID = parentRID
		if bytes.Equal(parentContent, currentContent) {
			versions[len(versions)-1] = parentInfo
			continue
		}
		currentContent = parentContent
		if parent, ok := breakIntoLines(parentContent); ok {
			untagged -= creditInsertions(credit, diffAll(parent, start), len(versions)-1)
		}
		versions = append(versions, parentInfo)
	}
	return credit, versions
}

// creditInsertions credits to version v every start line the edit script
// inserts that has no credit yet, and returns how many it credited: fossil's
// annotation_step.
func creditInsertions(credit []int, edit []int, v int) int {
	if v < 0 {
		panic("annotate.creditInsertions: negative version")
	}
	if len(edit)%3 != 0 {
		panic("annotate.creditInsertions: edit script is not triples")
	}
	credited, line := 0, 0
	for i := 0; i < len(edit); i += 3 {
		line += edit[i]
		for j := 0; j < edit[i+2]; j, line = j+1, line+1 {
			if credit[line] < 0 {
				credit[line] = v
				credited++
			}
		}
	}
	if line > len(credit) {
		panic("annotate.creditInsertions: edit script runs past the file")
	}
	return credited
}

// loadFileAt loads the content of a file at a given checkin RID, serving both
// the checkin manifest and the file blob through cache so a full-history walk
// amortizes their overlapping delta chains. A nil cache reads uncached.
func loadFileAt(r *repo.Repo, rid libfossil.FslID, filePath string, cache *content.Cache) ([]byte, error) {
	if r == nil {
		panic("annotate.loadFileAt: r must not be nil")
	}
	if rid <= 0 {
		panic("annotate.loadFileAt: rid must be positive")
	}
	if filePath == "" {
		panic("annotate.loadFileAt: filePath must not be empty")
	}
	files, err := manifest.ListFilesCached(r, rid, cache)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.Name == filePath {
			fileRID, ok := content.AvailableByUUID(r.DB(), f.UUID)
			if !ok {
				return nil, fmt.Errorf("blob %s not found", f.UUID)
			}
			return cache.Expand(r.DB(), fileRID)
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
