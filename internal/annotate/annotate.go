package annotate

import (
	"bytes"
	"context"
	"errors"
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
//
// BelowHorizon marks a line whose origin lies at or below the structural
// horizon of the store (draft-fossil-horizon-00 §5.5): the walk reached a
// parent the store does not hold because it lies below the horizon, so the
// line is reported as originating at the horizon, and Version is the
// horizon's check-in rather than the line's origin.
type Line struct {
	Text         string
	Version      VersionInfo
	BelowHorizon bool
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
	// Cache, when set, serves the walk in place of the one Annotate makes
	// for itself, so a caller's longer-lived cache carries over between
	// calls. The caller keeps its entries current.
	Cache *content.Cache
	// Fetch, when set, is asked for the payloads of a version the walk
	// reads below a content horizon and the store does not hold, a version
	// at a time, so that only what the walk reads is fetched
	// (draft-fossil-horizon-00 §4.4). At most FetchLimit names are asked
	// for; past that, or with no Fetch, the walk stops at the horizon.
	Fetch      func(ctx context.Context, names []string) error
	FetchLimit int
}

// Annotate attributes each line of a file to the commit that last changed it.
// It walks the primary parent chain from StartRID, pushing line attributions
// back to the earliest ancestor that contains the same line.
func Annotate(r *repo.Repo, opts Options) ([]Line, error) {
	return AnnotateContext(context.Background(), r, opts)
}

// AnnotateContext is Annotate under ctx, which bounds the walk and any
// demand fetch it makes.
func AnnotateContext(ctx context.Context, r *repo.Repo, opts Options) ([]Line, error) {
	if ctx == nil || r == nil {
		panic("annotate.Annotate: nil context or repo")
	}
	if opts.FetchLimit < 0 {
		panic("annotate.Annotate: negative FetchLimit")
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
	cache := opts.Cache
	if cache == nil {
		cache = content.NewCache(annotateCacheBytes)
	}

	fileContent, err := loadFileAt(ctx, r, opts.StartRID, opts.FilePath, cache)
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

	credit, versions, horizon, err := walkAncestors(ctx, r, opts, fileContent, start, startInfo, cache)
	if err != nil {
		return nil, fmt.Errorf("annotate: %w", err)
	}
	var horizonInfo VersionInfo
	if horizon != 0 {
		if horizonInfo, err = versionInfoFor(r, horizon); err != nil {
			return nil, fmt.Errorf("annotate: version info for the horizon: %w", err)
		}
	}
	result := make([]Line, len(start))
	for i, d := range start {
		switch v := credit[i]; {
		case v >= 0:
			result[i] = Line{Text: d.text, Version: versions[v]}
		case horizon != 0:
			result[i] = Line{Text: d.text, Version: horizonInfo, BelowHorizon: true}
		default:
			// In the oldest version the walk reached.
			result[i] = Line{Text: d.text, Version: versions[len(versions)-1]}
		}
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
//
// horizon is the store's horizon C when the walk stopped at a parent below
// it (draft-fossil-horizon-00 §5.5): one a structural store does not hold,
// or one whose version of the file a content-horizon store does not hold
// and opts.Fetch did not bring. It is 0 otherwise. A line without credit
// then originates at or below the horizon, which the store cannot tell
// apart.
func walkAncestors(
	ctx context.Context, r *repo.Repo, opts Options, startContent []byte, start []dline,
	startInfo VersionInfo, cache *content.Cache,
) (credit []int, versions []VersionInfo, horizon libfossil.FslID, err error) {
	credit = make([]int, len(start))
	for i := range credit {
		credit[i] = -1
	}
	versions = []VersionInfo{startInfo}
	untagged, fetched := len(start), 0
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
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		parentContent, err := loadFileAt(ctx, r, parentRID, opts.FilePath, cache)
		var missing *content.MissingBelowHorizon
		if errors.As(err, &missing) && len(missing.Checkins) == 0 && opts.Fetch != nil &&
			fetched+len(missing.Names) <= opts.FetchLimit {
			if err := opts.Fetch(ctx, missing.Names); err != nil {
				return nil, nil, 0, err
			}
			fetched += len(missing.Names)
			parentContent, err = loadFileAt(ctx, r, parentRID, opts.FilePath, cache)
		}
		if err != nil {
			// The parent lies below the horizon, and what is left may not
			// belong to this version; or the file is new here, and what is
			// left belongs to this version.
			horizon, err = crossedHorizon(ctx, r, parentRID, err)
			return credit, versions, horizon, err
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
	return credit, versions, 0, nil
}

// crossedHorizon is the rid of the store's horizon C when parent, whose
// read failed with readErr, is a check-in below it: one a structural store
// does not hold, or one whose version of the file a content-horizon store
// does not hold. It is 0 otherwise.
func crossedHorizon(
	ctx context.Context, r *repo.Repo, parent libfossil.FslID, readErr error,
) (libfossil.FslID, error) {
	var missing *content.MissingBelowHorizon
	hash := ""
	if errors.As(readErr, &missing) {
		hash = missing.Horizon
	} else {
		h, below, err := content.BelowHorizon(ctx, r.DB(), parent)
		if err != nil || !below {
			return 0, err
		}
		hash = h.Hash()
	}
	var rid libfossil.FslID
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=? AND size>=0", hash).Scan(&rid); err != nil {
		return 0, fmt.Errorf("the horizon %s: %w", hash, err)
	}
	return rid, nil
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
func loadFileAt(
	ctx context.Context, r *repo.Repo, rid libfossil.FslID, filePath string, cache *content.Cache,
) ([]byte, error) {
	if r == nil {
		panic("annotate.loadFileAt: r must not be nil")
	}
	if rid <= 0 {
		panic("annotate.loadFileAt: rid must be positive")
	}
	if filePath == "" {
		panic("annotate.loadFileAt: filePath must not be empty")
	}
	// One name, so ask for one name: LookupFileCached searches the delta's
	// own F cards first (artifact-format-00 8.4).
	data, found, err := manifest.ReadFileAt(ctx, r, rid, filePath, cache)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("file %q not found in checkin %d", filePath, rid)
	}
	return data, nil
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
